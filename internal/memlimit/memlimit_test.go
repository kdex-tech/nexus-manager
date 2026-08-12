package memlimit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeCgroupV2 lays out a cgroup v2 memory controller under root.
func writeCgroupV2(t *testing.T, root, contents string) {
	t.Helper()
	dir := filepath.Join(root, "sys", "fs", "cgroup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The whole point of the soft limit is headroom below the hard limit: GC has to
// push back BEFORE the cgroup OOMKills us, so the returned value must be
// strictly under what the container is actually allowed.
func TestFromCgroup_V2AppliesRatio(t *testing.T) {
	root := t.TempDir()
	writeCgroupV2(t, root, "1073741824\n") // 1Gi

	got, err := FromCgroup(root, 0.9)
	if err != nil {
		t.Fatalf("FromCgroup: %v", err)
	}

	if want := int64(966367641); got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

// noEnv stands in for an environment with nothing set.
func noEnv(string) string { return "" }

// The Go runtime already applies GOMEMLIMIT from the environment at startup.
// Deriving our own on top would silently overrule an operator who set it
// deliberately -- the one case where the human knows something we don't.
func TestConfigure_ExplicitEnvWins(t *testing.T) {
	root := t.TempDir()
	writeCgroupV2(t, root, "1073741824\n")

	env := func(key string) string {
		if key == "GOMEMLIMIT" {
			return "512MiB"
		}
		return ""
	}

	_, apply, err := Configure(root, 0.9, env)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}

	if apply {
		t.Error("apply = true, want false: derived limit would overrule the explicit GOMEMLIMIT")
	}
}

// A ratio of zero is the operator's off switch. It has to be a clean no-op, not
// an error, or the disabled path logs a failure every startup.
func TestConfigure_ZeroRatioDisables(t *testing.T) {
	root := t.TempDir()
	writeCgroupV2(t, root, "1073741824\n")

	_, apply, err := Configure(root, 0, noEnv)

	if err != nil {
		t.Errorf("got err %v, want nil: disabling is not a failure", err)
	}
	if apply {
		t.Error("apply = true, want false")
	}
}

// A soft limit at or above the hard limit is worse than none: GC pays the cost
// of backpressure that can never fire before the kernel kills the process. A
// typo like --memory-limit-ratio=90 must be rejected loudly at startup, not
// quietly honoured.
func TestConfigure_RatioAboveOneIsRejected(t *testing.T) {
	root := t.TempDir()
	writeCgroupV2(t, root, "1073741824\n")

	_, apply, err := Configure(root, 90, noEnv)

	if !errors.Is(err, ErrInvalidRatio) {
		t.Errorf("got err %v, want ErrInvalidRatio", err)
	}
	if apply {
		t.Error("apply = true, want false")
	}
}

// Misconfiguration and a plain absence of a cgroup limit both end with no soft
// limit installed, but only the first is the operator's mistake. main() exits on
// one and shrugs at the other, so they must not arrive as the same error.
func TestConfigure_NoLimitIsNotAnInvalidRatio(t *testing.T) {
	root := t.TempDir() // no cgroup at all

	_, _, err := Configure(root, 0.9, noEnv)

	if errors.Is(err, ErrInvalidRatio) {
		t.Errorf("got err %v, want ErrNoLimit: a missing cgroup would exit the operator", err)
	}
	if !errors.Is(err, ErrNoLimit) {
		t.Errorf("got err %v, want ErrNoLimit", err)
	}
}

// writeCgroupV1 lays out a cgroup v1 memory controller under root.
func writeCgroupV1(t *testing.T, root, contents string) {
	t.Helper()
	dir := filepath.Join(root, "sys", "fs", "cgroup", "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.limit_in_bytes"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// Nodes still running cgroup v1 expose the limit under a different path
// entirely. Reading only v2 would silently leave the operator unprotected on
// exactly the older clusters most likely to be memory-tight.
func TestFromCgroup_FallsBackToV1(t *testing.T) {
	root := t.TempDir()
	writeCgroupV1(t, root, "1073741824\n") // 1Gi, no memory.max present

	got, err := FromCgroup(root, 0.9)
	if err != nil {
		t.Fatalf("FromCgroup: %v", err)
	}

	if want := int64(966367641); got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}

// A hybrid host mounts both controllers, and only v2 carries the limit the
// kernel actually enforces there. Reading v1 would derive the limit from a
// controller that is not the one doing the killing.
func TestFromCgroup_PrefersV2OverV1(t *testing.T) {
	root := t.TempDir()
	writeCgroupV2(t, root, "1073741824\n") // 1Gi, enforced
	writeCgroupV1(t, root, "268435456\n")  // 256Mi, stale

	got, err := FromCgroup(root, 0.9)
	if err != nil {
		t.Fatalf("FromCgroup: %v", err)
	}

	if want := int64(966367641); got != want {
		t.Errorf("got %d, want %d: read the v1 limit instead of v2", got, want)
	}
}

// An unlimited cgroup writes the literal "max". Parsing that as a number and
// scaling it would install a nonsense soft limit; the caller must be told there
// is nothing to derive so it leaves the runtime default alone.
func TestFromCgroup_V2UnlimitedReportsNoLimit(t *testing.T) {
	root := t.TempDir()
	writeCgroupV2(t, root, "max\n")

	_, err := FromCgroup(root, 0.9)

	if !errors.Is(err, ErrNoLimit) {
		t.Errorf("got err %v, want ErrNoLimit", err)
	}
}

// `make run` against a kubeconfig, or any non-Linux dev machine, has no cgroup
// memory controller at all. That is the normal no-op case, not a failure, so it
// must arrive as the same sentinel the caller already ignores rather than as a
// bare ENOENT the caller would log as a problem.
func TestFromCgroup_NoCgroupReportsNoLimit(t *testing.T) {
	root := t.TempDir()

	_, err := FromCgroup(root, 0.9)

	if !errors.Is(err, ErrNoLimit) {
		t.Errorf("got err %v, want ErrNoLimit", err)
	}
}

// A zero limit parses cleanly and scales cleanly to zero, so without a lower
// guard it reaches debug.SetMemoryLimit(0) -- a permanent GC death spiral
// rather than the no-op every other "nothing to derive" case produces.
func TestFromCgroup_ZeroLimitReportsNoLimit(t *testing.T) {
	root := t.TempDir()
	writeCgroupV2(t, root, "0\n")

	_, err := FromCgroup(root, 0.9)

	if !errors.Is(err, ErrNoLimit) {
		t.Errorf("got err %v, want ErrNoLimit", err)
	}
}

// cgroup v1 has no "max" spelling: an unlimited controller reports
// PAGE_COUNTER_MAX, a number so large it parses cleanly and scales cleanly.
// Without an explicit guard the operator would announce a ~8 EiB soft limit and
// look protected while having no backpressure at all.
func TestFromCgroup_V1UnlimitedReportsNoLimit(t *testing.T) {
	root := t.TempDir()
	writeCgroupV1(t, root, "9223372036854771712\n")

	_, err := FromCgroup(root, 0.9)

	if !errors.Is(err, ErrNoLimit) {
		t.Errorf("got err %v, want ErrNoLimit", err)
	}
}
