package utils

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/release"
	v1 "helm.sh/helm/v4/pkg/repo/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	trueVal = "true"

	// defaultMaxHistory bounds retained Helm release revisions per release.
	// Matches the Helm CLI's own --history-max default, which the SDK does not
	// apply on its own.
	defaultMaxHistory = 10

	// HelmCallTimeout bounds what a Helm install, upgrade or uninstall waits
	// for once its chart is in hand: hooks, and an uninstall's foreground
	// deletion.
	HelmCallTimeout = 5 * time.Minute

	// RegistryFetchBudget bounds how long one chart operation may spend on
	// registry requests altogether, and RegistryFetchMaxRequests how many it
	// may make. A per-request timeout does not bound a fetch: resolving an
	// unpinned or ranged version lists the repository's tags, page after
	// page, each page a fast request, for as long as the registry hands out
	// another Link -- and holds the render slot throughout.
	RegistryFetchBudget      = 5 * time.Minute
	RegistryFetchMaxRequests = 500

	// ChartFetchTimeout bounds every request to a chart registry, as Helm's
	// own getter.DefaultHTTPTimeout bounds every request to a classic
	// (http/https) chart repository. A chart reference is tenant input, so a
	// registry that accepts the connection and never answers must not hold
	// the helm operation -- its render slot, and a teardown waiting on it --
	// indefinitely.
	ChartFetchTimeout = getter.DefaultHTTPTimeout * time.Second
)

// ChartSpec defines the parameters for a Helm chart installation or upgrade.
type ChartSpec struct {
	ReleaseName string
	ChartName   string
	Namespace   string
	Values      map[string]any
	Version     string
	UpgradeCRDs bool
	// Labels are custom labels stamped on the Helm release. Helm persists
	// them on the release's storage Secret and merges them on upgrade, so
	// they survive as a durable, queryable ownership marker.
	Labels map[string]string
	// Proceed, when not nil, is asked once the chart is in hand -- after a
	// fetch that can take as long as the registry makes it -- and before
	// anything is installed or upgraded. Its error ends the call unchanged.
	Proceed func() error
}

// HelmClientInterface defines the operations for Helm management.
type HelmClientInterface interface {
	AddRepository(name, repo string) error
	InstallOrUpgrade(spec *ChartSpec) error
	ListReleases(matchLabels map[string]string) ([]string, error)
	Uninstall(releaseName string) error
}

// HelmClient is a wrapper around helm v4 SDK to simplify Helm operations.
type HelmClient struct {
	settings     *cli.EnvSettings
	actionConfig *action.Configuration
	log          logr.Logger
	namespace    string
	mu           sync.RWMutex
	secrets      kdexv1alpha1.Secrets
}

var _ HelmClientInterface = (*HelmClient)(nil)

// NewHelmClient creates a new HelmClient for the given namespace.
func NewHelmClient(
	namespace string,
	secrets kdexv1alpha1.Secrets,
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

// ListReleases returns the names of releases in this client's namespace that
// carry every one of matchLabels. Releases in any state are returned — a
// release wedged in pending-upgrade is still one we own and must be able to
// clean up.
func (h *HelmClient) ListReleases(matchLabels map[string]string) ([]string, error) {
	client := action.NewList(h.actionConfig)
	client.StateMask = action.ListAll
	client.Selector = labels.Set(matchLabels).String()

	releases, err := client.Run()
	if err != nil {
		return nil, fmt.Errorf("failed to list releases: %w", err)
	}

	names := make([]string, 0, len(releases))
	for _, rel := range releases {
		accessor, err := release.NewAccessor(rel)
		if err != nil {
			return nil, fmt.Errorf("failed to read release: %w", err)
		}
		names = append(names, accessor.Name())
	}
	return names, nil
}

// locateChart resolves the chart archive's path, reusing a fleet-shared cache
// so the same artifact is not re-downloaded once per host. The key includes a
// hash of this client's resolved secrets so a cached archive is never reused
// across a credential boundary.
func (h *HelmClient) locateChart(spec *ChartSpec, load func() (string, error)) (string, error) {
	key := chartPathKey(spec.ChartName, spec.Version, Hash(h.secrets))
	return sharedChartPaths.get(key, load)
}

// Uninstall uninstalls a Helm release, running none of its hooks. A release
// that does not exist is not an error.
//
// Helm keeps a release's record -- hooks included -- in a Secret in the
// release's namespace, so whoever can write Secrets there can forge the record.
// Uninstall acts as the operator, whose RBAC reaches cluster-scoped resources,
// so running a release's delete hooks would create whatever the last writer of
// that Secret put in them. None of the releases this operator manages (the
// host-manager chart, the companion charts) relies on a delete hook.
// See kdex-tech/nexus-manager#46.
func (h *HelmClient) Uninstall(releaseName string) error {
	client := action.NewUninstall(h.actionConfig)
	client.DisableHooks = true
	// A release that does not exist is uninstalled. Without KeepHistory a
	// successful uninstall purges the release, so a teardown retrying after
	// another release failed would otherwise fail on this one -- already gone
	// -- every time, and hold the host's finalizer forever.
	client.IgnoreNotFound = true
	client.Timeout = HelmCallTimeout
	client.DeletionPropagation = "foreground"
	client.WaitStrategy = kube.StatusWatcherStrategy

	_, err := client.Run(releaseName)
	if err != nil {
		return fmt.Errorf("failed to uninstall release %s: %w", releaseName, err)
	}
	return nil
}

func (h *HelmClient) registryBind(spec *ChartSpec) error {
	options, err := registryClientOptions(spec.ChartName, h.secrets, h.settings.Debug, defaultRegistryFetchLimits)
	if err != nil {
		return err
	}

	regClient, err := registry.NewClient(options...)
	if err != nil {
		return fmt.Errorf("failed to create registry client: %w", err)
	}
	h.actionConfig.RegistryClient = regClient

	return nil
}

// registryFetchLimits bounds one chart operation's traffic to a registry.
type registryFetchLimits struct {
	requestTimeout time.Duration
	budget         time.Duration
	maxRequests    int
}

var defaultRegistryFetchLimits = registryFetchLimits{
	requestTimeout: ChartFetchTimeout,
	budget:         RegistryFetchBudget,
	maxRequests:    RegistryFetchMaxRequests,
}

// registryClientOptions configures the registry client for chartName's
// registry, with the credentials and transport settings of the Helm secret
// that matches it, within limits.
//
// The limits are ours to set. Helm v4 (pinned at v4.1.3; unchanged through
// v4.3.1, the latest) builds a registry client's default http.Client without a
// Timeout and has no option for one, and LocateChart hands the OCI getter this
// client rather than one built from its own getter.WithTimeout default. Nor
// does a timeout per request bound a fetch: Client.Tags lists tags with
// context.Background() and follows every page's Link, and the oras-go it uses
// (v2.6.0 here) caps neither. oras-go v2.7.0 adds Repository.TagListMaxPages,
// but it defaults to no cap and Helm, which builds the Repository, does not
// set it, so no upgrade of either bounds the listing. Hence a budget for the
// whole fetch on the transport. Delete it once Helm gives a registry client a
// timeout and a bounded tag listing.
func registryClientOptions(chartName string, secrets kdexv1alpha1.Secrets, debug bool, limits registryFetchLimits) ([]registry.ClientOption, error) {
	options := []registry.ClientOption{
		registry.ClientOptDebug(debug),
		registry.ClientOptEnableCache(true),
	}

	reg := chartName
	idx := strings.Index(reg, "//")

	if idx > -1 {
		reg = reg[idx:]
	}

	if !strings.HasPrefix(reg, "//") {
		reg = "//" + reg
	}
	registryURL, err := url.Parse(reg)
	if err != nil {
		return nil, err
	}

	if registryURL.Host == "" {
		return nil, fmt.Errorf("could not identify host from chartName: %s", chartName)
	}

	reg = fmt.Sprintf("%s%s", registryURL.Host, registryURL.Path)

	match := secrets.Find(func(s corev1.Secret) bool {
		if s.Annotations["kdex.dev/secret-type"] != "helm" {
			return false
		}

		repo := string(s.Data["repository"])
		// Strip any scheme from the secret repository for matching
		if idx := strings.Index(repo, "//"); idx > -1 {
			repo = repo[idx+2:]
		}
		return registryMatches(reg, repo)
	})

	insecure := false
	if match != nil {
		if string(match.Data["plainHTTP"]) == trueVal {
			options = append(options, registry.ClientOptPlainHTTP())
		}
		// A plainHTTP registry also gets TLS unverified, for a registry that
		// redirects to https. Either setting applies to every request to the
		// registry, the credentialed ones included.
		insecure = string(match.Data["plainHTTP"]) == trueVal || string(match.Data["insecure"]) == trueVal

		if len(match.Data["username"]) > 0 && len(match.Data["password"]) > 0 {
			// Helm's own authorizer: it sends through the http.Client below,
			// with Helm's User-Agent and token cache.
			options = append(options, registry.ClientOptBasicAuth(
				string(match.Data["username"]), string(match.Data["password"])))
		}
	}
	options = append(options, registry.ClientOptHTTPClient(&http.Client{
		Transport: newFetchBudget(registryTransport(debug, insecure), limits),
		Timeout:   limits.requestTimeout,
	}))

	return options, nil
}

// registryTransport is the transport registry.NewTransport builds -- a clone
// of http.DefaultTransport, so proxy-from-environment and its dial and idle
// timeouts are kept, wrapped for retries and, when debug, for logging -- with
// TLS verification off when insecure.
func registryTransport(debug, insecure bool) http.RoundTripper {
	if !insecure {
		return registry.NewTransport(debug)
	}
	var base http.RoundTripper
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t = t.Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the registry's secret asks for it
		base = t
	} else {
		base = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // as above
	}
	if debug {
		base = &registry.LoggingTransport{RoundTripper: base}
	}
	return retry.NewTransport(base)
}

// errFetchBudget ends a registry fetch that has spent its budget.
var errFetchBudget = errors.New("registry fetch budget exhausted")

// fetchBudget refuses every request once the fetch it belongs to has made
// maxRequests of them, and cuts off any request at the fetch's deadline. One is
// made per registry client, and registryBind makes a client per chart
// operation, so the budget is one operation's.
type fetchBudget struct {
	next        http.RoundTripper
	deadline    time.Time
	budget      time.Duration
	maxRequests int64
	made        atomic.Int64
}

func newFetchBudget(next http.RoundTripper, limits registryFetchLimits) *fetchBudget {
	return &fetchBudget{
		next:        next,
		deadline:    time.Now().Add(limits.budget),
		budget:      limits.budget,
		maxRequests: int64(limits.maxRequests),
	}
}

func (b *fetchBudget) RoundTrip(req *http.Request) (*http.Response, error) {
	if n := b.made.Add(1); n > b.maxRequests {
		return nil, fmt.Errorf("%w: more than %d requests", errFetchBudget, b.maxRequests)
	}
	if !time.Now().Before(b.deadline) {
		return nil, fmt.Errorf("%w: more than %s", errFetchBudget, b.budget)
	}
	ctx, cancel := context.WithDeadline(req.Context(), b.deadline)
	resp, err := b.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnClose releases a response's deadline once its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	defer c.cancel()
	return c.ReadCloser.Close()
}

// registryMatches reports whether a chart registry reference (host[/path], with
// any scheme already stripped) belongs to the registry identified by a Helm
// secret's repository value (also host[/path], scheme stripped).
//
// The match is anchored on a path-segment boundary so a credential is only
// offered to the registry it was issued for. A plain strings.HasPrefix would
// leak credentials to look-alike hosts (e.g. secret "myregistry.io" matching a
// chart at "myregistry.io.attacker.com/..."), and an empty repository would
// match every registry. Both are rejected here.
func registryMatches(reg, repo string) bool {
	if repo == "" {
		return false
	}
	if reg == repo {
		return true
	}
	// reg must extend repo at a path boundary, i.e. reg == repo + "/" + ...
	return strings.HasPrefix(reg, repo+"/")
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
	// HookOnly, not a full rollout wait. The wait is taken while holding the
	// fleet-wide render slot, and that slot exists to bound MEMORY (concurrent
	// chart loads) -- charging a minutes-long wait against it prices a parked
	// goroutine as if it were an active render, and serializes the whole fleet
	// behind one slow rollout.
	//
	// Nothing is lost by not waiting here: the reconciler already observes the
	// host-manager Deployment and drives Progressing/Ready/Degraded from
	// DeploymentAvailable (kdexhost_controller.go, readiness check 2). That
	// check is level-triggered and has no deadline, so it is strictly better
	// than this wait was -- a rollout healthy at six minutes was reported as a
	// FAILURE by the 5-minute wait while the watch reports it correctly.
	//
	// Hooks are still waited on, and a failed apply (invalid manifest, quota,
	// failed hook) still errors from Run. Only "did the pods become ready"
	// moves, and it moves to the mechanism that already owned it.
	client.WaitStrategy = kube.HookOnlyStrategy
	client.Timeout = HelmCallTimeout
	client.Labels = spec.Labels

	if spec.Version != "" {
		client.Version = spec.Version
	}

	// Locate the chart. LocateChart re-downloads the archive on every call, and
	// the chart is identical across hosts, so the located path is cached and
	// shared fleet-wide. loader.Load still runs per render: Helm mutates the
	// chart while rendering, so each render needs its own copy.
	cp, err := h.locateChart(spec, func() (string, error) {
		return client.LocateChart(spec.ChartName, h.settings)
	})
	if err != nil {
		return fmt.Errorf("failed to locate chart %s: %w", spec.ChartName, err)
	}

	// Load the chart
	chartRequested, err := loader.Load(cp)
	if err != nil {
		return fmt.Errorf("failed to load chart %s: %w", spec.ChartName, err)
	}

	if err := proceed(spec); err != nil {
		return err
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
	// HookOnly, not a full rollout wait. The wait is taken while holding the
	// fleet-wide render slot, and that slot exists to bound MEMORY (concurrent
	// chart loads) -- charging a minutes-long wait against it prices a parked
	// goroutine as if it were an active render, and serializes the whole fleet
	// behind one slow rollout.
	//
	// Nothing is lost by not waiting here: the reconciler already observes the
	// host-manager Deployment and drives Progressing/Ready/Degraded from
	// DeploymentAvailable (kdexhost_controller.go, readiness check 2). That
	// check is level-triggered and has no deadline, so it is strictly better
	// than this wait was -- a rollout healthy at six minutes was reported as a
	// FAILURE by the 5-minute wait while the watch reports it correctly.
	//
	// Hooks are still waited on, and a failed apply (invalid manifest, quota,
	// failed hook) still errors from Run. Only "did the pods become ready"
	// moves, and it moves to the mechanism that already owned it.
	client.WaitStrategy = kube.HookOnlyStrategy
	client.Timeout = HelmCallTimeout
	client.Labels = spec.Labels
	// The Helm SDK default is UNLIMITED history -- `--history-max=10` is a CLI
	// default that does not reach the SDK. Left unset, every reconcile-driven
	// upgrade adds a release Secret that is never pruned, and a cluster-config
	// edit re-renders the whole fleet at once (#27), so the count grows with
	// cluster age rather than fleet size. Those Secrets are then held by the
	// operator's cache for the process lifetime (#45).
	client.MaxHistory = defaultMaxHistory

	if spec.Version != "" {
		client.Version = spec.Version
	}

	// Locate the chart. LocateChart re-downloads the archive on every call, and
	// the chart is identical across hosts, so the located path is cached and
	// shared fleet-wide. loader.Load still runs per render: Helm mutates the
	// chart while rendering, so each render needs its own copy.
	cp, err := h.locateChart(spec, func() (string, error) {
		return client.LocateChart(spec.ChartName, h.settings)
	})
	if err != nil {
		return fmt.Errorf("failed to locate chart %s: %w", spec.ChartName, err)
	}

	// Load the chart
	chartRequested, err := loader.Load(cp)
	if err != nil {
		return fmt.Errorf("failed to load chart %s: %w", spec.ChartName, err)
	}

	if err := proceed(spec); err != nil {
		return err
	}

	// Execute the action
	_, err = client.Run(spec.ReleaseName, chartRequested, spec.Values)
	if err != nil {
		return fmt.Errorf("failed to upgrade chart %s: %w", spec.ChartName, err)
	}

	return nil
}

// proceed asks spec.Proceed, if set, whether to go on now that the chart is in
// hand.
func proceed(spec *ChartSpec) error {
	if spec.Proceed == nil {
		return nil
	}
	return spec.Proceed()
}
