package worker

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
)

const (
	UploadChunkSize   = 4 * 1024 * 1024
	CatalogTitle      = "org.opencontainers.image.title"
	CatalogLimit      = 256 * 1024 * 1024
	SnapshotFormat    = "infra-ci-snapshot"
	manifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	blobLimit         = int64(10 * 1024 * 1024 * 1024)
	metadataLimit     = 16 * 1024 * 1024
)

var ErrObjectNotFound = errors.New("object not found")

// Secret keeps credentials in memory and redacts them when formatted.
type Secret struct{ Data []byte }

func (Secret) String() string { return "[secret]" }

func (Secret) GoString() string { return "[secret]" }

func CredentialBytes(value any) ([]byte, error) {
	switch v := value.(type) {
	case Secret:
		return v.Data, nil
	case *Secret:
		return v.Data, nil
	case string:
		return os.ReadFile(v)
	default:
		return nil, fmt.Errorf("unsupported credential type %T", value)
	}
}

func WithCredential(value any, callback func(string, []*os.File) error) error {
	if path, ok := value.(string); ok {
		return callback(path, nil)
	}

	data, err := CredentialBytes(value)
	if err != nil {
		return err
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		_, err := writer.Write(data)
		writer.Close()
		done <- err
	}()
	err = callback("/dev/fd/3", []*os.File{reader})
	reader.Close()
	feedErr := <-done
	if err != nil {
		return err
	}

	return feedErr
}

var repositoryPattern = regexp.MustCompile(`^ghcr\.io/([a-z0-9-]+)/([a-z0-9][a-z0-9._-]*)$`)
var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var referencePattern = regexp.MustCompile(`^(?:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}|sha256:[a-f0-9]{64})$`)

func RepositoryParts(repository string) (string, string, error) {
	m := repositoryPattern.FindStringSubmatch(repository)
	if m == nil {
		return "", "", errors.New("expected ghcr.io/OWNER/PACKAGE")
	}

	return m[1], m[2], nil
}

func ResultTag(run, system string, attempt int) string {
	return fmt.Sprintf("nixos-cache-result-%s-%d-%s", run, attempt, system)
}

func ManifestPath(reference string) (string, error) {
	if !referencePattern.MatchString(reference) {
		return "", errors.New("invalid object tag")
	}

	return "/manifests/" + reference, nil
}

func contentDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type Descriptor struct {
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	MediaType   string            `json:"mediaType,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func (d *Descriptor) UnmarshalJSON(data []byte) error {
	var value struct {
		Digest      string            `json:"digest"`
		Size        *int64            `json:"size"`
		MediaType   string            `json:"mediaType"`
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}

	if value.Size == nil {
		return errors.New("missing layer size")
	}

	*d = Descriptor{
		Digest:      value.Digest,
		Size:        *value.Size,
		MediaType:   value.MediaType,
		Annotations: value.Annotations,
	}
	return nil
}

type Manifest struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Config        Descriptor        `json:"config"`
	Layers        []Descriptor      `json:"layers"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

type Storage interface {
	GetManifest(repository, reference string) (Manifest, string, error)
	PutManifest(repository, tag string, manifest Manifest) (string, error)
	UploadBlob(repository string, source io.Reader, encrypted bool) (Descriptor, error)
	Blob(repository string, descriptor Descriptor) (io.ReadCloser, error)
	BlobRange(repository string, file SnapshotFile) (io.ReadCloser, error)
}

type UploadError struct {
	Status     int
	Operation  string
	RetryAfter time.Duration
}

func (e *UploadError) Error() string {
	return fmt.Sprintf("GHCR %s failed (HTTP %d).", e.Operation, e.Status)
}

func RetryAfterSeconds(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}

	if regexp.MustCompile(`^[0-9]+$`).MatchString(value) {
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds > int64((1<<63-1)/int64(time.Second)) {
			return 0, false
		}

		return time.Duration(seconds) * time.Second, true
	}

	t, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}

	return max(0, t.Sub(now)), true
}

type registryToken struct {
	Value    string
	Deadline time.Time
}

type Registry struct {
	AuthFile                            any
	HTTP, UploadHTTP                    *http.Client
	tokensMu, writeTokensMu, cooldownMu sync.Mutex
	tokens, writeTokens                 map[string]registryToken
	cooldownUntil                       time.Time
	now                                 func() time.Time
	sleep                               func(time.Duration)
	jitter                              func() time.Duration
	uploadRetries                       int
}

type deadlineConn struct{ net.Conn }

func (c deadlineConn) Read(p []byte) (int, error) {
	c.SetReadDeadline(time.Now().Add(120 * time.Second))
	return c.Conn.Read(p)
}

func (c deadlineConn) Write(p []byte) (int, error) {
	c.SetWriteDeadline(time.Now().Add(120 * time.Second))
	return c.Conn.Write(p)
}

func NewRegistry(auth any) *Registry {
	dialer := net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   16,
		MaxConnsPerHost:       16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		DisableCompression:    true,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}

			return deadlineConn{conn}, nil
		},
	}
	return &Registry{
		AuthFile: auth,
		HTTP: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		// Publishing must not consume the connection slots used by substitution.
		UploadHTTP: &http.Client{
			Transport:     transport.Clone(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		tokens:        map[string]registryToken{},
		writeTokens:   map[string]registryToken{},
		now:           time.Now,
		sleep:         time.Sleep,
		jitter:        func() time.Duration { return time.Duration(rand.Int64N(int64(5 * time.Second))) },
		uploadRetries: 8,
	}
}

func Client(config map[string]any, auth any) (*Registry, error) {
	repository, _ := config["repository"].(string)
	if _, _, err := RepositoryParts(repository); err != nil {
		return nil, err
	}

	return NewRegistry(auth), nil
}

func (r *Registry) Close() {
	r.HTTP.CloseIdleConnections()
	r.UploadHTTP.CloseIdleConnections()
}

func readLimited(reader io.Reader, limit int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, errors.New("response exceeds size limit")
	}

	return b, nil
}

func (r *Registry) basicAuth() (string, error) {
	data, err := CredentialBytes(r.AuthFile)
	if err != nil {
		return "", err
	}

	var config struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err = json.Unmarshal(data, &config); err != nil {
		return "", errors.New("invalid registry authentication configuration")
	}

	auth := config.Auths["ghcr.io"].Auth
	if auth == "" {
		return "", errors.New("missing registry authentication")
	}

	return auth, nil
}

func (r *Registry) response(method, endpoint string, headers http.Header, body []byte, readRetry bool) (*http.Response, error) {
	tries := 1
	client := r.UploadHTTP
	if readRetry {
		tries = 3
		client = r.HTTP
	}

	for i := 0; i < tries; i++ {
		req, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, errors.New("invalid registry endpoint")
		}

		req.Header.Set("User-Agent", "https://github.com/awked-com/infra-ci")
		req.Header.Set("Accept-Encoding", "identity")
		for key, values := range headers {
			req.Header[key] = slices.Clone(values)
		}

		req.GetBody = nil // A lost write response is ambiguous and must never be replayed.
		response, err := client.Do(req)
		if err == nil {
			return response, nil
		}
		if i+1 == tries {
			return nil, fmt.Errorf("registry %s transport failed", method)
		}
	}

	panic("unreachable")
}

func (r *Registry) HTTPRequest(endpoint, method string, headers http.Header, data []byte, expected int) (http.Header, []byte, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host != "ghcr.io" || u.User != nil {
		return nil, nil, errors.New("unexpected upload endpoint")
	}

	response, err := r.response(method, endpoint, headers, data, false)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()

	body, err := readLimited(response.Body, 1024*1024)
	if err != nil {
		return nil, nil, err
	}

	if response.StatusCode != expected {
		operation := "request"
		switch {
		case u.Path == "/token":
			operation = "authentication"
		case strings.Contains(u.Path, "/manifests/"):
			operation = "manifest publication"
		case method == "POST":
			operation = "starting blob upload"
		case method == "PATCH":
			operation = "streaming blob upload"
		case method == "PUT":
			operation = "completing blob upload"
		}

		retry, _ := RetryAfterSeconds(response.Header.Get("Retry-After"), r.now())
		return nil, nil, &UploadError{response.StatusCode, operation, retry}
	}

	return response.Header, body, nil
}

func (r *Registry) retryUpload(operation func() error) error {
	deadline := r.now().Add(600 * time.Second)
	var last error = &UploadError{
		Status:    429,
		Operation: "waiting for registry rate limit",
	}
	for attempt := 0; attempt <= r.uploadRetries; attempt++ {
		for {
			now := r.now()
			r.cooldownMu.Lock()
			delay := r.cooldownUntil.Sub(now)
			r.cooldownMu.Unlock()
			if !now.Before(deadline) || now.Add(max(0, delay)).After(deadline) {
				return last
			}
			if delay <= 0 {
				break
			}

			r.sleep(min(delay, 60*time.Second))
		}

		err := operation()
		if err == nil {
			return nil
		}

		var upload *UploadError
		if !errors.As(err, &upload) || upload.Status != 429 {
			return err
		}

		delay := max(min(60*time.Second, 5*time.Second*time.Duration(1<<attempt))+r.jitter(), upload.RetryAfter)
		r.cooldownMu.Lock()
		if next := r.now().Add(delay); next.After(r.cooldownUntil) {
			r.cooldownUntil = next
		}

		until := r.cooldownUntil
		r.cooldownMu.Unlock()
		if attempt == r.uploadRetries || !until.Before(deadline) {
			return err
		}

		last = err
	}

	return last
}

func (r *Registry) token(repository, rejected string, write bool) (string, error) {
	owner, packageName, err := RepositoryParts(repository)
	if err != nil {
		return "", err
	}

	lock, tokens := &r.tokensMu, r.tokens
	scope := "pull"
	if write {
		if r.AuthFile == nil {
			return "", errors.New("publishing requires GHCR write credentials")
		}

		lock, tokens = &r.writeTokensMu, r.writeTokens
		scope = "pull,push"
	}

	lock.Lock()
	defer lock.Unlock()

	cached := tokens[repository]
	if cached.Value != "" && cached.Value != rejected && r.now().Before(cached.Deadline) {
		return cached.Value, nil
	}

	query := url.Values{
		"service": {"ghcr.io"},
		"scope":   {"repository:" + owner + "/" + packageName + ":" + scope},
	}
	headers := http.Header{}
	if r.AuthFile != nil {
		auth, err := r.basicAuth()
		if err != nil {
			return "", err
		}

		headers.Set("Authorization", "Basic "+auth)
	}

	var data []byte
	if write {
		err = r.retryUpload(func() error {
			var err error
			_, data, err = r.HTTPRequest("https://ghcr.io/token?"+query.Encode(), "GET", headers, nil, 200)
			return err
		})
	} else {
		var response *http.Response
		response, err = r.response("GET", "https://ghcr.io/token?"+query.Encode(), headers, nil, true)
		if err == nil {
			defer response.Body.Close()

			if response.StatusCode != 200 {
				return "", fmt.Errorf("registry token request failed (HTTP %d)", response.StatusCode)
			}

			data, err = readLimited(response.Body, metadataLimit)
		}
	}

	if err != nil {
		return "", err
	}

	var result struct {
		Token     string   `json:"token"`
		ExpiresIn *float64 `json:"expires_in"`
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return "", err
	}

	if result.Token == "" {
		return "", errors.New("registry returned an empty token")
	}

	lifetime := 60.0
	if result.ExpiresIn != nil {
		lifetime = max(1, *result.ExpiresIn)
	}

	tokens[repository] = registryToken{result.Token, r.now().Add(time.Duration(lifetime * 0.9 * float64(time.Second)))}
	return result.Token, nil
}

func (r *Registry) Token(repository, rejected string) (string, error) {
	return r.token(repository, rejected, false)
}

func (r *Registry) WriteToken(repository, rejected string) (string, error) {
	return r.token(repository, rejected, true)
}

func (r *Registry) registryResponse(repository, path string, headers http.Header) (*http.Response, error) {
	token, err := r.Token(repository, "")
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		h := headers.Clone()
		if h == nil {
			h = http.Header{}
		}
		h.Set("Authorization", "Bearer "+token)
		h.Set("Accept", manifestMediaType)
		response, err := r.response("GET", "https://"+strings.Replace(repository, "ghcr.io/", "ghcr.io/v2/", 1)+path, h, nil, true)
		if err != nil {
			return nil, err
		}

		if response.StatusCode != 401 || attempt == 1 {
			if response.StatusCode == 404 {
				_, err = readLimited(response.Body, metadataLimit)
				response.Body.Close()
				if err != nil {
					return nil, err
				}

				return nil, ErrObjectNotFound
			}

			return response, nil
		}

		_, err = readLimited(response.Body, metadataLimit)
		response.Body.Close()
		if err != nil {
			return nil, err
		}

		token, err = r.Token(repository, token)
		if err != nil {
			return nil, err
		}
	}

	panic("unreachable")
}

func (r *Registry) writeRequest(repository, endpoint, method string, headers http.Header, data []byte, expected int) (http.Header, []byte, error) {
	var resultHeaders http.Header
	var result []byte
	err := r.retryUpload(func() error {
		token, err := r.WriteToken(repository, "")
		if err != nil {
			return err
		}

		for attempt := 0; attempt < 2; attempt++ {
			h := headers.Clone()
			if h == nil {
				h = http.Header{}
			}

			h.Set("Authorization", "Bearer "+token)
			resultHeaders, result, err = r.HTTPRequest(endpoint, method, h, data, expected)
			var upload *UploadError
			if !errors.As(err, &upload) || upload.Status != 401 || attempt == 1 {
				return err
			}

			token, err = r.WriteToken(repository, token)
			if err != nil {
				return err
			}
		}

		return err
	})
	return resultHeaders, result, err
}

func uploadLocation(current string, headers http.Header) (string, error) {
	base, err := url.Parse(current)
	if err != nil {
		return "", err
	}

	location := headers.Get("Location")
	if location == "" {
		return "", errors.New("missing registry upload endpoint")
	}

	target, err := base.Parse(location)
	if err != nil || target.Scheme != "https" || target.Host != "ghcr.io" || target.User != nil {
		return "", errors.New("unexpected registry upload endpoint")
	}

	return target.String(), nil
}

func (r *Registry) UploadBlob(repository string, source io.Reader, encrypted bool) (Descriptor, error) {
	var empty Descriptor
	owner, packageName, err := RepositoryParts(repository)
	if err != nil {
		return empty, err
	}

	first := make([]byte, UploadChunkSize)
	n, readErr := io.ReadFull(source, first)
	first = first[:n]
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return empty, readErr
	}

	var next []byte
	if readErr == nil {
		buffer := make([]byte, UploadChunkSize)
		n, err := io.ReadFull(source, buffer)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return empty, err
		}

		next = buffer[:n]
	}

	if encrypted && !bytes.HasPrefix(first, []byte("age-encryption.org/v1\n")) {
		return empty, errors.New("refusing unencrypted upload")
	}
	if !encrypted && (!bytes.Equal(first, []byte("{}")) || len(next) > 0) {
		return empty, errors.New("only an empty OCI config may be uploaded as plaintext")
	}

	base := "https://ghcr.io/v2/" + owner + "/" + packageName
	headers, _, err := r.writeRequest(repository, base+"/blobs/uploads/", "POST", nil, nil, 202)
	if err != nil {
		return empty, err
	}

	location, err := uploadLocation(base+"/", headers)
	if err != nil {
		return empty, err
	}

	if len(next) == 0 {
		digest := contentDigest(first)
		u, _ := url.Parse(location)
		query := u.Query()
		query.Set("digest", digest)
		u.RawQuery = query.Encode()
		_, _, err = r.writeRequest(repository, u.String(), "PUT", http.Header{"Content-Type": {"application/octet-stream"}}, first, 201)
		return Descriptor{
			Digest:    digest,
			Size:      int64(len(first)),
			MediaType: "application/octet-stream",
		}, err
	}

	checksum := sha256.New()
	var size int64

	send := func(chunk []byte) error {
		if len(chunk) == 0 {
			return nil
		}
		if size+int64(len(chunk)) >= blobLimit {
			return errors.New("object exceeds GHCR layer size limit")
		}

		h, _, err := r.writeRequest(repository, location, "PATCH", http.Header{
			"Content-Type":  {"application/octet-stream"},
			"Content-Range": {fmt.Sprintf("%d-%d", size, size+int64(len(chunk))-1)},
		}, chunk, 202)
		if err != nil {
			return err
		}

		location, err = uploadLocation(location, h)
		if err != nil {
			return err
		}

		checksum.Write(chunk)
		size += int64(len(chunk))
		return nil
	}

	if err = send(first); err != nil {
		return empty, err
	}

	if err = send(next); err != nil {
		return empty, err
	}

	buffer := make([]byte, UploadChunkSize)
	for {
		n, err := io.ReadFull(source, buffer)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return empty, err
		}

		if e := send(buffer[:n]); e != nil {
			return empty, e
		}

		if err != nil {
			break
		}
	}

	digest := "sha256:" + hex.EncodeToString(checksum.Sum(nil))
	u, _ := url.Parse(location)
	query := u.Query()
	query.Set("digest", digest)
	u.RawQuery = query.Encode()
	_, _, err = r.writeRequest(repository, u.String(), "PUT", nil, nil, 201)
	return Descriptor{
		Digest:    digest,
		Size:      size,
		MediaType: "application/octet-stream",
	}, err
}

func (r *Registry) PutManifest(repository, tag string, manifest Manifest) (string, error) {
	path, err := ManifestPath(tag)
	if err != nil {
		return "", err
	}

	owner, packageName, err := RepositoryParts(repository)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	if len(data) > metadataLimit {
		return "", errors.New("snapshot manifest exceeds size limit")
	}

	_, _, err = r.writeRequest(
		repository,
		"https://ghcr.io/v2/"+owner+"/"+packageName+path,
		"PUT",
		http.Header{"Content-Type": {manifestMediaType}},
		data,
		201,
	)
	if err != nil {
		return "", err
	}

	return contentDigest(data), nil
}

func (r *Registry) GetManifest(repository, reference string) (Manifest, string, error) {
	var manifest Manifest
	path, err := ManifestPath(reference)
	if err != nil {
		return manifest, "", err
	}

	response, err := r.registryResponse(repository, path, nil)
	if err != nil {
		return manifest, "", err
	}
	defer response.Body.Close()

	if response.StatusCode != 200 {
		return manifest, "", fmt.Errorf("registry manifest request failed (HTTP %d)", response.StatusCode)
	}

	body, err := readLimited(response.Body, metadataLimit)
	if err != nil {
		return manifest, "", err
	}

	digest := contentDigest(body)
	if strings.HasPrefix(reference, "sha256:") && reference != digest {
		return manifest, "", errors.New("manifest digest mismatch")
	}

	err = json.Unmarshal(body, &manifest)
	return manifest, digest, err
}

type verifiedBlob struct {
	source     io.ReadCloser
	descriptor Descriptor
	checksum   hash.Hash
	received   int64
	terminal   error
}

func (v *verifiedBlob) Read(p []byte) (int, error) {
	if v.terminal != nil {
		return 0, v.terminal
	}

	n, err := v.source.Read(p)
	v.received += int64(n)
	v.checksum.Write(p[:n])
	if v.received > v.descriptor.Size {
		v.terminal = errors.New("registry blob exceeds declared size")
		return 0, v.terminal
	}

	if err == io.EOF && (v.received != v.descriptor.Size || "sha256:"+hex.EncodeToString(v.checksum.Sum(nil)) != v.descriptor.Digest) {
		err = errors.New("registry blob size or digest mismatch")
	}

	if err != nil {
		v.terminal = err
	}

	return n, err
}

func (v *verifiedBlob) Close() error { return v.source.Close() }

func (r *Registry) Blob(repository string, descriptor Descriptor) (io.ReadCloser, error) {
	return r.BlobRange(repository, wholeFile(descriptor))
}

func (r *Registry) BlobRange(repository string, file SnapshotFile) (io.ReadCloser, error) {
	if err := file.validate(); err != nil {
		return nil, err
	}
	headers := http.Header{}
	expectedStatus := http.StatusOK
	expectedRange := ""
	if file.Size != file.Blob.Size {
		end := file.Offset + file.Size - 1
		headers.Set("Range", fmt.Sprintf("bytes=%d-%d", file.Offset, end))
		expectedStatus = http.StatusPartialContent
		expectedRange = fmt.Sprintf("bytes %d-%d/%d", file.Offset, end, file.Blob.Size)
	}

	path := "/blobs/" + file.Blob.Digest
	endpoint := "https://" + strings.Replace(repository, "ghcr.io/", "ghcr.io/v2/", 1) + path
	response, err := r.registryResponse(repository, path, headers)
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 6; attempt++ {
		if slices.Contains([]int{301, 302, 303, 307, 308}, response.StatusCode) {
			base, _ := url.Parse(endpoint)
			next, e := base.Parse(response.Header.Get("Location"))
			_, readErr := readLimited(response.Body, metadataLimit)
			response.Body.Close()
			if e != nil || next.Scheme != "https" || next.User != nil || (next.Port() != "" && next.Port() != "443") || !(next.Hostname() == "ghcr.io" || strings.HasSuffix(next.Hostname(), ".githubusercontent.com")) {
				return nil, errors.New("unexpected registry download endpoint")
			}
			if readErr != nil {
				return nil, readErr
			}
			if attempt == 5 {
				return nil, errors.New("too many registry download redirects")
			}

			endpoint = next.String()
			response, err = r.response("GET", endpoint, headers, nil, true)
			if err != nil {
				return nil, err
			}

			continue
		}

		if response.StatusCode != expectedStatus {
			response.Body.Close()
			return nil, fmt.Errorf("registry blob request failed (HTTP %d)", response.StatusCode)
		}
		if response.Header.Get("Content-Range") != expectedRange || (response.ContentLength >= 0 && response.ContentLength != file.Size) {
			response.Body.Close()
			return nil, errors.New("registry blob range or length mismatch")
		}
		if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			response.Body.Close()
			return nil, errors.New("unexpected registry blob encoding")
		}

		return &verifiedBlob{
			source:     response.Body,
			descriptor: Descriptor{Digest: file.Digest, Size: file.Size},
			checksum:   sha256.New(),
		}, nil
	}

	panic("unreachable")
}

type processReader struct {
	*io.PipeReader
	cmd    *exec.Cmd
	source io.Closer
	done   chan struct{}
	once   sync.Once
}

func (p *processReader) Close() error {
	p.once.Do(func() {
		p.PipeReader.Close()
		if p.source != nil {
			p.source.Close()
		}

		p.cmd.Process.Kill()
		<-p.done
	})
	return nil
}

func ProcessStream(command []string, source io.Reader, files []*os.File, dir string, stderr io.Writer, environment ...[]string) (io.ReadCloser, error) {
	if len(command) == 0 {
		return nil, errors.New("empty subprocess command")
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin = source
	cmd.ExtraFiles = files
	cmd.Dir = dir
	cmd.Stderr = stderr
	if len(environment) > 0 {
		cmd.Env = environment[0]
	}

	reader, writer := io.Pipe()
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		reader.Close()
		writer.Close()
		return nil, err
	}

	result := &processReader{
		PipeReader: reader,
		cmd:        cmd,
		done:       make(chan struct{}),
	}
	if closer, ok := source.(io.Closer); ok {
		result.source = closer
	}

	go func() {
		err := cmd.Wait()
		if result.source != nil {
			result.source.Close()
		}

		writer.CloseWithError(err)
		close(result.done)
	}()
	return result, nil
}

// transformStream closes its source on completion or cancellation, including a
// subprocess whose output would otherwise remain blocked after an upload fails.
func transformStream(source io.Reader, transform func(io.Writer) error) io.ReadCloser {
	reader, writer := io.Pipe()
	stream := &transformReader{PipeReader: reader, done: make(chan struct{})}
	if closer, ok := source.(io.Closer); ok {
		stream.source = closer
	}
	go func() {
		err := transform(writer)
		stream.closeSource()
		writer.CloseWithError(err)
		close(stream.done)
	}()
	return stream
}

type transformReader struct {
	*io.PipeReader
	source io.Closer
	done   chan struct{}
	once   sync.Once
}

func (s *transformReader) closeSource() {
	s.once.Do(func() {
		if s.source != nil {
			s.source.Close()
		}
	})
}

func (s *transformReader) Close() error {
	s.PipeReader.Close()
	s.closeSource()
	<-s.done
	return nil
}

func IdentityRecipients(credential any) (Secret, error) {
	data, err := CredentialBytes(credential)
	if err != nil {
		return Secret{}, err
	}
	identities, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil {
		return Secret{}, errors.New("invalid age identity")
	}
	var recipients strings.Builder
	for _, identity := range identities {
		switch identity := identity.(type) {
		case *age.X25519Identity:
			fmt.Fprintln(&recipients, identity.Recipient())
		case *age.HybridIdentity:
			fmt.Fprintln(&recipients, identity.Recipient())
		default:
			return Secret{}, errors.New("unsupported age identity")
		}
	}
	if recipients.Len() == 0 {
		return Secret{}, errors.New("empty age identity")
	}
	return Secret{Data: []byte(recipients.String())}, nil
}

func EncryptedStream(source io.Reader, credential any) (io.ReadCloser, error) {
	data, err := CredentialBytes(credential)
	if err != nil {
		return nil, err
	}
	recipients, err := age.ParseRecipients(bytes.NewReader(data))
	if err != nil || len(recipients) == 0 {
		return nil, errors.New("invalid age recipients")
	}
	return transformStream(source, func(output io.Writer) error {
		writer, err := age.Encrypt(output, recipients...)
		if err != nil {
			return err
		}
		if _, err = io.Copy(writer, source); err != nil {
			return err
		}
		return writer.Close()
	}), nil
}

func DecryptBlob(storage Storage, repository string, descriptor Descriptor, identity any) (io.ReadCloser, error) {
	source, err := storage.Blob(repository, descriptor)
	if err != nil {
		return nil, err
	}

	stream, err := decryptedStream(source, identity)
	if err != nil {
		source.Close()
	}
	return stream, err
}

func decryptedStream(source io.Reader, identity any) (io.ReadCloser, error) {
	data, err := CredentialBytes(identity)
	if err != nil {
		return nil, err
	}
	identities, err := age.ParseIdentities(bytes.NewReader(data))
	if err != nil || len(identities) == 0 {
		return nil, errors.New("invalid age identity")
	}
	return transformStream(source, func(output io.Writer) error {
		reader, err := age.Decrypt(source, identities...)
		if err != nil {
			return err
		}
		_, err = io.Copy(output, reader)
		return err
	}), nil
}

func ArtifactTitle(tag string) string {
	for _, item := range [][2]string{
		{"nixos-cache-", "NixOS binary cache"},
		{"ci-source-", "CI source snapshot"},
		{"ci-input-", "CI build inputs"},
	} {
		if strings.HasPrefix(tag, item[0]) {
			return item[1]
		}
	}

	return "CI artifact"
}

func NarinfoKey(path string) string {
	return "cache/" + strings.SplitN(filepath.Base(path), "-", 2)[0] + ".narinfo"
}

var storePathPattern = regexp.MustCompile(`^/nix/store/[0-9abcdfghijklmnpqrsvwxyz]{32}-[A-Za-z0-9+._?=-]+$`)
var recordStorePattern = regexp.MustCompile(`^/nix/store/[0-9abcdfghijklmnpqrsvwxyz]{32}-[^/\\\n]+$`)
var recordRefPattern = regexp.MustCompile(`^[0-9abcdfghijklmnpqrsvwxyz]{32}-[^/\\\n]+$`)
var archivePattern = regexp.MustCompile(`^nar/[a-f0-9]{64}\.nar\.zst$`)

func ValidStorePath(path string) bool { return storePathPattern.MatchString(path) }

func NarinfoFields(value string) (map[string]string, error) {
	if !strings.HasPrefix(value, "StorePath: ") {
		return nil, errors.New("invalid canonical narinfo")
	}

	fields := map[string]string{}
	signed := false
	for _, line := range strings.Split(strings.TrimSuffix(value, "\n"), "\n") {
		key, text, ok := strings.Cut(line, ": ")
		_, exists := fields[key]
		if !ok || (exists && key != "Sig") {
			return nil, errors.New("invalid narinfo")
		}

		if key == "Sig" {
			signed = true
		} else {
			fields[key] = text
		}
	}

	for _, key := range []string{"StorePath", "NarHash", "NarSize", "References", "URL", "Compression"} {
		if _, ok := fields[key]; !ok {
			return nil, errors.New("incomplete narinfo")
		}
	}

	if !recordStorePattern.MatchString(fields["StorePath"]) {
		return nil, errors.New("invalid store path")
	}
	if fields["Compression"] != "zstd" || !archivePattern.MatchString(fields["URL"]) {
		return nil, errors.New("invalid archive URL")
	}

	for _, ref := range strings.Fields(fields["References"]) {
		if !recordRefPattern.MatchString(ref) {
			return nil, errors.New("invalid reference")
		}
	}

	if !signed {
		return nil, errors.New("unsigned cache record")
	}

	return fields, nil
}

type Snapshot struct {
	Storage       Storage
	Repository    string
	Identity      any
	Files, Inputs map[string]SnapshotFile
	Narinfos      map[string]string
	Metadata      map[string]any
	Upstream      map[string][]string
	Manifest      Manifest
	Digest        string
	ManifestBytes int64
	mu            sync.Mutex
}

func NewSnapshot(storage Storage, repository string) *Snapshot {
	return &Snapshot{
		Storage:    storage,
		Repository: repository,
		Files:      map[string]SnapshotFile{},
		Inputs:     map[string]SnapshotFile{},
		Narinfos:   map[string]string{},
		Metadata:   map[string]any{},
		Upstream:   map[string][]string{},
	}
}

// SnapshotFile identifies an independently encrypted record within an OCI blob.
type SnapshotFile struct {
	Blob   Descriptor `json:"blob"`
	Offset int64      `json:"offset"`
	Size   int64      `json:"size"`
	Digest string     `json:"digest"`
}

func wholeFile(blob Descriptor) SnapshotFile {
	return SnapshotFile{Blob: blob, Size: blob.Size, Digest: blob.Digest}
}

func (f *SnapshotFile) UnmarshalJSON(data []byte) error {
	var value struct {
		Blob   *Descriptor `json:"blob"`
		Offset *int64      `json:"offset"`
		Size   *int64      `json:"size"`
		Digest string      `json:"digest"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.Blob == nil || value.Offset == nil || value.Size == nil {
		return errors.New("incomplete snapshot file location")
	}
	*f = SnapshotFile{Blob: *value.Blob, Offset: *value.Offset, Size: *value.Size, Digest: value.Digest}
	return f.validate()
}

func (f SnapshotFile) validate() error {
	if !digestPattern.MatchString(f.Blob.Digest) || !digestPattern.MatchString(f.Digest) {
		return errors.New("invalid snapshot file digest")
	}
	if f.Blob.Size <= 0 || f.Blob.Size >= blobLimit || f.Size <= 0 || f.Size > f.Blob.Size || f.Offset < 0 || f.Offset > f.Blob.Size-f.Size {
		return errors.New("snapshot file range exceeds blob bounds")
	}
	if f.Size == f.Blob.Size && f.Digest != f.Blob.Digest {
		return errors.New("whole file digest differs from blob digest")
	}
	return nil
}

type snapshotCatalog struct {
	Format   string                  `json:"format"`
	Version  int                     `json:"version"`
	Files    map[string]SnapshotFile `json:"files"`
	Metadata map[string]any          `json:"metadata,omitempty"`
	Narinfos map[string]string       `json:"narinfos,omitempty"`
	Upstream map[string][]string     `json:"upstream,omitempty"`
}

func LoadSnapshot(storage Storage, repository, reference string, identity any, inputs bool) (*Snapshot, error) {
	result := NewSnapshot(storage, repository)
	result.Identity = identity
	manifest, digest, err := storage.GetManifest(repository, reference)
	if err != nil {
		return nil, err
	}

	result.Manifest, result.Digest = manifest, digest
	catalogs := map[string]Descriptor{}
	reachable := map[string]Descriptor{}
	for _, layer := range manifest.Layers {
		reachable[layer.Digest] = layer
		if title, ok := layer.Annotations[CatalogTitle]; ok {
			catalogs[title] = layer
		}
	}

	if len(catalogs) != 2 || catalogs["files"].Digest == "" || catalogs["inputs"].Digest == "" {
		return nil, errors.New("invalid snapshot catalogs")
	}

	kinds := []string{"files"}
	if inputs {
		kinds = append(kinds, "inputs")
	}

	for _, kind := range kinds {
		stream, err := DecryptBlob(storage, repository, catalogs[kind], identity)
		if err != nil {
			return nil, err
		}

		compressed, err := gzip.NewReader(stream)
		if err != nil {
			stream.Close()
			return nil, err
		}

		raw, err := readLimited(compressed, CatalogLimit)
		compressed.Close()
		if err == nil {
			_, err = io.Copy(io.Discard, stream)
		}

		stream.Close()
		if err != nil {
			return nil, err
		}

		var data snapshotCatalog
		if err = json.Unmarshal(raw, &data); err != nil {
			return nil, err
		}

		if data.Format != SnapshotFormat || data.Version != 2 {
			return nil, errors.New("unsupported catalog format")
		}
		if data.Files == nil {
			return nil, errors.New("missing catalog files")
		}

		for _, file := range data.Files {
			descriptor := file.Blob
			target, ok := reachable[descriptor.Digest]
			if !ok || target.Size != descriptor.Size || target.MediaType != descriptor.MediaType {
				return nil, errors.New("catalog references an unreachable blob")
			}
		}

		if kind == "files" {
			if data.Metadata == nil || data.Narinfos == nil || data.Upstream == nil {
				return nil, errors.New("incomplete files catalog")
			}

			result.Files, result.Metadata, result.Narinfos, result.Upstream = data.Files, data.Metadata, data.Narinfos, data.Upstream
		} else {
			result.Inputs = data.Files
		}
	}

	if err = result.ValidateRecords(); err != nil {
		return nil, err
	}

	return result, nil
}

func (s *Snapshot) ValidateRecords() error {
	for path, refs := range s.Upstream {
		if !ValidStorePath(path) || refs == nil {
			return errors.New("invalid upstream coverage")
		}

		for _, ref := range refs {
			if !ValidStorePath(ref) {
				return errors.New("invalid upstream coverage")
			}
		}
	}

	for name, value := range s.Narinfos {
		fields, err := NarinfoFields(value)
		if err != nil {
			return err
		}

		if _, ok := s.Files["cache/"+fields["URL"]]; name != NarinfoKey(fields["StorePath"]) || !ok {
			return errors.New("narinfo references an unreachable archive")
		}
	}

	return nil
}

func (s *Snapshot) RequireClosed() error {
	if err := s.ValidateRecords(); err != nil {
		return err
	}

	paths := map[string]bool{}
	for path := range s.Upstream {
		paths[path] = true
	}

	for _, value := range s.Narinfos {
		fields, _ := NarinfoFields(value)
		paths[fields["StorePath"]] = true
	}

	for _, refs := range s.Upstream {
		for _, ref := range refs {
			if !paths[ref] {
				return errors.New("upstream coverage contains incomplete references")
			}
		}
	}

	for _, value := range s.Narinfos {
		fields, _ := NarinfoFields(value)
		for _, ref := range strings.Fields(fields["References"]) {
			if !paths["/nix/store/"+ref] {
				return errors.New("cache contains incomplete references")
			}
		}
	}

	return nil
}

func (s *Snapshot) Contains(path string) bool {
	if _, ok := s.Upstream[path]; ok {
		return true
	}

	value, ok := s.Narinfos[NarinfoKey(path)]
	return ok && strings.HasPrefix(value, "StorePath: "+path+"\n")
}

func (s *Snapshot) Add(name string, source io.Reader, recipients any, private bool) error {
	if !private && strings.HasSuffix(name, ".narinfo") {
		return errors.New("narinfos belong in the inline catalog")
	}

	stream, err := EncryptedStream(source, recipients)
	if err != nil {
		return err
	}
	defer stream.Close()

	descriptor, err := s.Storage.UploadBlob(s.Repository, stream, true)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if private {
		s.Inputs[name] = wholeFile(descriptor)
	} else {
		s.Files[name] = wholeFile(descriptor)
	}

	return nil
}

func (s *Snapshot) HasFile(name string) bool {
	_, file := s.Files[name]
	_, record := s.Narinfos[name]
	return file || record
}

func (s *Snapshot) Read(name string, identity any, private bool) (io.ReadCloser, error) {
	if !private {
		if value, ok := s.Narinfos[name]; ok {
			return io.NopCloser(strings.NewReader(value)), nil
		}
	}

	mapping := s.Files
	if private {
		mapping = s.Inputs
	}

	file, ok := mapping[name]
	if !ok {
		return nil, ErrObjectNotFound
	}

	source, err := s.Storage.BlobRange(s.Repository, file)
	if errors.Is(err, ErrObjectNotFound) {
		return nil, errors.New("snapshot file references a missing blob")
	}
	if err != nil {
		return nil, err
	}
	stream, err := decryptedStream(source, identity)
	if err != nil {
		source.Close()
	}

	return stream, err
}

func (s *Snapshot) Merge(other *Snapshot) error {
	for path, refs := range other.Upstream {
		if existing, ok := s.Upstream[path]; ok && !slices.Equal(existing, refs) {
			return errors.New("conflicting upstream references")
		}

		s.Upstream[path] = slices.Clone(refs)
	}

	for name, value := range other.Narinfos {
		if existing, ok := s.Narinfos[name]; ok {
			left, err := NarinfoFields(existing)
			if err != nil {
				return err
			}

			right, err := NarinfoFields(value)
			if err != nil {
				return err
			}

			for _, key := range []string{"StorePath", "NarHash", "NarSize", "References", "URL"} {
				if left[key] != right[key] {
					return errors.New("conflicting cache record")
				}
			}
		} else {
			s.Narinfos[name] = value
		}
	}

	for name, descriptor := range other.Files {
		if strings.HasPrefix(name, "cache/") {
			if _, ok := s.Files[name]; !ok {
				s.Files[name] = descriptor
			}
		}
	}

	return nil
}

func (s *Snapshot) PreferUpstream() error {
	archives := map[string]bool{}
	for name, value := range s.Narinfos {
		fields, err := NarinfoFields(value)
		if err != nil {
			return err
		}

		if _, ok := s.Upstream[fields["StorePath"]]; ok {
			delete(s.Narinfos, name)
		} else {
			archives["cache/"+fields["URL"]] = true
		}
	}

	for name := range s.Files {
		if strings.HasPrefix(name, "cache/nar/") && !archives[name] {
			delete(s.Files, name)
		}
	}

	return nil
}

func (s *Snapshot) Publish(tag string, recipients, controlRecipients any) (string, error) {
	if err := s.ValidateRecords(); err != nil {
		return "", err
	}
	revision := ""
	if s.Metadata["kind"] == "source" {
		revision, _ = s.Metadata["revision"].(string)
		if _, err := SourceTag(revision); err != nil {
			return "", err
		}
	}

	layers := map[string]Descriptor{}
	for _, mapping := range []map[string]SnapshotFile{s.Files, s.Inputs} {
		for _, file := range mapping {
			if err := file.validate(); err != nil {
				return "", err
			}
			d := file.Blob
			if existing, ok := layers[d.Digest]; ok && (existing.Size != d.Size || existing.MediaType != d.MediaType) {
				return "", errors.New("conflicting snapshot blob descriptors")
			}
			layers[d.Digest] = d
		}
	}

	for _, kind := range []string{"files", "inputs"} {
		mapping, keys := s.Files, recipients
		data := map[string]any{
			"format":  SnapshotFormat,
			"version": 2,
		}
		if kind == "files" {
			data["metadata"], data["narinfos"], data["upstream"] = s.Metadata, s.Narinfos, s.Upstream
		} else {
			mapping, keys = s.Inputs, controlRecipients
		}

		data["files"] = mapping
		raw, err := json.Marshal(data)
		if err != nil {
			return "", err
		}
		if len(raw) > CatalogLimit {
			return "", errors.New("catalog exceeds limit")
		}

		var compressed bytes.Buffer
		gzipWriter, _ := gzip.NewWriterLevel(&compressed, 3)
		if _, err = gzipWriter.Write(raw); err != nil {
			return "", err
		}

		if err = gzipWriter.Close(); err != nil {
			return "", err
		}

		stream, err := EncryptedStream(&compressed, keys)
		if err != nil {
			return "", err
		}

		descriptor, err := s.Storage.UploadBlob(s.Repository, stream, true)
		stream.Close()
		if err != nil {
			return "", err
		}

		descriptor.Annotations = map[string]string{CatalogTitle: kind}
		layers[descriptor.Digest] = descriptor
	}

	config, err := s.Storage.UploadBlob(s.Repository, strings.NewReader("{}"), false)
	if err != nil {
		return "", err
	}

	config.MediaType = "application/octet-stream"
	names := make([]string, 0, len(layers))
	for digest := range layers {
		names = append(names, digest)
	}

	sort.Strings(names)
	descriptors := make([]Descriptor, 0, len(layers))
	for _, digest := range names {
		descriptors = append(descriptors, layers[digest])
	}

	s.Manifest = Manifest{
		SchemaVersion: 2,
		MediaType:     manifestMediaType,
		Config:        config,
		Layers:        descriptors,
		Annotations: map[string]string{
			"org.opencontainers.image.source": "https://github.com/" + strings.TrimPrefix(s.Repository, "ghcr.io/"),
			CatalogTitle:                      ArtifactTitle(tag),
		},
	}
	if revision != "" {
		s.Manifest.Annotations[SourceRevisionAnnotation] = revision
	}
	body, err := json.Marshal(s.Manifest)
	if err != nil {
		return "", err
	}

	s.ManifestBytes = int64(len(body))
	if len(body) > metadataLimit {
		return "", errors.New("manifest exceeds limit")
	}

	s.Digest, err = s.Storage.PutManifest(s.Repository, tag, s.Manifest)
	return s.Digest, err
}

type SnapshotReader struct {
	Storage              Storage
	Config               map[string]any
	Identity             any
	Log                  io.Writer
	mu                   sync.Mutex
	stateMu              sync.RWMutex
	snapshot             *Snapshot
	deadline, retryAfter time.Time
	refreshError         error
	now                  func() time.Time
}

func NewSnapshotReader(storage Storage, config map[string]any, identity any, log io.Writer) *SnapshotReader {
	return &SnapshotReader{
		Storage:  storage,
		Config:   config,
		Identity: identity,
		Log:      log,
		now:      time.Now,
	}
}

func (r *SnapshotReader) Current(name string) (*Snapshot, error) {
	r.stateMu.RLock()
	snapshot := r.snapshot
	r.stateMu.RUnlock()
	if snapshot != nil && (name == "" || snapshot.HasFile(name)) {
		if !r.mu.TryLock() {
			return snapshot, nil
		}
	} else {
		r.mu.Lock()
	}
	defer r.mu.Unlock()

	missing := r.snapshot == nil || (name != "" && !r.snapshot.HasFile(name))
	now := r.now()
	if !now.Before(r.deadline) || (missing && !now.Before(r.retryAfter)) {
		reference, _ := r.Config["reference"].(string)
		if reference == "" {
			reference = "nixos-cache-latest"
		}

		repository, _ := r.Config["repository"].(string)
		_, digest, err := r.Storage.GetManifest(repository, reference)
		var loaded *Snapshot
		if err == nil && (r.snapshot == nil || r.snapshot.Digest != digest) {
			loaded, err = LoadSnapshot(r.Storage, repository, digest, r.Identity, false)
		}

		if err != nil {
			r.refreshError = fmt.Errorf("cache catalog refresh failed: %w", err)
			r.deadline, r.retryAfter = r.now().Add(5*time.Second), r.now().Add(5*time.Second)
			if r.Log != nil {
				fmt.Fprintln(r.Log, r.refreshError)
			}
		} else {
			if loaded != nil {
				r.stateMu.Lock()
				r.snapshot = loaded
				r.stateMu.Unlock()
			}

			r.refreshError = nil
			r.deadline, r.retryAfter = r.now().Add(30*time.Second), r.now().Add(5*time.Second)
			if strings.HasPrefix(reference, "sha256:") {
				r.deadline, r.retryAfter = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			}
		}
	}

	if r.refreshError != nil && (r.snapshot == nil || (name != "" && !r.snapshot.HasFile(name))) {
		return nil, r.refreshError
	}

	return r.snapshot, nil
}

var cachePathPattern = regexp.MustCompile(`^(?:nix-cache-info|[0-9abcdfghijklmnpqrsvwxyz]{32}\.narinfo|nar/[A-Za-z0-9._-]+\.nar(?:\.[A-Za-z0-9]+)?)$`)

func ValidCachePath(path string) bool { return cachePathPattern.MatchString(path) }

type CacheHandler struct {
	storage  Storage
	identity any
	reader   *SnapshotReader
	log      io.Writer
	mu       sync.RWMutex
	snapshot *Snapshot
	errors   []error
	slots    chan struct{}
}

func NewCacheHandler(storage Storage, config map[string]any, identity any, log io.Writer) *CacheHandler {
	h := &CacheHandler{
		storage:  storage,
		identity: identity,
		log:      log,
		slots:    make(chan struct{}, 32),
	}
	h.snapshot, _ = config["snapshot"].(*Snapshot)
	h.reader, _ = config["reader"].(*SnapshotReader)
	if h.snapshot == nil && h.reader == nil {
		h.reader = NewSnapshotReader(storage, config, identity, log)
	}

	return h
}

func (h *CacheHandler) SetSnapshot(snapshot *Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.snapshot = snapshot
}

func (h *CacheHandler) Errors() []error {
	h.mu.RLock()
	defer h.mu.RUnlock()

	return slices.Clone(h.errors)
}

func (h *CacheHandler) current(name string) (*Snapshot, error) {
	h.mu.RLock()
	snapshot := h.snapshot
	h.mu.RUnlock()
	if snapshot != nil {
		return snapshot, nil
	}
	if h.reader == nil {
		return nil, ErrObjectNotFound
	}

	return h.reader.Current(name)
}

func (h *CacheHandler) failure(w http.ResponseWriter, name string, err error, started bool) {
	if !errors.Is(err, ErrObjectNotFound) {
		h.mu.Lock()
		h.errors = append(h.errors, err)
		if h.log != nil {
			fmt.Fprintf(h.log, "Cache request %s failed: %v\n", name, err)
		}

		h.mu.Unlock()
	}

	if started {
		panic(http.ErrAbortHandler)
	}

	status := http.StatusBadGateway
	if errors.Is(err, ErrObjectNotFound) {
		status = http.StatusNotFound
	}

	http.Error(w, http.StatusText(status), status)
}

func (h *CacheHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		panic(http.ErrAbortHandler)
	}

	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(r.RequestURI, "/")
	if !ValidCachePath(name) {
		http.NotFound(w, r)
		return
	}

	if name == "nix-cache-info" {
		data := "StoreDir: /nix/store\nWantMassQuery: 1\n"
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(200)
		if r.Method != "HEAD" {
			io.WriteString(w, data)
		}

		return
	}

	snapshot, err := h.current("cache/" + name)
	if err != nil {
		h.failure(w, name, err, false)
		return
	}

	if r.Method == "HEAD" {
		if !snapshot.HasFile("cache/" + name) {
			h.failure(w, name, ErrObjectNotFound, false)
			return
		}

		w.WriteHeader(200)
		return
	}

	stream, err := snapshot.Read("cache/"+name, h.identity, false)
	if err != nil {
		h.failure(w, name, err, false)
		return
	}
	defer stream.Close()

	buffer := make([]byte, 64*1024)
	n, readErr := stream.Read(buffer)
	if readErr != nil && readErr != io.EOF && n == 0 {
		h.failure(w, name, readErr, false)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(200)
	for {
		if n > 0 {
			http.NewResponseController(w).SetWriteDeadline(time.Now().Add(120 * time.Second))
			if _, err = w.Write(buffer[:n]); err != nil {
				return
			}

			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}

		if readErr != nil {
			if readErr != io.EOF {
				h.failure(w, name, readErr, true)
			}

			return
		}

		n, readErr = stream.Read(buffer)
	}
}

type cacheListener struct {
	net.Listener
	slots chan struct{}
}

type cacheConnection struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *cacheConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func (l *cacheListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		select {
		case l.slots <- struct{}{}:
			return &cacheConnection{
				Conn:    connection,
				release: func() { <-l.slots },
			}, nil
		default:
			connection.Close()
		}
	}
}

func StartCacheServer(handler http.Handler, port int) (*http.Server, net.Listener, error) {
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, nil, err
	}

	bounded := &cacheListener{
		Listener: listener,
		slots:    make(chan struct{}, 32),
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go server.Serve(bounded)
	return server, bounded, nil
}
