package cache

import (
	"sync"
	"testing"
	"time"
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

func TestSuccessIsRefreshedAfterTTL(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := New(time.Hour, clk.Now)

	if !c.Due("a") {
		t.Fatal("missing key must be due")
	}
	c.Put("a", Result{OK: true, LatestTag: "1.2.3"})
	if c.Due("a") {
		t.Error("fresh success must not be due")
	}
	clk.Advance(53 * time.Minute) // below TTL minus 10% jitter
	if c.Due("a") {
		t.Error("success due before TTL")
	}
	clk.Advance(8 * time.Minute)
	if !c.Due("a") {
		t.Error("success not due after TTL")
	}
	got, ok := c.Get("a")
	if !ok || !got.OK || got.LatestTag != "1.2.3" {
		t.Errorf("Get = %+v, %v", got, ok)
	}
}

func TestFailureBackoffIsBoundedAndNotPermanent(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := New(10*time.Minute, clk.Now)

	// Expected delays: 1m, 2m, 4m, 8m, then capped at the 10m TTL.
	for i, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute} {
		c.Put("a", Result{Reason: "boom"})
		if c.Due("a") {
			t.Fatalf("failure %d: due immediately", i+1)
		}
		clk.Advance(delay*89/100 - time.Second)
		if c.Due("a") {
			t.Fatalf("failure %d: due before backoff of %s", i+1, delay)
		}
		clk.Advance(delay*11/100 + 2*time.Second)
		if !c.Due("a") {
			t.Fatalf("failure %d: not due after backoff of %s", i+1, delay)
		}
	}

	// A success resets the failure streak.
	c.Put("a", Result{OK: true})
	c.Put("a", Result{Reason: "boom"})
	clk.Advance(time.Minute + time.Second)
	if !c.Due("a") {
		t.Error("backoff not reset after a success")
	}
}

func TestFailureReplacesSuccess(t *testing.T) {
	c := New(time.Hour, nil)
	c.Put("a", Result{OK: true, LatestTag: "2.0.0", UpdateAvailable: true})
	c.Put("a", Result{Reason: "rate limited"})
	got, _ := c.Get("a")
	if got.OK || got.LatestTag != "" || got.UpdateAvailable {
		t.Errorf("stale success survived: %+v", got)
	}
}

func TestBackoffNeverExceedsShortTTL(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := New(10*time.Second, clk.Now)
	c.Put("a", Result{Reason: "boom"})
	clk.Advance(11 * time.Second)
	if !c.Due("a") {
		t.Error("failure backoff exceeded the TTL")
	}
}

func TestRetain(t *testing.T) {
	c := New(time.Hour, nil)
	c.Put("a", Result{OK: true})
	c.Put("b", Result{OK: true})
	c.Retain(map[string]struct{}{"b": {}})
	if _, ok := c.Get("a"); ok {
		t.Error("a not pruned")
	}
	if _, ok := c.Get("b"); !ok {
		t.Error("b pruned")
	}
	if c.Len() != 1 {
		t.Errorf("Len = %d", c.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := New(time.Hour, nil)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				key := string(rune('a' + (i+j)%5))
				c.Put(key, Result{OK: j%2 == 0})
				c.Get(key)
				c.Due(key)
				c.Retain(map[string]struct{}{key: {}})
			}
		}()
	}
	wg.Wait()
}
