// Package cache keeps registry check results in memory and decides when
// a result must be refreshed.
package cache

import (
	"math/rand/v2"
	"sync"
	"time"
)

// Result is the outcome of one registry check.
type Result struct {
	// OK is false when the check failed or its outcome is unknown.
	OK bool
	// LatestTag is the newest compatible tag. Empty when unknown.
	LatestTag string
	// UpdateAvailable is only meaningful when OK is true.
	UpdateAvailable bool
	// Reason describes a failure. It never contains credentials.
	Reason string
}

type entry struct {
	result   Result
	failures int
	next     time.Time
}

// Cache is a thread-safe result store.
//
// A successful result is refreshed after the TTL. A failed result is never
// kept for the TTL: it is retried after a bounded exponential backoff that
// starts at MinBackoff, doubles per consecutive failure and never exceeds
// the TTL. Both delays get up to 10% jitter so refreshes spread out.
type Cache struct {
	ttl        time.Duration
	minBackoff time.Duration
	now        func() time.Time

	mu      sync.Mutex
	entries map[string]entry
}

// MinBackoff is the first retry delay after a failed check.
const MinBackoff = time.Minute

// New creates a Cache. A nil clock uses time.Now.
func New(ttl time.Duration, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	minBackoff := MinBackoff
	if minBackoff > ttl {
		minBackoff = ttl
	}
	return &Cache{ttl: ttl, minBackoff: minBackoff, now: now, entries: map[string]entry{}}
}

// Get returns the stored result for key.
func (c *Cache) Get(key string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return e.result, ok
}

// Due reports whether key has no result or its result must be refreshed.
func (c *Cache) Due(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	return !ok || !c.now().Before(e.next)
}

// Put stores a result. A failure replaces an earlier success, so a stale
// success is never served as current.
func (c *Cache) Put(key string, r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := entry{result: r}
	delay := c.ttl
	if !r.OK {
		e.failures = c.entries[key].failures + 1
		delay = c.minBackoff
		for i := 1; i < e.failures && delay < c.ttl; i++ {
			delay *= 2
		}
		if delay > c.ttl {
			delay = c.ttl
		}
	}
	e.next = c.now().Add(jitter(delay))
	c.entries[key] = e
}

// Retain drops every entry whose key is not in keep.
func (c *Cache) Retain(keep map[string]struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if _, ok := keep[key]; !ok {
			delete(c.entries, key)
		}
	}
}

// Len returns the number of stored results.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// jitter shortens d by up to 10%.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - time.Duration(rand.Int64N(int64(d)/10+1))
}
