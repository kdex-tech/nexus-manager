package utils

import "sync"

// defaultChartPathCacheSize bounds how many (chart, version, credential)
// combinations are remembered. A fleet renders a handful of chart versions at
// a time, so this is generous; the cap exists so a long-lived operator rolling
// through versions cannot accumulate entries without limit.
const defaultChartPathCacheSize = 32

// chartPathCache remembers where a located chart archive landed on disk.
//
// LocateChart performs a full registry pull of the archive on EVERY Helm
// operation, and the host-manager chart is byte-identical across the fleet, so
// a fleet-wide re-render pays one download per host for the same artifact.
//
// It deliberately caches the PATH, not the loaded *chart.Chart. Helm mutates
// the chart during a render -- ProcessDependencies calls SetDependencies and
// rewrites dependency metadata, and which dependencies survive depends on the
// values of THAT render -- so a shared chart object would let one host's values
// alter what another host renders. Caching the path keeps the expensive
// download shared while each render still gets its own mutable chart from
// loader.Load.
type chartPathCache struct {
	mu    sync.RWMutex
	paths map[string]string
	max   int
}

func newChartPathCache(max int) *chartPathCache {
	return &chartPathCache{paths: make(map[string]string), max: max}
}

// sharedChartPaths is deliberately package-level. A HelmClient is created per
// host, so a per-client cache would never see the repeat it exists to catch --
// the repeat is across hosts rendering the same chart.
var sharedChartPaths = newChartPathCache(defaultChartPathCacheSize)

// chartPathKey scopes a cached archive to the credentials that fetched it.
// Registry credentials are resolved per host, so keying on chart+version alone
// would let a host render an artifact it could not have pulled itself. Hosts
// sharing credentials -- the common case, cluster-wide defaults -- still share
// the download, which is where the fleet-scale win comes from.
func chartPathKey(chartName, version, credentialHash string) string {
	return chartName + "@" + version + "#" + credentialHash
}

// get returns the cached path for key, calling load exactly once per key on a
// miss. A failed load is not cached: remembering it would wedge every later
// render of that chart behind a single registry blip.
func (c *chartPathCache) get(key string, load func() (string, error)) (string, error) {
	c.mu.RLock()
	path, ok := c.paths[key]
	c.mu.RUnlock()
	if ok {
		return path, nil
	}

	path, err := load()
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict rather than grow without bound. Entries are interchangeable
	// pointers to on-disk archives, so which one goes costs only a re-download
	// on the next touch.
	if len(c.paths) >= c.max {
		for k := range c.paths {
			delete(c.paths, k)
			if len(c.paths) < c.max {
				break
			}
		}
	}
	c.paths[key] = path

	return path, nil
}

func (c *chartPathCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.paths)
}
