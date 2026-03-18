package utils

import (
	"context"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/registry"
	v1 "helm.sh/helm/v4/pkg/repo/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ChartSpec defines the parameters for a Helm chart installation or upgrade.
type ChartSpec struct {
	ReleaseName string
	ChartName   string
	Namespace   string
	Values      map[string]any
	Version     string
	Wait        bool
	UpgradeCRDs bool
}

// HelmClientInterface defines the operations for Helm management.
type HelmClientInterface interface {
	InstallOrUpgrade(ctx context.Context, spec *ChartSpec) error
	Uninstall(releaseName string) error
	AddRepository(name, url string) error
}

// HelmClient is a wrapper around helm v4 SDK to simplify Helm operations.
type HelmClient struct {
	settings     *cli.EnvSettings
	actionConfig *action.Configuration
	namespace    string
}

var _ HelmClientInterface = (*HelmClient)(nil)

// NewHelmClient creates a new HelmClient for the given namespace.
func NewHelmClient(namespace string) (*HelmClient, error) {
	settings := cli.New()

	actionConfig := action.NewConfiguration(
		action.ConfigurationSetLogger(
			logr.ToSlogHandler(logf.Log.WithName("helm"))))

	// Use secret driver by default
	helmDriver := os.Getenv("HELM_DRIVER")
	if helmDriver == "" {
		helmDriver = "secret"
	}

	if err := actionConfig.Init(
		settings.RESTClientGetter(),
		namespace,
		helmDriver,
	); err != nil {
		return nil, fmt.Errorf("failed to init action config: %w", err)
	}

	regClient, err := registry.NewClient(
		registry.ClientOptDebug(settings.Debug),
		registry.ClientOptEnableCache(true),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create registry client: %w", err)
	}
	actionConfig.RegistryClient = regClient

	return &HelmClient{
		settings:     settings,
		actionConfig: actionConfig,
		namespace:    namespace,
	}, nil
}

// InstallOrUpgrade installs or upgrades a Helm chart.
func (h *HelmClient) InstallOrUpgrade(ctx context.Context, spec *ChartSpec) error {
	// Use Upgrade action with Install=true for "upgrade --install" behavior
	client := action.NewUpgrade(h.actionConfig)
	client.Install = true
	client.Namespace = spec.Namespace
	client.Version = spec.Version

	// Helm 4 uses ServerSideApply as a string
	client.ServerSideApply = "true"

	// Locate the chart
	cp, err := client.LocateChart(spec.ChartName, h.settings)
	if err != nil {
		return fmt.Errorf("failed to locate chart %s: %w", spec.ChartName, err)
	}

	// Load the chart
	chartRequested, err := loader.Load(cp)
	if err != nil {
		return fmt.Errorf("failed to load chart %s: %w", spec.ChartName, err)
	}

	// Check values against the schema before running the action
	_, err = util.CoalesceValues(chartRequested, spec.Values)
	if err != nil {
		return fmt.Errorf("failed to compute values: %w", err)
	}

	// Execute the action (Run does not take context in Helm 4)
	_, err = client.Run(spec.ReleaseName, chartRequested, spec.Values)
	if err != nil {
		return fmt.Errorf("failed to install or upgrade chart %s: %w", spec.ChartName, err)
	}

	return nil
}

// Uninstall uninstalls a Helm release.
func (h *HelmClient) Uninstall(releaseName string) error {
	client := action.NewUninstall(h.actionConfig)
	_, err := client.Run(releaseName)
	if err != nil {
		return fmt.Errorf("failed to uninstall release %s: %w", releaseName, err)
	}
	return nil
}

// AddRepository adds a Helm repository.
func (h *HelmClient) AddRepository(name, url string) error {
	// Helm v4 still uses repositories.yaml for non-OCI charts.
	f, err := v1.LoadFile(h.settings.RepositoryConfig)
	if err != nil {
		if os.IsNotExist(err) {
			f = v1.NewFile()
		} else {
			return fmt.Errorf("failed to load repository file: %w", err)
		}
	}

	if f.Has(name) {
		// Already exists
		return nil
	}

	c := v1.Entry{
		Name: name,
		URL:  url,
	}

	f.Add(&c)

	if err := f.WriteFile(h.settings.RepositoryConfig, 0644); err != nil {
		return fmt.Errorf("failed to write repository file: %w", err)
	}

	return nil
}
