package worker

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const SourceRevisionAnnotation = "org.opencontainers.image.revision"

var sourceTagPattern = regexp.MustCompile(`^ci-source-(latest|[a-f0-9]{40}|[a-f0-9]{64})$`)
var revisionRE = regexp.MustCompile(`^([a-f0-9]{40}|[a-f0-9]{64})$`)

func SourceTag(revision string) (string, error) {
	if !revisionRE.MatchString(revision) {
		return "", errors.New("use a full source commit hash")
	}
	return "ci-source-" + revision, nil
}

func SourceReference(value string) (string, error) {
	if value == "latest" {
		return "ci-source-latest", nil
	}
	if revisionRE.MatchString(value) {
		return SourceTag(value)
	}
	if !digestPattern.MatchString(value) && !sourceTagPattern.MatchString(value) {
		return "", errors.New("use a full source commit hash, manifest digest, or latest")
	}

	return value, nil
}

func SourceRevision(manifest Manifest, reference string) (string, error) {
	revision := manifest.Annotations[SourceRevisionAnnotation]
	tag, e := SourceTag(revision)
	if e != nil {
		return "", errors.New("source manifest has no valid commit hash; run just ci source upload")
	}
	if strings.HasPrefix(reference, "ci-source-") && reference != "ci-source-latest" && reference != tag {
		return "", errors.New("source revision does not match its tag")
	}
	return revision, nil
}

func LoadSource(storage Storage, repository, version string, identity any) (*Snapshot, error) {
	ref, e := SourceReference(version)
	if e != nil {
		return nil, e
	}

	s, e := LoadSnapshot(storage, repository, ref, identity, true)
	if e != nil {
		return nil, e
	}

	revision, e := SourceRevision(s.Manifest, ref)
	if e != nil {
		return nil, e
	}
	_, ok := s.Inputs["source.tar.gz"]
	if s.Metadata["kind"] != "source" || s.Metadata["revision"] != revision || !ok || len(s.Inputs) != 1 || len(s.Files) != 0 || len(s.Narinfos) != 0 || len(s.Upstream) != 0 {
		return nil, errors.New("invalid source snapshot")
	}

	return s, nil
}

// ExtractArchive rejects links whose resolved targets leave the extraction root.
func ExtractArchive(input io.Reader, destination string, allowlist map[string]bool, limit int64) error {
	gz, e := gzip.NewReader(input)
	if e != nil {
		return e
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var expanded int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		// Git archives carry their commit in a global PAX header. It is metadata, not a file.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}

		name := strings.TrimSuffix(h.Name, "/")
		clean := filepath.Clean(name)
		if filepath.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return errors.New("unsafe archive entry")
		}

		path := filepath.Join(destination, clean)
		if e = secureParents(destination, filepath.Dir(path)); e != nil {
			return e
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(path, 0700); e != nil {
				return e
			}
		case tar.TypeReg, tar.TypeRegA:
			if allowlist != nil && (!allowlist[h.Name] || seen[h.Name]) {
				return errors.New("invalid worker file set")
			}

			seen[h.Name] = true
			if limit > 0 && h.Size > limit-expanded {
				return errors.New("expanded worker bundle exceeds limit")
			}

			expanded += h.Size
			if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				return e
			}

			mode := os.FileMode(h.Mode) & 0755
			if mode&0100 == 0 {
				mode &^= 0111
			}

			mode |= 0600
			f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if e != nil {
				return e
			}

			_, e = io.Copy(f, tr)
			ce := f.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
		case tar.TypeSymlink:
			if allowlist != nil {
				return errors.New("unsafe worker entry")
			}

			target := h.Linkname
			if filepath.IsAbs(target) {
				return errors.New("unsafe archive symlink")
			}

			resolved := filepath.Clean(filepath.Join(filepath.Dir(clean), target))
			if resolved == ".." || strings.HasPrefix(resolved, "../") {
				return errors.New("unsafe archive symlink")
			}

			if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				return e
			}

			if e = os.Symlink(target, path); e != nil {
				return e
			}
		case tar.TypeLink:
			if allowlist != nil {
				return errors.New("unsafe worker entry")
			}

			target := filepath.Clean(h.Linkname)
			if filepath.IsAbs(target) || target == ".." || strings.HasPrefix(target, "../") {
				return errors.New("unsafe archive hardlink")
			}

			if e = secureParents(destination, filepath.Dir(filepath.Join(destination, target))); e != nil {
				return e
			}

			info, e := os.Lstat(filepath.Join(destination, target))
			if e != nil || !info.Mode().IsRegular() {
				return errors.New("invalid archive hardlink")
			}

			if e = os.Link(filepath.Join(destination, target), path); e != nil {
				return e
			}
		default:
			return errors.New("unsafe archive entry")
		}
	}

	if allowlist != nil && len(seen) != len(allowlist) {
		return errors.New("invalid worker file set")
	}

	// Tar can end before gzip checks its trailer and the source authenticates EOF.
	_, e = io.Copy(io.Discard, gz)
	return e
}

func secureParents(root, parent string) error {
	rel, e := filepath.Rel(root, parent)
	if e != nil {
		return e
	}

	current := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "." || part == "" {
			continue
		}

		current = filepath.Join(current, part)
		info, e := os.Lstat(current)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("unsafe archive parent")
		}
	}

	return nil
}

func ExtractSource(s *Snapshot, destination string, identity any) error {
	stream, e := s.Read("source.tar.gz", identity, true)
	if e != nil {
		return e
	}
	defer stream.Close()

	if e = os.Mkdir(destination, 0700); e != nil {
		return e
	}

	e = ExtractArchive(stream, destination, nil, 0)
	if e != nil {
		os.RemoveAll(destination)
	}

	return e
}

func RegistryCredential(user, token string) Secret {
	b, _ := json.Marshal(map[string]any{
		"auths": map[string]any{
			"ghcr.io": map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + token))},
		},
	})
	return Secret{Data: b}
}

func takeEnv(name string) string {
	v := os.Getenv(name)
	os.Unsetenv(name)
	return v
}

func SourceMain() error {
	identity := Secret{Data: []byte(takeEnv("CI_IDENTITY"))}
	credential := RegistryCredential(os.Getenv("REGISTRY_USER"), takeEnv("REGISTRY_TOKEN"))
	storage := NewRegistry(credential)
	defer storage.Close()

	s, e := LoadSource(storage, os.Getenv("SOURCE_REPOSITORY"), os.Getenv("SOURCE_REF"), identity)
	if e != nil {
		return e
	}

	temporary, e := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "infra-ci-source-")
	if e != nil {
		return e
	}

	destination := filepath.Join(temporary, "source")
	if e = ExtractSource(s, destination, identity); e != nil {
		os.RemoveAll(temporary)
		return e
	}

	f, e := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()

	_, e = fmt.Fprintf(f, "path=%s\ndigest=%s\nrevision=%s\n", destination, s.Digest, s.Metadata["revision"])
	return e
}
