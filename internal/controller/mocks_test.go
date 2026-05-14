package controller

import (
	"fmt"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/utils"
)

type MockHelmClient struct {
	utils.HelmClientInterface
	InstalledCharts   []string
	UninstalledCharts []string
	ChartValues       map[string]any
	ChartVersions     map[string]string
	SimulateDelay     time.Duration
	FailInstall       bool
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

	return nil
}

func (m *MockHelmClient) Uninstall(releaseName string) error {
	m.UninstalledCharts = append(m.UninstalledCharts, releaseName)
	return nil
}

func (m *MockHelmClient) AddRepository(name, url string) error {
	return nil
}
