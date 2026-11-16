package scanner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
	"github.com/container-image-exporter/container-image-exporter/internal/image"
	"github.com/container-image-exporter/container-image-exporter/internal/metrics"
	"github.com/container-image-exporter/container-image-exporter/internal/registry"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeDiscoverer struct {
	mu        sync.Mutex
	instances []discovery.Instance
	err       error
}

func (f *fakeDiscoverer) set(instances []discovery.Instance, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances, f.err = instances, err
}

func (f *fakeDiscoverer) Discover(context.Context) ([]discovery.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances, f.err
}

// fakeRegistry is a mocked registry keyed by repository.
type fakeRegistry struct {
	mu        sync.Mutex
	tags      map[string][]string          // repository key -> tags
	manifests map[string]registry.Manifest // "repo:tag" -> manifest with platforms
	errs      map[string]error             // repository key -> error
	block     chan struct{}                // when set, calls wait for close or ctx
	keychains []authn.Keychain

	listCalls, digestCalls, manifestCalls atomic.Int64
	inFlight, maxInFlight                 atomic.Int64
}

func (f *fakeRegistry) enter(ctx context.Context, ref image.Ref, kc authn.Keychain) error {
	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		m := f.maxInFlight.Load()
		if n <= m || f.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	block := f.block
	err := f.errs[ref.RepositoryKey()]
	f.keychains = append(f.keychains, kc)
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		time.Sleep(time.Millisecond) // let workers overlap
	}
	return err
}

func (f *fakeRegistry) ListTags(ctx context.Context, ref image.Ref, kc authn.Keychain) ([]string, error) {
	f.listCalls.Add(1)
	if err := f.enter(ctx, ref, kc); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tags[ref.RepositoryKey()], nil
}

func (f *fakeRegistry) Digest(ctx context.Context, ref image.Ref, kc authn.Keychain) (registry.Manifest, error) {
	f.digestCalls.Add(1)
	if err := f.enter(ctx, ref, kc); err != nil {
		return registry.Manifest{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.manifests[ref.String()]
	m.Platforms = nil // HEAD never returns platform manifests
	return m, nil
}

func (f *fakeRegistry) Manifest(ctx context.Context, ref image.Ref, kc authn.Keychain) (registry.Manifest, error) {
	f.manifestCalls.Add(1)
	if err := f.enter(ctx, ref, kc); err != nil {
		return registry.Manifest{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifests[ref.String()], nil
}

func (f *fakeRegistry) setErr(repo string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errs == nil {
		f.errs = map[string]error{}
	}
	if err == nil {
		delete(f.errs, repo)
		return
	}
	f.errs[repo] = err
}

func httpErr(code int) error { return &transport.Error{StatusCode: code} }

func inst(t *testing.T, container, img string, digests ...string) discovery.Instance {
	t.Helper()
	ref, err := image.Parse(img)
	if err != nil {
		t.Fatal(err)
	}
	return discovery.Instance{Platform: discovery.PlatformDocker, Container: container, RawImage: img, Image: ref, Digests: digests}
}

func newScanner(disc discovery.Discoverer, reg Registry, clk *clock, concurrency int) *Scanner {
	return New(disc, reg, Options{
		ScanInterval:     time.Minute,
		RegistryInterval: time.Hour,
		RegistryTimeout:  5 * time.Second,
		Concurrency:      concurrency,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:              clk.Now,
	})
}

func newClock() *clock { return &clock{t: time.Unix(1_700_000_000, 0)} }

// info returns value and latest_tag of the info series of a container.
func info(t *testing.T, snap *metrics.Snapshot, container string) (float64, string) {
	t.Helper()
	if snap == nil {
		t.Fatal("no snapshot")
	}
	for _, s := range snap.Info {
		if s.Labels[1] == container {
			return s.Value, s.Labels[7]
		}
	}
	t.Fatalf("no info series for %q in %+v", container, snap.Info)
	return 0, ""
}

// update returns the update value of a container; ok is false when the
// series is absent.
func update(snap *metrics.Snapshot, container string) (float64, bool) {
	for _, s := range snap.Update {
		if s.Labels[1] == container {
			return s.Value, true
		}
	}
	return 0, false
}

func TestSemVerAndFloatingChecks(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{
		tags: map[string][]string{
			"docker.io/library/nginx": {"1.26.2", "1.27.0", "1.27.0-alpine", "latest"},
			"docker.io/library/redis": {"7.4.1", "7.4.0"},
		},
		manifests: map[string]registry.Manifest{
			"ghcr.io/org/app:latest":   {Digest: digestA},
			"ghcr.io/org/old:stable":   {Digest: digestB},
			"ghcr.io/org/multi:latest": {Digest: digestB, IsIndex: true, Platforms: []string{digestA, digestC}},
		},
	}
	disc.set([]discovery.Instance{
		inst(t, "outdated", "nginx:1.26.2"),
		inst(t, "current", "redis:7.4.1"),
		inst(t, "float-same", "ghcr.io/org/app:latest", digestA),
		inst(t, "float-diff", "ghcr.io/org/old:stable", digestA),
		inst(t, "float-platform", "ghcr.io/org/multi:latest", digestA),
		inst(t, "float-nodigest", "ghcr.io/org/app:latest"),
		inst(t, "unsupported", "ghcr.io/org/app:abc123"),
	}, nil)

	s := newScanner(disc, reg, newClock(), 4)
	s.Cycle(context.Background())
	snap := s.Snapshot()

	tests := []struct {
		container  string
		info       float64
		latest     string
		update     float64
		hasUpdate  bool
		wantDigest bool
	}{
		{"outdated", 1, "1.27.0", 1, true, false},
		{"current", 1, "7.4.1", 0, true, false},
		{"float-same", 1, "latest", 0, true, false},
		{"float-diff", 1, "stable", 1, true, false},
		{"float-platform", 1, "latest", 0, true, false},
		{"float-nodigest", 0, "", 0, false, false},
		{"unsupported", 0, "", 0, false, false},
	}
	for _, tt := range tests {
		value, latest := info(t, snap, tt.container)
		if value != tt.info || latest != tt.latest {
			t.Errorf("%s: info=%v latest=%q, want %v %q", tt.container, value, latest, tt.info, tt.latest)
		}
		got, ok := update(snap, tt.container)
		if ok != tt.hasUpdate || got != tt.update {
			t.Errorf("%s: update=%v present=%v, want %v %v", tt.container, got, ok, tt.update, tt.hasUpdate)
		}
	}

	// The platform manifests are only fetched when the tag digest differs
	// and the image is an index: exactly once here.
	if got := reg.manifestCalls.Load(); got != 1 {
		t.Errorf("manifest GETs = %d, want 1", got)
	}
	// No registry request for an instance without a running digest or
	// with an unsupported tag: 3 HEADs, 2 listings.
	if reg.digestCalls.Load() != 3 || reg.listCalls.Load() != 2 {
		t.Errorf("digest HEADs = %d, listings = %d", reg.digestCalls.Load(), reg.listCalls.Load())
	}
}

func TestLookupsAreDeduplicated(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{
		tags:      map[string][]string{"docker.io/library/nginx": {"1.26.2", "1.27.0", "1.27.0-alpine", "1.26.2-alpine"}},
		manifests: map[string]registry.Manifest{"ghcr.io/org/app:latest": {Digest: digestA}},
	}
	disc.set([]discovery.Instance{
		inst(t, "web-1", "nginx:1.26.2"),
		inst(t, "web-2", "nginx:1.26.2"),
		inst(t, "web-3", "docker.io/library/nginx:1.26.2"),
		inst(t, "web-alpine", "nginx:1.26.2-alpine"), // same repository, other tag
		inst(t, "app-1", "ghcr.io/org/app:latest", digestA),
		inst(t, "app-2", "ghcr.io/org/app:latest", digestA),
	}, nil)

	clk := newClock()
	s := newScanner(disc, reg, clk, 4)
	s.Cycle(context.Background())
	if got := reg.listCalls.Load(); got != 1 {
		t.Errorf("tag listings = %d, want 1 for the whole repository", got)
	}
	if got := reg.digestCalls.Load(); got != 1 {
		t.Errorf("digest lookups = %d, want 1", got)
	}
	if _, latest := info(t, s.Snapshot(), "web-alpine"); latest != "1.27.0-alpine" {
		t.Errorf("alpine latest = %q", latest)
	}

	// Results are cached: scans inside the registry interval cost nothing.
	for range 5 {
		clk.Advance(time.Minute)
		s.Cycle(context.Background())
	}
	if reg.listCalls.Load() != 1 || reg.digestCalls.Load() != 1 {
		t.Errorf("cached results were refreshed: lists=%d digests=%d", reg.listCalls.Load(), reg.digestCalls.Load())
	}

	// After the registry interval they are refreshed.
	clk.Advance(time.Hour)
	s.Cycle(context.Background())
	if reg.listCalls.Load() != 2 || reg.digestCalls.Load() != 2 {
		t.Errorf("results not refreshed: lists=%d digests=%d", reg.listCalls.Load(), reg.digestCalls.Load())
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{tags: map[string][]string{}}
	var instances []discovery.Instance
	for i := range 40 {
		instances = append(instances, inst(t, "c"+string(rune('a'+i%26))+string(rune('a'+i/26)), "ghcr.io/org/app"+string(rune('a'+i%26))+string(rune('a'+i/26))+":1.0.0"))
	}
	disc.set(instances, nil)

	s := newScanner(disc, reg, newClock(), 3)
	s.Cycle(context.Background())
	if got := reg.listCalls.Load(); got != 40 {
		t.Errorf("listings = %d, want 40", got)
	}
	if got := reg.maxInFlight.Load(); got > 3 || got < 2 {
		t.Errorf("max parallel checks = %d, want 2..3", got)
	}
}

func TestFailureIsNotCachedPermanentlyAndHidesStaleResult(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{tags: map[string][]string{"docker.io/library/nginx": {"1.26.2", "1.27.0"}}}
	disc.set([]discovery.Instance{inst(t, "web", "nginx:1.26.2")}, nil)
	clk := newClock()
	s := newScanner(disc, reg, clk, 2)

	s.Cycle(context.Background())
	if v, ok := update(s.Snapshot(), "web"); !ok || v != 1 {
		t.Fatalf("update = %v, %v", v, ok)
	}

	// The registry goes down when the result is due for refresh.
	reg.setErr("docker.io/library/nginx", httpErr(http.StatusServiceUnavailable))
	clk.Advance(time.Hour + time.Minute)
	s.Cycle(context.Background())
	value, latest := info(t, s.Snapshot(), "web")
	if value != 0 || latest != "" {
		t.Errorf("info = %v latest = %q, want 0 and empty (no stale result)", value, latest)
	}
	if _, ok := update(s.Snapshot(), "web"); ok {
		t.Error("update series still published after a failed check")
	}

	// Within the backoff no new request is made.
	calls := reg.listCalls.Load()
	clk.Advance(10 * time.Second)
	s.Cycle(context.Background())
	if reg.listCalls.Load() != calls {
		t.Error("failed check retried before the backoff elapsed")
	}

	// The registry recovers; the failure is retried after the backoff,
	// long before the registry interval.
	reg.setErr("docker.io/library/nginx", nil)
	clk.Advance(2 * time.Minute)
	s.Cycle(context.Background())
	if v, ok := update(s.Snapshot(), "web"); !ok || v != 1 {
		t.Errorf("after recovery: update = %v, %v", v, ok)
	}
}

func TestOneFailingImageDoesNotAffectOthers(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{
		tags: map[string][]string{"docker.io/library/nginx": {"1.26.2"}},
		errs: map[string]error{
			"ghcr.io/org/private": httpErr(http.StatusUnauthorized),
			"ghcr.io/org/removed": httpErr(http.StatusNotFound),
		},
	}
	disc.set([]discovery.Instance{
		inst(t, "ok", "nginx:1.26.2"),
		inst(t, "private", "ghcr.io/org/private:1.0.0"),
		inst(t, "removed", "ghcr.io/org/removed:latest", digestA),
		{Platform: discovery.PlatformDocker, Container: "broken", RawImage: "sha256:feed", ImageErr: image.ErrNoReference},
		inst(t, "pinned", "ghcr.io/org/app@"+digestA),
	}, nil)
	s := newScanner(disc, reg, newClock(), 2)
	s.Cycle(context.Background())
	snap := s.Snapshot()

	if v, _ := info(t, snap, "ok"); v != 1 {
		t.Error("healthy image affected by failures of others")
	}
	for _, c := range []string{"private", "removed", "broken", "pinned"} {
		if v, latest := info(t, snap, c); v != 0 || latest != "" {
			t.Errorf("%s: info = %v latest = %q", c, v, latest)
		}
		if _, ok := update(snap, c); ok {
			t.Errorf("%s: update series published", c)
		}
	}
	// The unresolvable image keeps its raw string as repository.
	for _, s := range snap.Info {
		if s.Labels[1] == "broken" && s.Labels[5] != "sha256:feed" {
			t.Errorf("broken repository label = %q", s.Labels[5])
		}
	}
}

func TestRemovedContainersDisappear(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{tags: map[string][]string{
		"docker.io/library/nginx": {"1.26.2"},
		"docker.io/library/redis": {"7.4.1"},
	}}
	disc.set([]discovery.Instance{inst(t, "web", "nginx:1.26.2"), inst(t, "cache", "redis:7.4.1")}, nil)
	clk := newClock()
	s := newScanner(disc, reg, clk, 2)
	s.Cycle(context.Background())
	if len(s.Snapshot().Info) != 2 || s.cache.Len() != 2 {
		t.Fatalf("snapshot = %+v", s.Snapshot())
	}

	disc.set([]discovery.Instance{inst(t, "web", "nginx:1.26.2")}, nil)
	clk.Advance(time.Minute)
	s.Cycle(context.Background())
	snap := s.Snapshot()
	if len(snap.Info) != 1 || len(snap.Update) != 1 || snap.Info[0].Labels[1] != "web" {
		t.Errorf("stopped container still exported: %+v", snap)
	}
	if s.cache.Len() != 1 {
		t.Errorf("cache not pruned: %d entries", s.cache.Len())
	}

	// A genuinely empty inventory is a valid, empty snapshot.
	disc.set(nil, nil)
	clk.Advance(time.Minute)
	s.Cycle(context.Background())
	if snap := s.Snapshot(); snap == nil || len(snap.Info) != 0 {
		t.Errorf("empty inventory: snapshot = %+v", snap)
	}
}

func TestStartupAndDiscoveryFailure(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{tags: map[string][]string{"docker.io/library/nginx": {"1.26.2"}}}
	clk := newClock()
	s := newScanner(disc, reg, clk, 2)

	// Before the first scan there is no snapshot.
	if s.Snapshot() != nil {
		t.Fatal("snapshot before the first scan")
	}

	// The runtime is unreachable at startup: still no snapshot, which is
	// different from an empty inventory.
	disc.set(nil, errors.New("docker daemon unavailable"))
	s.Cycle(context.Background())
	if s.Snapshot() != nil {
		t.Fatal("failed discovery produced a snapshot")
	}

	// First success.
	disc.set([]discovery.Instance{inst(t, "web", "nginx:1.26.2")}, nil)
	clk.Advance(time.Minute)
	s.Cycle(context.Background())
	if snap := s.Snapshot(); snap == nil || len(snap.Info) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}

	// A short outage keeps the last inventory.
	disc.set(nil, errors.New("docker daemon unavailable"))
	for range StaleFactor {
		clk.Advance(time.Minute)
		s.Cycle(context.Background())
		if snap := s.Snapshot(); snap == nil || len(snap.Info) != 1 {
			t.Fatalf("short outage dropped the inventory: %+v", snap)
		}
	}

	// A long outage withdraws the metrics instead of serving stale data.
	clk.Advance(time.Minute)
	s.Cycle(context.Background())
	if s.Snapshot() != nil {
		t.Fatal("stale inventory still served after a long outage")
	}

	// Recovery.
	disc.set([]discovery.Instance{inst(t, "web", "nginx:1.26.2")}, nil)
	clk.Advance(time.Minute)
	s.Cycle(context.Background())
	if snap := s.Snapshot(); snap == nil || len(snap.Info) != 1 {
		t.Fatalf("no recovery: %+v", snap)
	}
}

func TestRateLimitPausesRegistry(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{
		tags: map[string][]string{"quay.io/org/ok": {"1.0.0"}},
		errs: map[string]error{},
	}
	var instances []discovery.Instance
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		repo := "docker.io/library/" + name
		reg.errs[repo] = httpErr(http.StatusTooManyRequests)
		instances = append(instances, inst(t, name, name+":1.0.0"))
	}
	instances = append(instances, inst(t, "other", "quay.io/org/ok:1.0.0"))
	disc.set(instances, nil)

	clk := newClock()
	s := newScanner(disc, reg, clk, 1)
	s.Cycle(context.Background())

	// Concurrency 1: the first 429 pauses docker.io, the remaining five
	// images are not requested. The quay.io image is unaffected.
	if got := reg.listCalls.Load(); got != 2 {
		t.Errorf("listings = %d, want 2 (one 429, one quay.io)", got)
	}
	snap := s.Snapshot()
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		if v, _ := info(t, snap, name); v != 0 {
			t.Errorf("%s: info = %v, want 0", name, v)
		}
		if _, ok := update(snap, name); ok {
			t.Errorf("%s: update series published while rate limited", name)
		}
	}
	if v, _ := info(t, snap, "other"); v != 1 {
		t.Error("other registry affected by the rate limit")
	}

	// The limit is lifted; after the pause the images are checked again.
	for repo := range reg.errs {
		reg.setErr(repo, nil)
	}
	reg.mu.Lock()
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		reg.tags["docker.io/library/"+name] = []string{"1.0.0"}
	}
	reg.mu.Unlock()
	clk.Advance(3 * time.Minute)
	s.Cycle(context.Background())
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		if v, _ := info(t, s.Snapshot(), name); v != 1 {
			t.Errorf("%s: not recovered after the pause", name)
		}
	}
}

func TestRateLimitPauseGrowsAndIsCapped(t *testing.T) {
	clk := newClock()
	r := newRateLimits(clk.Now)
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, w := range want {
		if got := r.hit("docker.io"); got != w {
			t.Errorf("hit %d: pause = %s, want %s", i+1, got, w)
		}
		if _, limited := r.limited("docker.io"); !limited {
			t.Error("not limited right after a hit")
		}
		// A second worker hitting the limit during the pause does not extend it.
		if got := r.hit("docker.io"); got != w {
			t.Errorf("concurrent hit changed the pause to %s", got)
		}
		clk.Advance(w)
		if _, limited := r.limited("docker.io"); limited {
			t.Error("still limited after the pause")
		}
	}
	r.clear("docker.io")
	if got := r.hit("docker.io"); got != time.Minute {
		t.Errorf("pause after clear = %s", got)
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{block: make(chan struct{})}
	disc.set([]discovery.Instance{inst(t, "slow", "nginx:1.26.2")}, nil)

	// Timeout: the check fails, the cycle completes.
	s := New(disc, reg, Options{
		ScanInterval: time.Minute, RegistryInterval: time.Hour, RegistryTimeout: 50 * time.Millisecond,
		Concurrency: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	done := make(chan struct{})
	go func() { s.Cycle(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cycle did not honour the registry timeout")
	}
	if v, _ := info(t, s.Snapshot(), "slow"); v != 0 {
		t.Error("timed out check reported as success")
	}

	// Cancellation: Run returns promptly and publishes nothing.
	s2 := New(disc, reg, Options{
		ScanInterval: time.Minute, RegistryInterval: time.Hour, RegistryTimeout: time.Minute,
		Concurrency: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { s2.Run(ctx); close(stopped) }()
	for reg.inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if s2.Snapshot() != nil {
		t.Error("canceled cycle published a snapshot")
	}
	if s2.cache.Len() != 0 {
		t.Error("canceled check was cached as a failure")
	}
}

func TestRunScansPeriodically(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{tags: map[string][]string{"docker.io/library/nginx": {"1.26.2"}}}
	disc.set([]discovery.Instance{inst(t, "web", "nginx:1.26.2")}, nil)
	s := New(disc, reg, Options{
		ScanInterval: 10 * time.Millisecond, RegistryInterval: time.Hour, RegistryTimeout: time.Second,
		Concurrency: 2, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for s.Snapshot() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no snapshot")
		}
		time.Sleep(time.Millisecond)
	}
	// A new container appears and is picked up by a later scan while the
	// snapshot is read concurrently.
	disc.set([]discovery.Instance{inst(t, "web", "nginx:1.26.2"), inst(t, "web2", "nginx:1.26.2")}, nil)
	for len(s.Snapshot().Info) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("new container not discovered")
		}
		time.Sleep(time.Millisecond)
	}
	if got := reg.listCalls.Load(); got != 1 {
		t.Errorf("listings = %d, want 1", got)
	}
}

func TestKeychainIsForwarded(t *testing.T) {
	disc := &fakeDiscoverer{}
	reg := &fakeRegistry{tags: map[string][]string{"ghcr.io/org/app": {"1.0.0"}}}
	kc := authn.NewMultiKeychain()
	a := inst(t, "a", "ghcr.io/org/app:1.0.0")
	b := inst(t, "b", "ghcr.io/org/app:1.0.0")
	b.Keychain = kc // only the second instance has pull secrets
	disc.set([]discovery.Instance{a, b}, nil)

	s := newScanner(disc, reg, newClock(), 1)
	s.Cycle(context.Background())
	if len(reg.keychains) != 1 || reg.keychains[0] != kc {
		t.Errorf("keychains = %v, want the workload keychain", reg.keychains)
	}
}

func TestTargetKey(t *testing.T) {
	a := inst(t, "a", "nginx:latest", digestA, digestB)
	b := inst(t, "b", "nginx:latest", digestB, digestA)
	c := inst(t, "c", "nginx:latest", digestC)
	keys := map[string]bool{}
	for _, i := range []discovery.Instance{a, b, c} {
		keys[targetKeyFor(i)] = true
	}
	if len(keys) != 2 {
		t.Errorf("keys = %v, want digest order to be irrelevant and digests to matter", keys)
	}
}
