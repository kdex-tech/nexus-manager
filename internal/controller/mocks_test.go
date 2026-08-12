package controller

import (
	"fmt"
	"slices"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/utils"
)

type MockHelmClient struct {
	utils.HelmClientInterface
	InstalledCharts   []string
	UninstalledCharts []string
	ChartValues       map[string]any
	ChartVersions     map[string]string
	// ReleaseLabels models Helm's per-release custom labels: they are
	// persisted onto the storage Secret, merged (not replaced) on upgrade,
	// and read back by List for selector matching. Keyed by release name.
	ReleaseLabels map[string]map[string]string
	FailList      bool
	SimulateDelay time.Duration
	FailInstall   bool
	// FailInstallCount makes the next N calls fail and then succeed.
	// Independent of FailInstall (which is permanent). Used to model a
	// transient failure followed by recovery.
	FailInstallCount int
	FailMessage      string
}

func (m *MockHelmClient) InstallOrUpgrade(spec *utils.ChartSpec) error {
	if m.SimulateDelay > 0 {
		time.Sleep(m.SimulateDelay)
	}

	if m.FailInstall {
		return fmt.Errorf("%s", m.FailMessage)
	}

	if m.FailInstallCount > 0 {
		m.FailInstallCount--
		return fmt.Errorf("%s", m.FailMessage)
	}

	if m.InstalledCharts == nil {
		m.InstalledCharts = []string{}
	}
	m.InstalledCharts = append(m.InstalledCharts, spec.ReleaseName)

	if m.ChartValues == nil {
		m.ChartValues = make(map[string]any)
	}
	m.ChartValues[spec.ReleaseName] = spec.Values

	if m.ChartVersions == nil {
		m.ChartVersions = make(map[string]string)
	}
	m.ChartVersions[spec.ReleaseName] = spec.Version

	if m.ReleaseLabels == nil {
		m.ReleaseLabels = make(map[string]map[string]string)
	}
	if m.ReleaseLabels[spec.ReleaseName] == nil {
		m.ReleaseLabels[spec.ReleaseName] = make(map[string]string)
	}
	// Helm merges custom labels on upgrade rather than replacing them.
	for k, v := range spec.Labels {
		m.ReleaseLabels[spec.ReleaseName][k] = v
	}

	return nil
}

func (m *MockHelmClient) Uninstall(releaseName string) error {
	m.UninstalledCharts = append(m.UninstalledCharts, releaseName)
	delete(m.ReleaseLabels, releaseName)
	return nil
}

// ListReleases mirrors Helm's List+Selector: a release matches only when it
// carries every requested label.
func (m *MockHelmClient) ListReleases(matchLabels map[string]string) ([]string, error) {
	if m.FailList {
		return nil, fmt.Errorf("failed to list releases")
	}

	names := []string{}
	for name, lbls := range m.ReleaseLabels {
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
