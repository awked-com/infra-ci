// Bootstrap authenticates the complete worker archive before compiling any code.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/awked-com/infra/ci/worker"
)

const limit = 16 * 1024 * 1024

var files = []string{
	"go.mod",
	"go.sum",
	"ci/worker/actions.go",
	"ci/worker/cache.go",
	"ci/worker/cleanup.go",
	"ci/worker/planner.go",
	"ci/worker/upstream.go",
	"ci/worker/runtime.go",
	"ci/worker/runner.go",
	"ci/worker/source.go",
	"ci/worker/cmd/infra-ci-worker/main.go",
}

// Retained requests from before direct worker execution include these files.
// Remove this allowance once their worker bundles have left retained CI state.
var legacyRuntimeFiles = []string{"ci/worker/flake.nix", "ci/worker/flake.lock"}

func download(endpoint string, headers map[string]string) ([]byte, error) {
	client := &http.Client{
		Timeout:       120 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for range 6 {
		u, e := url.Parse(endpoint)
		if e != nil {
			return nil, e
		}
		if u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") || !(u.Hostname() == "ghcr.io" || strings.HasSuffix(u.Hostname(), ".githubusercontent.com")) {
			return nil, errors.New("invalid download endpoint")
		}

		req, e := http.NewRequest("GET", endpoint, nil)
		if e != nil {
			return nil, e
		}

		req.Header.Set("User-Agent", "https://github.com/awked-com/infra-ci")
		req.Header.Set("Accept-Encoding", "identity")
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		r, e := client.Do(req)
		if e != nil {
			return nil, e
		}

		if r.StatusCode == 301 || r.StatusCode == 302 || r.StatusCode == 303 || r.StatusCode == 307 || r.StatusCode == 308 {
			location, e := r.Location()
			r.Body.Close()
			if e != nil {
				return nil, e
			}

			endpoint = location.String()
			headers = nil
			continue
		}

		b, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
		r.Body.Close()
		if e != nil {
			return nil, e
		}
		if len(b) > limit {
			return nil, errors.New("worker bundle exceeds limit")
		}
		if r.StatusCode != 200 {
			return nil, fmt.Errorf("download failed (HTTP %d)", r.StatusCode)
		}

		return b, nil
	}

	return nil, errors.New("too many download redirects")
}

func unpack(ciphertext []byte, digest, identity, destination string) error {
	sum := sha256.Sum256(ciphertext)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return errors.New("worker digest mismatch")
	}
	if len(identity) > 4096 {
		return errors.New("invalid bootstrap identity size")
	}

	identities, e := age.ParseIdentities(strings.NewReader(identity))
	if e != nil {
		return errors.New("invalid bootstrap identity")
	}
	reader, e := age.Decrypt(bytes.NewReader(ciphertext), identities...)
	if e != nil {
		return e
	}
	// Authenticate the final chunk before extracting any source code.
	plaintext, e := io.ReadAll(reader)
	if e != nil {
		return e
	}

	gz, e := gzip.NewReader(bytes.NewReader(plaintext))
	if e != nil {
		return e
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	allowed := map[string]bool{}
	directories := map[string]bool{}
	for _, f := range files {
		allowed[f] = true
		for p := filepath.Dir(f); p != "."; p = filepath.Dir(p) {
			directories[p] = true
		}
	}
	for _, f := range legacyRuntimeFiles {
		allowed[f] = true
	}

	type entry struct {
		name string
		data []byte
	}
	entries := []entry{}
	seen := map[string]bool{}
	var size int64
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		// Git archives include a global PAX header containing the source commit.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if h.Typeflag == tar.TypeDir && directories[strings.TrimSuffix(h.Name, "/")] {
			continue
		}
		if h.Typeflag != tar.TypeReg || !allowed[h.Name] || seen[h.Name] {
			return errors.New("unsafe worker entry")
		}
		if h.Size > limit-size {
			return errors.New("expanded worker bundle exceeds limit")
		}

		size += h.Size
		b, e := io.ReadAll(tr)
		if e != nil {
			return e
		}

		seen[h.Name] = true
		entries = append(entries, entry{h.Name, b})
	}

	for _, f := range files {
		if !seen[f] {
			return errors.New("invalid worker file set")
		}
	}
	if seen[legacyRuntimeFiles[0]] != seen[legacyRuntimeFiles[1]] {
		return errors.New("invalid worker file set")
	}

	for _, entry := range entries {
		p := filepath.Join(destination, entry.name)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}

		if e = os.WriteFile(p, entry.data, 0600); e != nil {
			return e
		}
	}

	return nil
}

func maintenanceWorker(storage worker.Storage, repository, request string, identity any) (string, string, error) {
	reference := "nixos-cache-latest"
	kind := "commit"
	if request != "" {
		if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(request) {
			return "", "", errors.New("invalid maintenance request")
		}
		reference, kind = "ci-input-"+request, "request"
	}
	snapshot, err := worker.LoadSnapshot(storage, repository, reference, identity, true)
	if err != nil {
		return "", "", err
	}
	resolved, _ := snapshot.Metadata["request"].(string)
	bundle := snapshot.Inputs["worker.tar.gz"]
	if snapshot.Metadata["kind"] != kind || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(resolved) || (request != "" && request != resolved) {
		return "", "", errors.New("invalid maintenance snapshot")
	}
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(bundle.Digest) || bundle.Digest != bundle.Blob.Digest || bundle.Offset != 0 || bundle.Size != bundle.Blob.Size || bundle.Size > limit {
		return "", "", errors.New("invalid maintenance worker bundle")
	}
	return resolved, bundle.Digest, nil
}

func mainRun() error {
	syscall.Umask(0077)
	os.Unsetenv("RUNNER_TRACKING_ID")
	var config struct {
		Repository string `json:"repository"`
	}
	if e := json.Unmarshal([]byte(os.Getenv("CI_STORAGE")), &config); e != nil {
		return e
	}

	if !regexp.MustCompile(`^ghcr\.io/[a-z0-9-]+/[a-z0-9][a-z0-9._-]*$`).MatchString(config.Repository) {
		return errors.New("invalid repository")
	}

	digest := os.Getenv("INPUT_WORKER")
	if os.Getenv("INPUT_MODE") == "maintenance" {
		storage := worker.NewRegistry(worker.RegistryCredential(os.Getenv("REGISTRY_USER"), os.Getenv("REGISTRY_TOKEN")))
		defer storage.Close()
		request, resolved, err := maintenanceWorker(storage, config.Repository, os.Getenv("INPUT_REQUEST"), worker.Secret{Data: []byte(os.Getenv("CI_IDENTITY"))})
		if err != nil {
			return err
		}
		digest = resolved
		os.Setenv("INPUT_REQUEST", request)
		os.Setenv("INPUT_WORKER", digest)
	}
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(digest) {
		return errors.New("invalid worker digest")
	}

	pkg := strings.TrimPrefix(config.Repository, "ghcr.io/")
	auth := base64.StdEncoding.EncodeToString([]byte(os.Getenv("REGISTRY_USER") + ":" + os.Getenv("REGISTRY_TOKEN")))
	query := url.Values{
		"service": {"ghcr.io"},
		"scope":   {"repository:" + pkg + ":pull"},
	}
	b, e := download("https://ghcr.io/token?"+query.Encode(), map[string]string{"Authorization": "Basic " + auth})
	if e != nil {
		return e
	}

	var token struct {
		Token string `json:"token"`
	}
	if e = json.Unmarshal(b, &token); e != nil {
		return e
	}

	ciphertext, e := download("https://ghcr.io/v2/"+pkg+"/blobs/"+digest, map[string]string{"Authorization": "Bearer " + token.Token})
	if e != nil {
		return e
	}

	directory, e := os.MkdirTemp("", "worker-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(directory)

	if e = unpack(ciphertext, digest, os.Getenv("CI_IDENTITY"), directory); e != nil {
		return e
	}

	cmd := exec.Command("go", "run", "./ci/worker/cmd/infra-ci-worker")
	cmd.Dir = directory
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func main() {
	if e := mainRun(); e != nil {
		fmt.Fprintln(os.Stderr, "Worker setup failed.")
		os.Exit(1)
	}
}
