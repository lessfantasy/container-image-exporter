package registry

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/container-image-exporter/container-image-exporter/internal/image"
)

func host(srv *httptest.Server) string { return strings.TrimPrefix(srv.URL, "http://") }

func mustRef(t *testing.T, s string) image.Ref {
	t.Helper()
	ref, err := image.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// newRegistry starts an in-memory OCI registry.
func newRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	return srv
}

func pushImage(t *testing.T, ref string) v1.Hash {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()
	return digest
}

func pushIndex(t *testing.T, ref string) (v1.Hash, []string) {
	t.Helper()
	idx, err := random.Index(64, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, idx); err != nil {
		t.Fatal(err)
	}
	digest, _ := idx.Digest()
	manifest, _ := idx.IndexManifest()
	var children []string
	for _, m := range manifest.Manifests {
		children = append(children, m.Digest.String())
	}
	return digest, children
}

func TestListTagsAndDigests(t *testing.T) {
	srv := newRegistry(t)
	repo := host(srv) + "/team/app"
	single := pushImage(t, repo+":1.0.0")
	pushImage(t, repo+":1.1.0")
	indexDigest, children := pushIndex(t, repo+":latest")

	c := New(Options{UserAgent: "test"})
	ctx := context.Background()

	tags, err := c.ListTags(ctx, mustRef(t, repo+":1.0.0"), nil)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(tags)
	if want := []string{"1.0.0", "1.1.0", "latest"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}

	// Single-platform image.
	m, err := c.Digest(ctx, mustRef(t, repo+":1.0.0"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest != single.String() || m.IsIndex {
		t.Errorf("Digest = %+v, want %s", m, single)
	}
	m, err = c.Manifest(ctx, mustRef(t, repo+":1.0.0"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest != single.String() || m.IsIndex || len(m.Platforms) != 0 {
		t.Errorf("Manifest = %+v", m)
	}

	// Multi-platform image: HEAD gives the index digest, GET the children.
	m, err = c.Digest(ctx, mustRef(t, repo+":latest"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest != indexDigest.String() || !m.IsIndex {
		t.Errorf("Digest = %+v, want index %s", m, indexDigest)
	}
	m, err = c.Manifest(ctx, mustRef(t, repo+":latest"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Digest != indexDigest.String() || !m.IsIndex || !reflect.DeepEqual(m.Platforms, children) {
		t.Errorf("Manifest = %+v, want children %v", m, children)
	}
}

func TestNotFound(t *testing.T) {
	srv := newRegistry(t)
	c := New(Options{})
	ref := mustRef(t, host(srv)+"/missing/app:1.0.0")

	if _, err := c.ListTags(context.Background(), ref, nil); !IsNotFound(err) {
		t.Errorf("ListTags error = %v, want not found", err)
	}
	_, err := c.Digest(context.Background(), ref, nil)
	if !IsNotFound(err) {
		t.Errorf("Digest error = %v, want not found", err)
	}
	if got := Reason(err); got != "repository or tag not found" {
		t.Errorf("Reason = %q", got)
	}
}

// scripted is a minimal registry whose tag endpoint is controlled by the test.
type scripted struct {
	tagRequests atomic.Int64
	handler     func(n int64, w http.ResponseWriter, r *http.Request)
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.handler(s.tagRequests.Add(1), w, r)
}

func newScripted(t *testing.T, h func(n int64, w http.ResponseWriter, r *http.Request)) (*httptest.Server, *scripted) {
	t.Helper()
	s := &scripted{handler: h}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv, s
}

func TestListTagsPagination(t *testing.T) {
	pages := map[string]struct {
		tags []string
		next string
	}{
		"":      {[]string{"1.0.0", "1.1.0"}, "1.1.0"},
		"1.1.0": {[]string{"1.2.0", "1.3.0"}, "1.3.0"},
		"1.3.0": {[]string{"2.0.0"}, ""},
	}
	srv, s := newScripted(t, func(_ int64, w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("n") != "2" {
			t.Errorf("page size not forwarded: %s", r.URL.RawQuery)
		}
		page := pages[r.URL.Query().Get("last")]
		if page.next != "" {
			w.Header().Set("Link", fmt.Sprintf(`</v2/app/tags/list?n=2&last=%s>; rel="next"`, page.next))
		}
		fmt.Fprintf(w, `{"name":"app","tags":["%s"]}`, strings.Join(page.tags, `","`))
	})

	c := New(Options{PageSize: 2})
	tags, err := c.ListTags(context.Background(), mustRef(t, host(srv)+"/app:1.0.0"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"1.0.0", "1.1.0", "1.2.0", "1.3.0", "2.0.0"}; !reflect.DeepEqual(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
	if got := s.tagRequests.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 pages", got)
	}
}

func TestRateLimitIsNotRetried(t *testing.T) {
	srv, s := newScripted(t, func(_ int64, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"errors":[{"code":"TOOMANYREQUESTS","message":"slow down"}]}`)
	})
	c := New(Options{})
	_, err := c.ListTags(context.Background(), mustRef(t, host(srv)+"/app:1.0.0"), nil)
	if !IsRateLimited(err) {
		t.Fatalf("error = %v, want rate limited", err)
	}
	if got := Reason(err); got != "rate limited" {
		t.Errorf("Reason = %q", got)
	}
	if got := s.tagRequests.Load(); got != 1 {
		t.Errorf("requests = %d, want exactly 1 (no retry on 429)", got)
	}
}

func TestServerErrorIsRetried(t *testing.T) {
	srv, s := newScripted(t, func(n int64, w http.ResponseWriter, _ *http.Request) {
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"name":"app","tags":["1.0.0"]}`)
	})
	c := New(Options{})
	tags, err := c.ListTags(context.Background(), mustRef(t, host(srv)+"/app:1.0.0"), nil)
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 1 || s.tagRequests.Load() != 2 {
		t.Errorf("tags = %v after %d requests", tags, s.tagRequests.Load())
	}
}

func TestPersistentServerErrorGivesUp(t *testing.T) {
	srv, s := newScripted(t, func(_ int64, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	c := New(Options{})
	_, err := c.ListTags(context.Background(), mustRef(t, host(srv)+"/app:1.0.0"), nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := Reason(err); got != "registry unavailable" {
		t.Errorf("Reason = %q (%v)", got, err)
	}
	if got := s.tagRequests.Load(); got < 2 || got > 12 {
		t.Errorf("requests = %d, want a bounded number of retries", got)
	}
}

func TestUnauthorized(t *testing.T) {
	srv, _ := newScripted(t, func(_ int64, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
	})
	c := New(Options{})
	ref := mustRef(t, host(srv)+"/private/app:latest")
	_, err := c.ListTags(context.Background(), ref, nil)
	if !IsUnauthorized(err) || Reason(err) != "authentication failed" {
		t.Errorf("ListTags error = %v (%q)", err, Reason(err))
	}
	if _, err := c.Digest(context.Background(), ref, nil); !IsUnauthorized(err) {
		t.Errorf("Digest error = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	srv, _ := newScripted(t, func(_ int64, _ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	c := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.ListTags(ctx, mustRef(t, host(srv)+"/app:1.0.0"), nil)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if got := Reason(err); got != "timeout" {
		t.Errorf("Reason = %q (%v)", got, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout not honoured, took %s", elapsed)
	}
}

func TestRegistryUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := host(srv)
	srv.Close() // nothing listens here any more

	c := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := c.Digest(ctx, mustRef(t, addr+"/app:latest"), nil)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if got := Reason(err); got != "registry request failed" && got != "timeout" {
		t.Errorf("Reason = %q (%v)", got, err)
	}
}

// basicAuthRegistry requires credentials for every request.
func basicAuthRegistry(t *testing.T, user, pass string) *httptest.Server {
	t.Helper()
	inner := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"authentication required"}]}`)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type staticKeychain struct {
	host string
	auth authn.Authenticator
}

func (k staticKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if r.RegistryStr() == k.host {
		return k.auth, nil
	}
	return authn.Anonymous, nil
}

func TestPrivateRegistryAuthentication(t *testing.T) {
	srv := basicAuthRegistry(t, "robot", "s3cret")
	creds := &authn.Basic{Username: "robot", Password: "s3cret"}
	repo := host(srv) + "/private/app"

	img, _ := random.Image(64, 1)
	tag, _ := name.NewTag(repo + ":2.0.0")
	if err := remote.Write(tag, img, remote.WithAuth(creds)); err != nil {
		t.Fatal(err)
	}
	ref := mustRef(t, repo+":2.0.0")
	ctx := context.Background()

	// Anonymous access is rejected.
	if _, err := New(Options{}).ListTags(ctx, ref, nil); !IsUnauthorized(err) {
		t.Errorf("anonymous error = %v, want unauthorized", err)
	}

	// Global keychain.
	withGlobal := New(Options{Keychain: staticKeychain{host: host(srv), auth: creds}})
	if tags, err := withGlobal.ListTags(ctx, ref, nil); err != nil || len(tags) != 1 {
		t.Errorf("global keychain: tags=%v err=%v", tags, err)
	}

	// Per-workload keychain (imagePullSecrets) takes precedence over a
	// global keychain without credentials for the registry.
	withExtra := New(Options{Keychain: staticKeychain{host: "other.example.com", auth: creds}})
	if _, err := withExtra.Digest(ctx, ref, staticKeychain{host: host(srv), auth: creds}); err != nil {
		t.Errorf("extra keychain: %v", err)
	}

	// Wrong credentials fail, and the error text does not leak them.
	bad := New(Options{Keychain: staticKeychain{host: host(srv), auth: &authn.Basic{Username: "robot", Password: "wr0ng-pass"}}})
	_, err := bad.ListTags(ctx, ref, nil)
	if !IsUnauthorized(err) {
		t.Fatalf("bad credentials error = %v", err)
	}
	if strings.Contains(err.Error(), "wr0ng-pass") {
		t.Errorf("error leaks the password: %v", err)
	}
}

func TestLenientKeychain(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	target, _ := name.NewRegistry("ghcr.io")

	failing := lenient{name: "x", inner: failingKeychain{}, logger: logger}
	auth, err := failing.Resolve(target)
	if err != nil || auth != authn.Anonymous {
		t.Errorf("failing source: auth=%v err=%v, want anonymous", auth, err)
	}

	creds := &authn.Basic{Username: "u", Password: "p"}
	gated := lenient{name: "x", inner: staticKeychain{host: "ghcr.io", auth: creds}, logger: logger, match: isACRHost}
	if auth, _ := gated.Resolve(target); auth != authn.Anonymous {
		t.Error("host gate ignored")
	}
	open := lenient{name: "x", inner: staticKeychain{host: "ghcr.io", auth: creds}, logger: logger}
	if auth, _ := open.Resolve(target); auth != creds {
		t.Error("credentials not passed through")
	}
}

type failingKeychain struct{}

func (failingKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return nil, fmt.Errorf("broken credential source")
}

func TestRegistryHostMatching(t *testing.T) {
	tests := []struct {
		host             string
		ecr, google, acr bool
	}{
		{"123456789012.dkr.ecr.eu-west-1.amazonaws.com", true, false, false},
		{"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn", true, false, false},
		{"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com", true, false, false},
		{"public.ecr.aws", true, false, false},
		{"gcr.io", false, true, false},
		{"eu.gcr.io", false, true, false},
		{"europe-west1-docker.pkg.dev", false, true, false},
		{"myregistry.azurecr.io", false, false, true},
		{"index.docker.io", false, false, false},
		{"ghcr.io", false, false, false},
		{"quay.io", false, false, false},
		{"evil-gcr.io.example.com", false, false, false},
		{"dkr.ecr.amazonaws.com.example.com", false, false, false},
	}
	for _, tt := range tests {
		if got := ecrHostRE.MatchString(tt.host); got != tt.ecr {
			t.Errorf("ecr(%s) = %v", tt.host, got)
		}
		if got := isGoogleHost(tt.host); got != tt.google {
			t.Errorf("google(%s) = %v", tt.host, got)
		}
		if got := isACRHost(tt.host); got != tt.acr {
			t.Errorf("acr(%s) = %v", tt.host, got)
		}
	}
}

// TestDefaultKeychainAnonymous checks that public registries work without
// any credentials configured.
func TestDefaultKeychainAnonymous(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	kc := DefaultKeychain(slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, h := range []string{"index.docker.io", "ghcr.io", "quay.io"} {
		target, _ := name.NewRegistry(h)
		auth, err := kc.Resolve(target)
		if err != nil || auth != authn.Anonymous {
			t.Errorf("%s: auth=%v err=%v, want anonymous", h, auth, err)
		}
	}
}
