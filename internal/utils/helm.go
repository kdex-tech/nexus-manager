package utils

import (
	"context"
	"fmt"

	helmclient "github.com/mittwald/go-helm-client"
	"helm.sh/helm/v3/pkg/repo"
)

// HelmClientInterface defines the operations for Helm management.
type HelmClientInterface interface {
	InstallOrUpgrade(ctx context.Context, spec *helmclient.ChartSpec) error
	Uninstall(releaseName string) error
	AddRepository(name, url string) error
}

// HelmClient is a wrapper around go-helm-client to simplify Helm operations.
type HelmClient struct {
	client helmclient.Client
}

var _ HelmClientInterface = (*HelmClient)(nil)

// NewHelmClient creates a new HelmClient for the given namespace.
func NewHelmClient(namespace string) (*HelmClient, error) {
	opt := &helmclient.Options{
		Namespace:        namespace,
		RepositoryCache:  "/tmp/.helmcache",
		RepositoryConfig: "/tmp/.helmrepo",
		Debug:            true,
		Linting:          true,
	}

	client, err := helmclient.New(opt)
	if err != nil {
		return nil, fmt.Errorf("failed to create helm client: %w", err)
	}

	return &HelmClient{
		client: client,
	}, nil
}

// InstallOrUpgrade installs or upgrades a Helm chart.
func (h *HelmClient) InstallOrUpgrade(ctx context.Context, spec *helmclient.ChartSpec) error {
	_, err := h.client.InstallOrUpgradeChart(ctx, spec, nil)
	if err != nil {
		return fmt.Errorf("failed to install or upgrade chart %s: %w", spec.ChartName, err)
	}
	return nil
}

// Uninstall uninstalls a Helm release.
func (h *HelmClient) Uninstall(releaseName string) error {
	err := h.client.UninstallReleaseByName(releaseName)
	if err != nil {
		return fmt.Errorf("failed to uninstall release %s: %w", releaseName, err)
	}
	return nil
}

// AddRepository adds a Helm repository.
func (h *HelmClient) AddRepository(name, url string) error {
	chartRepo := repo.Entry{
		Name: name,
		URL:  url,
	}

	if err := h.client.AddOrUpdateChartRepo(chartRepo); err != nil {
		return fmt.Errorf("failed to add repository %s: %w", name, err)
	}
	return nil
}
