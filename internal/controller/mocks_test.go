package controller

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/utils"
)

// Compile-time conformance. Do NOT embed utils.HelmClientInterface here: an
// embedded (nil) interface satisfies the contract even when a method is
// missing, so adding a method to the interface would surface as a runtime nil
// dereference instead of a build failure.
var _ utils.HelmClientInterface = (*MockHelmClient)(nil)

// MockHelmClient is reached from several goroutines at once: reconciles run
// their Helm work in detached goroutines (runAsyncHelmReconcile), a superseded
// generation can still be in flight while its successor starts, and Ginkgo
// Eventually closures poll the recorded state while all of that runs. Every
// field is therefore guarded, and readers get copies -- returning the live map
// or slice would just move the race into the caller.
type MockHelmClient struct {
	mu sync.Mutex

	installedCharts   []string
	uninstalledCharts []string
	chartValues       map[string]any
	chartVersions     map[string]string
	// releaseLabels models Helm's per-release custom labels: they are
	// persisted onto the storage Secret, merged (not replaced) on upgrade,
	// and read back by List for selector matching. Keyed by release name.
	releaseLabels map[string]map[string]string

	failList bool
	// failUninstall names releases whose Uninstall returns an error.
	failUninstall map[string]struct{}
	simulateDelay time.Duration
	failInstall   bool
	// failInstallCount makes the next N calls fail and then succeed.
	// Independent of failInstall (which is permanent). Used to model a
	// transient failure followed by recovery.
	failInstallCount int
	failMessage      string
}

// NewMockHelmClient returns a mock pre-seeded with the given release labels,
// modelling releases that already exist in the cluster.
func NewMockHelmClient(releaseLabels map[string]map[string]string) *MockHelmClient {
	m := &MockHelmClient{releaseLabels: map[string]map[string]string{}}
	for name, lbls := range releaseLabels {
		m.releaseLabels[name] = maps.Clone(lbls)
	}
	return m
}

func (m *MockHelmClient) InstallOrUpgrade(spec *utils.ChartSpec) error {
	m.mu.Lock()
	delay := m.simulateDelay
	m.mu.Unlock()

	// Slept outside the lock so a simulated slow render actually overlaps with
	// other callers -- holding the lock here would serialize them and hide the
	// very concurrency the delay exists to produce.
	if delay > 0 {
		time.Sleep(delay)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failInstall {
		return fmt.Errorf("%s", m.failMessage)
	}

	if m.failInstallCount > 0 {
		m.failInstallCount--
		return fmt.Errorf("%s", m.failMessage)
	}

	m.installedCharts = append(m.installedCharts, spec.ReleaseName)

	if m.chartValues == nil {
		m.chartValues = make(map[string]any)
	}
	m.chartValues[spec.ReleaseName] = spec.Values

	if m.chartVersions == nil {
		m.chartVersions = make(map[string]string)
	}
	m.chartVersions[spec.ReleaseName] = spec.Version

	if m.releaseLabels == nil {
		m.releaseLabels = make(map[string]map[string]string)
	}
	if m.releaseLabels[spec.ReleaseName] == nil {
		m.releaseLabels[spec.ReleaseName] = make(map[string]string)
	}
	// Helm merges custom labels on upgrade rather than replacing them.
	maps.Copy(m.releaseLabels[spec.ReleaseName], spec.Labels)

	return nil
}

func (m *MockHelmClient) Uninstall(releaseName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, fails := m.failUninstall[releaseName]; fails {
		// Models a real uninstall failure: Helm uses foreground propagation
		// with a 5-minute timeout, so a resource held by a finalizer makes the
		// call return an error with the release still present.
		return fmt.Errorf("failed to uninstall release %s: timed out waiting for deletion", releaseName)
	}

	m.uninstalledCharts = append(m.uninstalledCharts, releaseName)
	delete(m.releaseLabels, releaseName)
	return nil
}

// ListReleases mirrors Helm's List+Selector: a release matches only when it
// carries every requested label.
func (m *MockHelmClient) ListReleases(matchLabels map[string]string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failList {
		return nil, fmt.Errorf("failed to list releases")
	}

	names := []string{}
	for name, lbls := range m.releaseLabels {
		matched := true
		for k, v := range matchLabels {
			if lbls[k] != v {
				matched = false
				break
			}
		}
		if matched {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func (m *MockHelmClient) AddRepository(name, url string) error {
	return nil
}

// --- accessors: every one returns a copy ---

func (m *MockHelmClient) Installed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.installedCharts)
}

func (m *MockHelmClient) Uninstalled() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.uninstalledCharts)
}

func (m *MockHelmClient) ValuesFor(releaseName string) (any, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.chartValues[releaseName]
	return v, ok
}

func (m *MockHelmClient) VersionFor(releaseName string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chartVersions[releaseName]
}

func (m *MockHelmClient) LabelsFor(releaseName string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.releaseLabels[releaseName])
}

// --- fault injection: safe to call while reconciles are in flight ---

func (m *MockHelmClient) SetFailInstall(fail bool, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failInstall = fail
	m.failMessage = message
}

func (m *MockHelmClient) SetFailInstallCount(n int, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failInstallCount = n
	m.failMessage = message
}

// RemainingInstallFailures reports how much of the transient-failure budget is
// left, so a test can assert it was consumed exactly once.
func (m *MockHelmClient) RemainingInstallFailures() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failInstallCount
}

// SetFailUninstall makes Uninstall fail for the named releases, modelling a
// release whose teardown is blocked (e.g. a PVC held by a terminating pod).
func (m *MockHelmClient) SetFailUninstall(releaseNames ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failUninstall == nil {
		m.failUninstall = make(map[string]struct{}, len(releaseNames))
	}
	for _, n := range releaseNames {
		m.failUninstall[n] = struct{}{}
	}
}

func (m *MockHelmClient) SetFailList(fail bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failList = fail
}

func (m *MockHelmClient) SetSimulateDelay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.simulateDelay = d
}
