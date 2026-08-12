package utils

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// The whole point of the cache is that the chart is identical across hosts, so
// a repeat request must not re-run the download.
func TestChartPathCache_LoadsOncePerKey(t *testing.T) {
	c := newChartPathCache(8)
	var calls atomic.Int32

	load := func() (string, error) {
		calls.Add(1)
		return "/cache/host-manager-0.5.3.tgz", nil
	}

	for range 3 {
		got, err := c.get("host-manager@0.5.3", load)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got != "/cache/host-manager-0.5.3.tgz" {
			t.Fatalf("got %q", got)
		}
	}

	if n := calls.Load(); n != 1 {
		t.Errorf("download ran %d times, want 1", n)
	}
}

// A different chart version is a different artifact; serving the cached path
// for it would silently render the wrong chart, which is worse than a slow
// render.
func TestChartPathCache_SeparatesKeys(t *testing.T) {
	c := newChartPathCache(8)
	var calls atomic.Int32

	loadFor := func(path string) func() (string, error) {
		return func() (string, error) {
			calls.Add(1)
			return path, nil
		}
	}

	first, _ := c.get("host-manager@0.5.3", loadFor("/cache/a.tgz"))
	second, _ := c.get("host-manager@0.5.4", loadFor("/cache/b.tgz"))

	if first == second {
		t.Errorf("distinct versions shared a path: %q", first)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("download ran %d times, want 2", n)
	}
}

// A failed download must not be remembered, or one registry blip would wedge
// every later render of that chart behind a cached error.
func TestChartPathCache_DoesNotCacheFailures(t *testing.T) {
	c := newChartPathCache(8)
	var calls atomic.Int32

	failing := func() (string, error) {
		calls.Add(1)
		return "", fmt.Errorf("registry unavailable")
	}

	if _, err := c.get("host-manager@0.5.3", failing); err == nil {
		t.Fatal("expected an error from a failing download")
	}
	if _, err := c.get("host-manager@0.5.3", failing); err == nil {
		t.Fatal("expected an error from a failing download")
	}

	if n := calls.Load(); n != 2 {
		t.Errorf("download ran %d times, want 2 (failure must not be cached)", n)
	}
}

// Renders run concurrently once --helm-render-concurrency is raised above 1,
// which is the point of caching; the cache must not be the thing that breaks
// when it is.
func TestChartPathCache_ConcurrentGetsAreSafe(t *testing.T) {
	c := newChartPathCache(8)
	var wg sync.WaitGroup

	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("chart@%d", i%4)
			_, _ = c.get(key, func() (string, error) {
				return "/cache/" + key, nil
			})
		}(i)
	}
	wg.Wait()
}

// Unbounded caches are how operators leak memory across long uptimes; entries
// are cheap but must not accumulate without limit as chart versions roll.
func TestChartPathCache_EvictsBeyondCapacity(t *testing.T) {
	c := newChartPathCache(2)

	for i := range 5 {
		key := fmt.Sprintf("chart@%d", i)
		if _, err := c.get(key, func() (string, error) { return "/cache/" + key, nil }); err != nil {
			t.Fatalf("get: %v", err)
		}
	}

	if got := c.len(); got > 2 {
		t.Errorf("cache holds %d entries, want <= 2", got)
	}
}
