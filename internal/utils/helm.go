package utils

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	v1 "helm.sh/helm/v4/pkg/repo/v1"
	corev1 "k8s.io/api/core/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"oras.land/oras-go/v2/registry/remote/auth"
)

const (
	trueVal = "true"
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
	AddRepository(name, repo string) error
	InstallOrUpgrade(spec *ChartSpec) error
	Uninstall(releaseName string) error
}

// HelmClient is a wrapper around helm v4 SDK to simplify Helm operations.
type HelmClient struct {
	settings     *cli.EnvSettings
	actionConfig *action.Configuration
	log          logr.Logger
	namespace    string
	mu           sync.RWMutex
	secrets      kdexv1alpha1.ServiceAccountSecrets
}

var _ HelmClientInterface = (*HelmClient)(nil)

// NewHelmClient creates a new HelmClient for the given namespace.
func NewHelmClient(
	namespace string,
	secrets kdexv1alpha1.ServiceAccountSecrets,
	logger logr.Logger,
) (*HelmClient, error) {
	settings := cli.New()
	settings.SetNamespace(namespace)

	actionConfig := action.NewConfiguration(
		action.ConfigurationSetLogger(logr.ToSlogHandler(logger)))

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

	return &HelmClient{
		actionConfig: actionConfig,
		log:          logger,
		namespace:    namespace,
		secrets:      secrets,
		settings:     settings,
	}, nil
}

// AddRepository adds a Helm repository.
func (h *HelmClient) AddRepository(name, repo string) error {
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
		URL:  repo,
	}

	f.Add(&c)

	if err := f.WriteFile(h.settings.RepositoryConfig, 0644); err != nil {
		return fmt.Errorf("failed to write repository file: %w", err)
	}

	return nil
}

// InstallOrUpgrade installs or upgrades a Helm chart.
func (h *HelmClient) InstallOrUpgrade(spec *ChartSpec) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Bind the registry just in time
	err := h.registryBind(spec)
	if err != nil {
		return err
	}

	// Check if release exists
	exists, err := h.releaseExists(spec.ReleaseName)
	if err != nil {
		return fmt.Errorf("failed to check if release exists: %w", err)
	}

	if !exists {
		return h.install(spec)
	}
	return h.upgrade(spec)
}

func (h *HelmClient) ShowChart(spec *ChartSpec) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Bind the registry just in time
	err := h.registryBind(spec)
	if err != nil {
		return "", err
	}

	client := action.NewShow(action.ShowChart, h.actionConfig)

	if spec.Version != "" {
		client.Version = spec.Version
	}

	// Locate the chart
	cp, err := client.LocateChart(spec.ChartName, h.settings)
	if err != nil {
		return "", fmt.Errorf("failed to locate chart %s: %w", spec.ChartName, err)
	}

	return client.Run(cp)
}

// Uninstall uninstalls a Helm release.
func (h *HelmClient) Uninstall(releaseName string) error {
	client := action.NewUninstall(h.actionConfig)
	client.Timeout = 5 * time.Minute
	client.DeletionPropagation = "foreground"
	client.WaitStrategy = kube.StatusWatcherStrategy

	_, err := client.Run(releaseName)
	if err != nil {
		return fmt.Errorf("failed to uninstall release %s: %w", releaseName, err)
	}
	return nil
}

func (h *HelmClient) registryBind(spec *ChartSpec) error {
	options := []registry.ClientOption{
		registry.ClientOptDebug(h.settings.Debug),
		registry.ClientOptEnableCache(true),
	}

	reg := spec.ChartName
	idx := strings.Index(reg, "//")

	if idx > -1 {
		reg = reg[idx:]
	}

	if !strings.HasPrefix(reg, "//") {
		reg = "//" + reg
	}
	registryURL, err := url.Parse(reg)
	if err != nil {
		return err
	}

	if registryURL.Host == "" {
		return fmt.Errorf("could not identify host from chartName: %s", spec.ChartName)
	}

	reg = fmt.Sprintf("%s%s", registryURL.Host, registryURL.Path)

	match := h.secrets.Find(func(s corev1.Secret) bool {
		if s.Annotations["kdex.dev/secret-type"] != "helm" {
			return false
		}

		repo := string(s.Data["repository"])
		// Strip any scheme from the secret repository for matching
		if idx := strings.Index(repo, "//"); idx > -1 {
			repo = repo[idx+2:]
		}
		return strings.HasPrefix(reg, repo)
	})

	if match != nil {
		if string(match.Data["plainHTTP"]) == trueVal {
			options = append(options, registry.ClientOptPlainHTTP())
		}

		if string(match.Data["plainHTTP"]) == trueVal || string(match.Data["insecure"]) == trueVal {
			httpClient := &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: true,
					},
				},
			}
			options = append(options, registry.ClientOptHTTPClient(httpClient))
		}

		if len(match.Data["username"]) > 0 && len(match.Data["password"]) > 0 {
			options = append(options, registry.ClientOptAuthorizer(
				auth.Client{
					Credential: func(ctx context.Context, hostport string) (auth.Credential, error) {
						return auth.Credential{
							Password: string(match.Data["password"]),
							Username: string(match.Data["username"]),
						}, nil
					},
				},
			))
		}
	}

	regClient, err := registry.NewClient(options...)
	if err != nil {
		return fmt.Errorf("failed to create registry client: %w", err)
	}
	h.actionConfig.RegistryClient = regClient

	return nil
}

func (h *HelmClient) releaseExists(name string) (bool, error) {
	h.log.V(2).Info("releaseExists", "release", name)
	_, err := h.actionConfig.Releases.Last(name)
	if err != nil {
		errStr := err.Error()
		h.log.V(2).Info("releaseExists", "release", name, "err", errStr)
		if strings.Contains(errStr, "not found") || strings.Contains(errStr, "has no deployed releases") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (h *HelmClient) install(spec *ChartSpec) error {
	h.log.V(2).Info("install", "chartName", spec.ChartName, "namespace", spec.Namespace, "release", spec.ReleaseName, "version", spec.Version)
	client := action.NewInstall(h.actionConfig)
	client.ReleaseName = spec.ReleaseName
	client.Namespace = spec.Namespace
	client.CreateNamespace = false
	client.SkipCRDs = !spec.UpgradeCRDs
	client.WaitStrategy = "legacy"
	client.Timeout = 5 * time.Minute

	if spec.Version != "" {
		client.Version = spec.Version
	}

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

	// Execute the action
	_, err = client.Run(chartRequested, spec.Values)
	if err != nil {
		return fmt.Errorf("failed to install chart %s: %w", spec.ChartName, err)
	}

	return nil
}

func (h *HelmClient) upgrade(spec *ChartSpec) error {
	h.log.V(2).Info("upgrade", "chartName", spec.ChartName, "namespace", spec.Namespace, "release", spec.ReleaseName, "version", spec.Version)
	client := action.NewUpgrade(h.actionConfig)
	client.Namespace = spec.Namespace
	client.SkipCRDs = !spec.UpgradeCRDs
	client.WaitStrategy = "legacy"
	client.Timeout = 5 * time.Minute

	if spec.Version != "" {
		client.Version = spec.Version
	}

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

	// Execute the action
	_, err = client.Run(spec.ReleaseName, chartRequested, spec.Values)
	if err != nil {
		return fmt.Errorf("failed to upgrade chart %s: %w", spec.ChartName, err)
	}

	return nil
}
