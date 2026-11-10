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

	"github.com/Masterminds/semver/v3"
	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/kube"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/release"
	v1 "helm.sh/helm/v4/pkg/repo/v1"
	"helm.sh/helm/v4/pkg/storage/driver"
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
	// may make -- counted as the registry client makes them, so a request
	// the transport retries counts once. A per-request timeout does not bound
	// a fetch: resolving an unpinned or ranged version lists the repository's
	// tags, page after page, each page a fast request, for as long as the
	// registry hands out another Link -- and holds the render slot throughout.
	RegistryFetchBudget      = 5 * time.Minute
	RegistryFetchMaxRequests = 500

	// RegistryFetchMaxBytes is the default bound on the bytes one chart
	// operation may read from a registry, across every response: tag
	// listings, manifests, tokens and blobs alike. Time and request count do
	// not bound memory: to resolve a version range Helm keeps every semver tag
	// of every page, and it pulls a chart's blobs into memory whatever size
	// they declare. One budget for all of it, because the registry writes
	// every URL the client follows -- each page's Link, each redirect -- so no
	// path tells a blob from a tag page.
	//
	// Tag JSON is the worst case: measured, it retains about nine times its
	// size in heap (16MiB of it held ~147MiB), so 4MiB costs ~37MiB, well
	// inside the operator's memory limit. A real chart archive is far under
	// it: KDex's own host-manager chart is tens of KiB, and nearly every
	// public chart is under 1MiB compressed. The manager's
	// --registry-fetch-max-bytes sets it; raising it needs the memory limit
	// raised to match. Separately, oras-go v2.7.0 preallocates up to 32MiB
	// per blob from the size its manifest declares, before reading a byte
	// this budget counts, and copies up to three blobs at once: forged sizes
	// cost up to ~96MiB transiently, then fail.
	RegistryFetchMaxBytes = 4 << 20

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
	// maxFetchBytes overrides RegistryFetchMaxBytes when positive.
	maxFetchBytes int64
}

// HelmClientOption configures a HelmClient.
type HelmClientOption func(*HelmClient)

// WithRegistryFetchMaxBytes bounds the bytes one chart operation may read from
// a registry, in place of RegistryFetchMaxBytes.
func WithRegistryFetchMaxBytes(n int64) HelmClientOption {
	return func(h *HelmClient) { h.maxFetchBytes = n }
}

// registryLimits is defaultRegistryFetchLimits, with the client's byte budget.
func (h *HelmClient) registryLimits() registryFetchLimits {
	limits := defaultRegistryFetchLimits
	if h.maxFetchBytes > 0 {
		limits.maxBytes = h.maxFetchBytes
	}
	return limits
}

var _ HelmClientInterface = (*HelmClient)(nil)

// NewHelmClient creates a new HelmClient for the given namespace.
func NewHelmClient(
	namespace string,
	secrets kdexv1alpha1.Secrets,
	logger logr.Logger,
	opts ...HelmClientOption,
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

	h := &HelmClient{
		actionConfig: actionConfig,
		log:          logger,
		namespace:    namespace,
		secrets:      secrets,
		settings:     settings,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
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
//
// Only a chart named by an exact version is cached. An empty version, a range
// or any other tag names whatever the repository resolves it to now, which may
// be a new chart tomorrow: cached, it would resolve once per process. An exact
// version re-pushed under the same tag is still cached; that part of #50 is
// open.
func (h *HelmClient) locateChart(spec *ChartSpec, load func() (string, error)) (string, error) {
	if _, err := semver.StrictNewVersion(spec.Version); err != nil {
		return load()
	}
	key := chartPathKey(spec.ChartName, spec.Version, HashSecrets(h.secrets))
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
	client.Timeout = HelmCallTimeout
	client.DeletionPropagation = "foreground"
	client.WaitStrategy = kube.StatusWatcherStrategy

	_, err := client.Run(releaseName)
	// A release that does not exist is uninstalled. Without KeepHistory a
	// successful uninstall purges the release, so a teardown retrying after
	// another release failed would otherwise fail on this one -- already gone
	// -- every time, and hold the host's finalizer forever.
	//
	// Not by Uninstall.IgnoreNotFound: in Helm v4.1.3 (and v4.3.1, the
	// latest) it returns success for ANY error looking up the release's
	// history -- throttling, an unavailable API server, a timeout -- and
	// success here lets teardown drop the finalizer, or a prune drop the
	// release from the record, while the release lives on. Run wraps the
	// lookup's error, so only a genuine ErrReleaseNotFound is let through.
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to uninstall release %s: %w", releaseName, err)
	}
	return nil
}

func (h *HelmClient) registryBind(spec *ChartSpec) error {
	options, err := registryClientOptions(spec.ChartName, h.secrets, h.settings.Debug, h.registryLimits())
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
	maxBytes       int64
}

var defaultRegistryFetchLimits = registryFetchLimits{
	requestTimeout: ChartFetchTimeout,
	budget:         RegistryFetchBudget,
	maxRequests:    RegistryFetchMaxRequests,
	maxBytes:       RegistryFetchMaxBytes,
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
// (v2.7.0 here, the latest) caps neither: Repository.TagListMaxPages exists
// but defaults to no cap and Helm, which builds the Repository, does not set
// it, and MaxMetadataBytes bounds a single page only. No upgrade of either
// bounds the listing. Hence a budget for the whole fetch on the transport.
// Delete it once Helm gives a registry client a timeout and a bounded tag
// listing.
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
		// registry, the credentialed ones included. oras-go v2.7.0 drops
		// the Authorization header on any cross-origin hop (scheme or port
		// included) and re-authenticates only after a same-host http:80 to
		// https:443 upgrade, so a credentialed plainHTTP registry may
		// redirect only that way.
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

// registryTransport is the transport registry.NewTransport (Helm v4.1.3)
// builds -- a clone of http.DefaultTransport, wrapped for retries and, when
// debug, for logging -- with TLS verification off when insecure. It is a
// workaround: Helm v4.1.3 through v4.3.1 has no ClientOpt for unverified TLS,
// so an insecure registry needs a transport of our own, and this one keeps
// what NewTransport gives every other registry. Delete it once Helm has one.
//
// An insecure (or plain-HTTP) registry is reached directly, as it always was,
// not through a proxy taken from the environment: that proxy would carry the
// registry's credentials in the clear, and an in-cluster registry missing
// from NO_PROXY would become unreachable.
func registryTransport(debug, insecure bool) http.RoundTripper {
	if !insecure {
		return registry.NewTransport(debug)
	}
	t, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Only if something replaced http.DefaultTransport. The zero
		// transport works, but sets no dial, TLS-handshake or idle timeout of
		// its own: only the client's per-request timeout and the fetch's
		// deadline bound it.
		t = &http.Transport{}
	}
	t = t.Clone()
	t.Proxy = nil
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the registry's secret asks for it
	var base http.RoundTripper = t
	if debug {
		base = &registry.LoggingTransport{RoundTripper: base}
	}
	return retry.NewTransport(base)
}

// errFetchBudget ends a registry fetch that has spent its budget.
var errFetchBudget = errors.New("registry fetch budget exhausted")

// fetchBudget refuses every request once the fetch it belongs to has made
// maxRequests of them, cuts off any request at the fetch's deadline, and fails
// any read past maxBytes across all of the fetch's responses. One is made per
// registry client, and registryBind makes a client per chart operation, so the
// budget is one operation's.
type fetchBudget struct {
	next     http.RoundTripper
	deadline time.Time
	limits   registryFetchLimits
	made     atomic.Int64
	read     atomic.Int64
}

func newFetchBudget(next http.RoundTripper, limits registryFetchLimits) *fetchBudget {
	return &fetchBudget{
		next:     next,
		deadline: time.Now().Add(limits.budget),
		limits:   limits,
	}
}

func (b *fetchBudget) RoundTrip(req *http.Request) (*http.Response, error) {
	if n := b.made.Add(1); n > int64(b.limits.maxRequests) {
		return nil, fmt.Errorf("%w: more than %d requests", errFetchBudget, b.limits.maxRequests)
	}
	if !time.Now().Before(b.deadline) {
		return nil, fmt.Errorf("%w: more than %s", errFetchBudget, b.limits.budget)
	}
	ctx, cancel := context.WithDeadline(req.Context(), b.deadline)
	resp, err := b.next.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &budgetedBody{ReadCloser: resp.Body, cancel: cancel, budget: b}
	return resp, nil
}

// budgetedBody counts what is read of a response against its fetch's byte
// budget, and releases the response's deadline once it is closed.
type budgetedBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	budget *fetchBudget
}

func (b *budgetedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if limit := b.budget.limits.maxBytes; b.budget.read.Add(int64(n)) > limit {
		return n, fmt.Errorf("%w: more than %d bytes read from the registry; a chart that needs more is let through "+
			"by raising the manager's --registry-fetch-max-bytes (chart value "+
			"controllerManager.container.registryFetchMaxBytes), and its memory limit with it", errFetchBudget, limit)
	}
	return n, err
}

func (b *budgetedBody) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
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

	// Locate the chart. LocateChart re-downloads the archive on every call,
	// and the chart is identical across hosts, so an exact version's located
	// path is cached and shared fleet-wide (see locateChart). loader.Load
	// still runs per render: Helm mutates the chart while rendering, so each
	// render needs its own copy.
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

	// Locate the chart. LocateChart re-downloads the archive on every call,
	// and the chart is identical across hosts, so an exact version's located
	// path is cached and shared fleet-wide (see locateChart). loader.Load
	// still runs per render: Helm mutates the chart while rendering, so each
	// render needs its own copy.
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
