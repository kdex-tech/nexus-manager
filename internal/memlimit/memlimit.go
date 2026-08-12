// Package memlimit derives a Go soft memory limit (GOMEMLIMIT) from the
// container's cgroup memory limit.
package memlimit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNoLimit reports that the cgroup imposes no memory limit, so there is
// nothing to derive a soft limit from.
var ErrNoLimit = errors.New("cgroup imposes no memory limit")

// ErrInvalidRatio reports a ratio outside (0, 1]. Unlike ErrNoLimit this is the
// operator's own misconfiguration, so the caller should fail loudly rather than
// start up quietly unprotected.
var ErrInvalidRatio = errors.New("invalid memory limit ratio")

// unlimitedV1 is the floor above which a cgroup v1 memory.limit_in_bytes means
// "unlimited". v1 has no "max" spelling: it reports PAGE_COUNTER_MAX, which is
// LONG_MAX rounded down to a page boundary and so varies with page size. Any
// value this large is an absence of a limit, not a limit.
const unlimitedV1 = int64(1) << 62

// Configure decides the soft memory limit to install. It reports apply=false
// when the caller must leave the Go runtime's own setting alone.
func Configure(root string, ratio float64, getenv func(string) string) (int64, bool, error) {
	if ratio <= 0 {
		// Explicitly disabled.
		return 0, false, nil
	}
	if ratio > 1 {
		return 0, false, fmt.Errorf("%w: %v must be > 0 and <= 1", ErrInvalidRatio, ratio)
	}

	if getenv("GOMEMLIMIT") != "" {
		// The runtime already applied it; deriving one would overrule it.
		return 0, false, nil
	}

	limit, err := FromCgroup(root, ratio)
	if err != nil {
		return 0, false, err
	}

	return limit, true, nil
}

// FromCgroup returns the soft memory limit to install, in bytes, derived from
// the cgroup memory limit visible under root by applying ratio. It reads the
// cgroup v2 interface first and falls back to v1.
func FromCgroup(root string, ratio float64) (int64, error) {
	limit, err := readCgroupLimit(root)
	if err != nil {
		return 0, err
	}

	return int64(float64(limit) * ratio), nil
}

// readCgroupLimit returns the raw cgroup memory limit in bytes.
func readCgroupLimit(root string) (int64, error) {
	base := filepath.Join(root, "sys", "fs", "cgroup")

	raw, err := os.ReadFile(filepath.Join(base, "memory.max"))
	if errors.Is(err, os.ErrNotExist) {
		raw, err = os.ReadFile(filepath.Join(base, "memory", "memory.limit_in_bytes"))
	}
	if errors.Is(err, os.ErrNotExist) {
		// Neither controller is mounted: not a container, or not Linux.
		return 0, ErrNoLimit
	}
	if err != nil {
		return 0, err
	}

	value := strings.TrimSpace(string(raw))
	if value == "max" {
		return 0, ErrNoLimit
	}

	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	if limit >= unlimitedV1 {
		return 0, ErrNoLimit
	}

	return limit, nil
}
