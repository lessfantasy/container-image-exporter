// Package scanner periodically discovers running images, checks them
// against their registries with a bounded worker pool and publishes an
// immutable metrics snapshot.
package scanner

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/container-image-exporter/container-image-exporter/internal/cache"
	"github.com/container-image-exporter/container-image-exporter/internal/discovery"
	"github.com/container-image-exporter/container-image-exporter/internal/metrics"
	"github.com/container-image-exporter/container-image-exporter/internal/registry"
	"github.com/container-image-exporter/container-image-exporter/internal/version"
)

// StaleFactor controls how long the last known inventory is reused while
// discovery fails: StaleFactor x scan interval. After that the exporter
// stops serving metrics instead of presenting stale data as current.
const StaleFactor = 3

// Options configure a Scanner.
type Options struct {
	ScanInterval     time.Duration
	RegistryInterval time.Duration
	RegistryTimeout  time.Duration
	Concurrency      int
	Logger           *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Scanner owns the scan loop. Create it with New and call Run once.
type Scanner struct {
	disc   discovery.Discoverer
	reg    Registry
	opts   Options
	cache  *cache.Cache
	limits *rateLimits
	logger *slog.Logger
	now    func() time.Time

	// snapshot is nil until the first complete cycle and whenever the
	// inventory is unknown.
	snapshot atomic.Pointer[metrics.Snapshot]

	// Only touched by the scan loop goroutine.
	inventory     []discovery.Instance
	lastDiscovery time.Time
	haveInventory bool
}

// New creates a Scanner.
func New(disc discovery.Discoverer, reg Registry, opts Options) *Scanner {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	return &Scanner{
		disc:   disc,
		reg:    reg,
		opts:   opts,
		cache:  cache.New(opts.RegistryInterval, opts.Now),
		limits: newRateLimits(opts.Now),
		logger: opts.Logger,
		now:    opts.Now,
	}
}

// Snapshot returns the current metrics snapshot, or nil when no reliable
// snapshot exists (startup, or discovery failing for too long). It never
// blocks and performs no I/O.
func (s *Scanner) Snapshot() *metrics.Snapshot { return s.snapshot.Load() }

// Run scans until ctx is canceled.
func (s *Scanner) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.Cycle(ctx)
		timer.Reset(s.opts.ScanInterval)
	}
}

// Cycle runs one discovery and check pass. Exported for tests.
func (s *Scanner) Cycle(ctx context.Context) {
	instances, ok := s.discover(ctx)
	if !ok {
		return
	}

	// Plan: one target per distinct registry lookup.
	type plan struct {
		key    string       // empty when no registry lookup is possible
		direct cache.Result // result when key is empty
	}
	plans := make([]plan, len(instances))
	targets := map[string]*target{}
	keep := map[string]struct{}{}
	for i, inst := range instances {
		if inst.ImageErr != nil {
			plans[i].direct = cache.Result{Reason: "image reference cannot be resolved"}
			continue
		}
		if inst.Image.Tag == "" {
			plans[i].direct = cache.Result{Reason: "image is referenced by digest without a tag"}
			continue
		}
		det := version.Detect(inst.Image.Tag)
		if det.Strategy == version.Unsupported {
			plans[i].direct = cache.Result{Reason: "unsupported tag: " + det.Reason}
			continue
		}
		key := targetKey(inst.Image, det, inst.Digests)
		plans[i].key = key
		keep[key] = struct{}{}
		if t, exists := targets[key]; exists {
			if t.keychain == nil {
				t.keychain = inst.Keychain
			}
			continue
		}
		targets[key] = &target{key: key, ref: inst.Image, detection: det, digests: inst.Digests, keychain: inst.Keychain}
	}

	var due []*target
	for key, t := range targets {
		if s.cache.Due(key) {
			due = append(due, t)
		}
	}
	s.runChecks(ctx, due)
	if ctx.Err() != nil {
		return // shutting down: do not publish a half-finished cycle
	}
	s.cache.Retain(keep)

	observations := make([]metrics.Observation, 0, len(instances))
	for i, inst := range instances {
		result := plans[i].direct
		if plans[i].key != "" {
			cached, found := s.cache.Get(plans[i].key)
			if !found {
				continue
			}
			result = cached
		}
		obs := metrics.Observation{
			Platform:        inst.Platform,
			Container:       inst.Container,
			Namespace:       inst.Namespace,
			WorkloadKind:    inst.WorkloadKind,
			Registry:        inst.Image.Registry,
			Repository:      inst.Image.Repository,
			CurrentTag:      inst.Image.Tag,
			OK:              result.OK,
			UpdateAvailable: result.UpdateAvailable,
		}
		if inst.ImageErr != nil {
			obs.Repository = inst.RawImage
		}
		if result.OK {
			obs.LatestTag = result.LatestTag
		}
		observations = append(observations, obs)
	}
	s.snapshot.Store(metrics.Build(observations))
	s.logger.Debug("scan cycle finished", "instances", len(instances), "lookups", len(targets), "checked", len(due))
}

// discover returns the inventory to use for this cycle. On failure the
// previous inventory is reused for a limited time; after that, and before
// the first success, the snapshot is withdrawn so that an unknown
// inventory is never reported as an empty one.
func (s *Scanner) discover(ctx context.Context) ([]discovery.Instance, bool) {
	instances, err := s.disc.Discover(ctx)
	if ctx.Err() != nil {
		return nil, false
	}
	now := s.now()
	if err == nil {
		if !s.haveInventory {
			s.logger.Info("runtime discovery succeeded", "instances", len(instances))
		}
		s.inventory, s.lastDiscovery, s.haveInventory = instances, now, true
		return instances, true
	}

	age := now.Sub(s.lastDiscovery)
	if s.haveInventory && age <= StaleFactor*s.opts.ScanInterval {
		s.logger.Warn("runtime discovery failed, reusing last inventory", "error", err, "inventory_age", age.Round(time.Second))
		return s.inventory, true
	}
	s.logger.Error("runtime discovery failed, inventory unknown, metrics withdrawn", "error", err)
	s.haveInventory = false
	s.snapshot.Store(nil)
	return nil, false
}

// runChecks processes targets with a bounded worker pool.
func (s *Scanner) runChecks(ctx context.Context, targets []*target) {
	if len(targets) == 0 {
		return
	}
	tags := newTagLists(s.reg)
	jobs := make(chan *target)
	var wg sync.WaitGroup
	workers := min(s.opts.Concurrency, len(targets))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range jobs {
				s.checkOne(ctx, tags, t)
			}
		}()
	}
	for _, t := range targets {
		select {
		case jobs <- t:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
}

func (s *Scanner) checkOne(ctx context.Context, tags *tagLists, t *target) {
	if ctx.Err() != nil {
		return
	}
	host := t.ref.Registry
	if until, limited := s.limits.limited(host); limited {
		s.cache.Put(t.key, cache.Result{Reason: "rate limited"})
		s.logger.Debug("skipping check, registry is rate limited", "image", t.ref.String(), "retry_after", until.Sub(s.now()).Round(time.Second))
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, s.opts.RegistryTimeout)
	result, err := check(checkCtx, s.reg, tags, *t)
	cancel()
	if ctx.Err() != nil {
		return // canceled by shutdown, not a real failure
	}

	switch {
	case registry.IsRateLimited(err):
		pause := s.limits.hit(host)
		s.logger.Warn("registry rate limit reached, pausing checks", "registry", host, "pause", pause)
	case err == nil && result.OK:
		s.limits.clear(host)
	}

	s.cache.Put(t.key, result)
	if result.OK {
		s.logger.Debug("image checked", "image", t.ref.String(), "strategy", t.detection.Strategy.String(),
			"latest_tag", result.LatestTag, "update_available", result.UpdateAvailable)
		return
	}
	args := []any{"image", t.ref.String(), "strategy", t.detection.Strategy.String(), "reason", result.Reason}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	s.logger.Warn("image check failed", args...)
}

// rateLimits pauses checks per registry after a 429 response.
type rateLimits struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]rateLimit
}

type rateLimit struct {
	hits  int
	until time.Time
}

const (
	rateLimitMinPause = time.Minute
	rateLimitMaxPause = 30 * time.Minute
)

func newRateLimits(now func() time.Time) *rateLimits {
	return &rateLimits{now: now, m: map[string]rateLimit{}}
}

func (r *rateLimits) limited(host string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.m[host]
	return l.until, r.now().Before(l.until)
}

// hit records a 429 and returns the pause: 1m, 2m, 4m ... capped at 30m.
func (r *rateLimits) hit(host string) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.m[host]
	now := r.now()
	if now.Before(l.until) {
		return l.until.Sub(now) // already pausing; concurrent workers count once
	}
	pause := rateLimitMinPause
	for i := 0; i < l.hits && pause < rateLimitMaxPause; i++ {
		pause *= 2
	}
	pause = min(pause, rateLimitMaxPause)
	r.m[host] = rateLimit{hits: l.hits + 1, until: now.Add(pause)}
	return pause
}

func (r *rateLimits) clear(host string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, host)
}
