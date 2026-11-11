package utils

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
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
	rspb "helm.sh/helm/v4/pkg/release/v1"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/rest"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"oras.land/oras-go/v2/registry/remote/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
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

// HelmClientInterface defines the operations for Helm management. Every
// operation acts as the namespace's installer (see NewHelmClient).
type HelmClientInterface interface {
	InstallOrUpgrade(spec *ChartSpec) error
	ListReleases(matchLabels map[string]string) ([]string, error)
	ReleaseStatus(releaseName string) (string, error)
	DropClusterRBACFromRecord(releaseName string) ([]string, error)
	Uninstall(releaseName string) error
}

// HelmClient is a wrapper around helm v4 SDK to simplify Helm operations.
type HelmClient struct {
	// settings carries what Helm reads beyond the cluster connection --
	// registry, repository and cache paths, history and rate limits. Its
	// kube fields (KUBECONFIG, HELM_KUBE*) are never read: the cluster
	// connection is restConfig, through a restConfigGetter.
	settings     *cli.EnvSettings
	actionConfig *action.Configuration
	// getter is the connection actionConfig acts through, impersonating the
	// namespace's installer.
	getter    *restConfigGetter
	log       logr.Logger
	namespace string
	mu        sync.RWMutex
	secrets   kdexv1alpha1.Secrets
	// maxFetchBytes overrides RegistryFetchMaxBytes when positive.
	maxFetchBytes int64
	// metaReader lists release Secrets as metadata only (see ListReleases).
	metaReader ctrlclient.Reader
}

// HelmClientOption configures a HelmClient.
type HelmClientOption func(*HelmClient)

// WithRegistryFetchMaxBytes bounds the bytes one chart operation may read from
// a registry, in place of RegistryFetchMaxBytes.
func WithRegistryFetchMaxBytes(n int64) HelmClientOption {
	return func(h *HelmClient) { h.maxFetchBytes = n }
}

// WithMetadataReader sets the reader ListReleases uses to list Helm's release
// Secrets as metadata only, so the operator never holds a release's payload
// (the chart, its values, its manifest) in memory just to learn its name.
// Give it an uncached reader: a cached one would start an informer holding
// every Secret's metadata in the namespace-wide fleet.
func WithMetadataReader(r ctrlclient.Reader) HelmClientOption {
	return func(h *HelmClient) { h.metaReader = r }
}

// InstallerServiceAccountName is the per-namespace ServiceAccount nexus
// impersonates to install a host's charts. It must equal the host-manager
// constant of the same name (kdex-host-manager
// internal/controller/reserved_service_accounts.go).
const InstallerServiceAccountName = "kdex-installer"

// InstallerUser is the impersonation user for a namespace's installer.
func InstallerUser(namespace string) string {
	return "system:serviceaccount:" + namespace + ":" + InstallerServiceAccountName
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

// NewHelmClient creates a new HelmClient for the given namespace, acting on
// the cluster restConfig connects to -- the operator's own config: in-cluster
// in a pod, envtest's in a test -- always impersonating the namespace's
// installer (InstallerUser), whatever restConfig impersonated. There is no
// other identity: a release record is a Secret whoever writes Secrets in the
// namespace can forge, and Helm acts on what it lists, so no Helm action may
// run with more than the installer's namespace-bounded rights
// (kdex-tech/nexus-manager#46). restConfig is copied, never changed.
//
// The connection is never Helm's own kubeconfig loading (cli.EnvSettings'
// RESTClientGetter). Given an impersonation override, client-go's deferred
// loader treats the merged config as non-default and skips its in-cluster
// fallback, so in a pod -- no kubeconfig file -- an impersonating client
// targeted http://localhost:8080 (client-go v0.35.2,
// tools/clientcmd/merged_client_builder.go; kubectl --as in a pod fails the
// same way). Nor would its environment (KUBECONFIG, HELM_KUBEASUSER,
// HELM_KUBEAPISERVER, ...) be a connection the operator should take.
func NewHelmClient(
	namespace string,
	restConfig *rest.Config,
	secrets kdexv1alpha1.Secrets,
	logger logr.Logger,
	opts ...HelmClientOption,
) (*HelmClient, error) {
	if restConfig == nil {
		return nil, errors.New("failed to create helm client: no REST config")
	}
	settings := cli.New()
	settings.SetNamespace(namespace)

	h := &HelmClient{
		log:       logger,
		namespace: namespace,
		secrets:   secrets,
		settings:  settings,
	}
	// Options may touch settings, which Init reads: apply them first.
	for _, opt := range opts {
		opt(h)
	}

	actionConfig := action.NewConfiguration(
		action.ConfigurationSetLogger(logr.ToSlogHandler(logger)))

	// Use secret driver by default
	helmDriver := os.Getenv("HELM_DRIVER")
	if helmDriver == "" {
		helmDriver = "secret"
	}

	h.getter = newRESTConfigGetter(restConfig, namespace, InstallerUser(namespace), settings)
	if err := actionConfig.Init(
		h.getter,
		namespace,
		helmDriver,
	); err != nil {
		return nil, fmt.Errorf("failed to init action config: %w", err)
	}

	h.actionConfig = actionConfig
	return h, nil
}

// freshen makes the operation starting now see the cluster as it is now.
// A client is cached per host and identity and serves operation after
// operation on one action configuration, which keeps what it learns: the
// Capabilities a chart renders against (.Capabilities.APIVersions), read once
// and kept, and the discovery the REST mappings come from. Left alone, a kind
// that appeared after the client's first operation -- a CRD installed since
// -- would be missing from both until the operator restarted, and every later
// install or upgrade of a chart using it would fail with "no matches for
// kind". The caller holds h.mu, which every reader of Capabilities holds.
//
// Still needed through client-go v0.37.2 / Helm v4.3.1 (checked 2026-11-11):
// Helm caches Capabilities per action.Configuration, and nothing in it
// refreshes them or the memory-cached discovery between operations.
func (h *HelmClient) freshen() {
	h.actionConfig.Capabilities = nil
	h.getter.invalidateDiscovery()
}

// InstallOrUpgrade installs or upgrades a Helm chart.
func (h *HelmClient) InstallOrUpgrade(spec *ChartSpec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.freshen()

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
// carry every one of matchLabels. Releases in any state are returned -- a
// release wedged in pending-upgrade is still one we own and must be able to
// clean up.
//
// Helm stores each revision of a release as a Secret labelled owner=helm and
// name=<release>, plus the labels the release was installed with. Listing
// those as metadata reads no release payload, where Helm's own list action
// decodes every revision's chart, values and manifest. There is no fallback to
// that: without a metadata reader this is an error.
func (h *HelmClient) ListReleases(matchLabels map[string]string) ([]string, error) {
	if h.metaReader == nil {
		return nil, errors.New("failed to list releases: no metadata reader configured (WithMetadataReader)")
	}

	sel := labels.Set{"owner": "helm"}
	for k, v := range matchLabels {
		sel[k] = v
	}
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := h.metaReader.List(context.Background(), list,
		ctrlclient.InNamespace(h.namespace), ctrlclient.MatchingLabels(sel)); err != nil {
		return nil, fmt.Errorf("failed to list releases: %w", err)
	}

	// One Secret per revision: collapse to release names.
	seen := make(map[string]struct{}, len(list.Items))
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		name := list.Items[i].Labels["name"]
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	slices.Sort(names)
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

// latestRevision reads the release's latest revision from storage, nil for a
// release that does not exist.
func (h *HelmClient) latestRevision(releaseName string) (*rspb.Release, error) {
	last, err := h.actionConfig.Releases.Last(releaseName)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read the record of release %s: %w", releaseName, err)
	}
	switch rel := last.(type) {
	case *rspb.Release:
		return rel, nil
	case rspb.Release:
		return &rel, nil
	default:
		return nil, fmt.Errorf("failed to read the record of release %s: unsupported release type %T", releaseName, last)
	}
}

// ReleaseStatus is the status of the release's latest revision ("deployed",
// "failed", "pending-upgrade", ...), or "" for a release that does not exist.
// It reads storage without the client's lock, as Uninstall does: a read must
// not wait out an install in progress.
func (h *HelmClient) ReleaseStatus(releaseName string) (string, error) {
	rel, err := h.latestRevision(releaseName)
	if err != nil || rel == nil || rel.Info == nil {
		return "", err
	}
	return string(rel.Info.Status), nil
}

// DropClusterRBACFromRecord rewrites the manifest of the release's latest
// revision -- the record an uninstall acts on -- without its ClusterRole and
// ClusterRoleBinding documents and without any list-typed document, and
// returns what it dropped ("Kind/name"). A record with nothing to drop is not
// written; a release that does not exist has nothing to drop.
//
// Migration code: delete in release B (privilege-redesign follow-up issue).
// A pre-redesign host-manager release lists its legacy ClusterRole and
// ClusterRoleBinding, which the installer -- the only identity a HelmClient
// has -- is Forbidden to delete, so its uninstall would fail. nexus deletes
// the legacy objects itself (verified by UID and Helm ownership annotations,
// never from this record) and then drops them from the record here, so the
// installer's uninstall does not trip over what is already gone.
//
// This is no authorization: the record is a Secret whoever writes Secrets in
// the namespace can forge, before or after this rewrite, and the uninstall
// that follows can do only what the installer may. List-typed documents go
// too because Helm's uninstall flattens them -- a document whose top-level
// kind is ClusterRole can carry an items list of anything -- and nothing a
// host-manager chart renders is one. Every other document stays: one the
// installer cannot delete (a cluster-scoped kind, another namespace) fails the
// uninstall closed, which keeps the host's finalizer for an administrator.
// Each document is classified with the unstructured decoder Helm's resource
// builder uses; one that does not decode is kept, and fails the uninstall's
// build in turn.
func (h *HelmClient) DropClusterRBACFromRecord(releaseName string) ([]string, error) {
	rel, err := h.latestRevision(releaseName)
	if err != nil || rel == nil {
		return nil, err
	}
	docs := releaseutil.SplitManifests(rel.Manifest)
	keys := slices.Collect(maps.Keys(docs))
	sort.Sort(releaseutil.BySplitManifestsOrder(keys))

	var kept strings.Builder
	var dropped []string
	for _, k := range keys {
		if what, drop := clusterRBACOrList(docs[k]); drop {
			dropped = append(dropped, what)
			continue
		}
		kept.WriteString("---\n")
		kept.WriteString(docs[k])
		kept.WriteString("\n")
	}
	if len(dropped) == 0 {
		return nil, nil
	}
	rel.Manifest = kept.String()
	if err := h.actionConfig.Releases.Update(rel); err != nil {
		return nil, fmt.Errorf("failed to rewrite the record of release %s: %w", releaseName, err)
	}
	return dropped, nil
}

// clusterRBACOrList decodes one manifest document as Helm's resource builder
// does and reports whether DropClusterRBACFromRecord drops it, naming it when
// it does.
func clusterRBACOrList(doc string) (string, bool) {
	raw, err := yaml.YAMLToJSON([]byte(doc))
	if err != nil {
		return "", false
	}
	obj, gvk, err := unstructured.UnstructuredJSONScheme.Decode(raw, nil, nil)
	if err != nil || gvk == nil {
		return "", false
	}
	name := ""
	if o, ok := obj.(metav1.Object); ok {
		name = o.GetName()
	}
	what := gvk.Kind + "/" + name
	switch {
	case meta.IsListType(obj), strings.HasSuffix(gvk.Kind, "List"):
		return what, true
	case gvk.Group == rbacv1.GroupName && (gvk.Kind == "ClusterRole" || gvk.Kind == "ClusterRoleBinding"):
		return what, true
	}
	return "", false
}

// Uninstall uninstalls a Helm release, running none of its hooks. A release
// that does not exist is not an error.
//
// Helm keeps a release's record -- hooks included -- in a Secret in the
// release's namespace, so whoever can write Secrets there can forge the record.
// Uninstall acts as the namespace's installer, which bounds what a forged
// record can delete; running its delete hooks would still create whatever the
// last writer of that Secret put in them. None of the releases this operator
// manages (the host-manager chart, the companion charts) relies on a delete
// hook. See kdex-tech/nexus-manager#46.
func (h *HelmClient) Uninstall(releaseName string) error {
	// Discovery only, and without h.mu: an uninstall reads no Capabilities,
	// and a teardown that has stopped waiting for a run still inside an
	// uncancellable Helm call must not queue behind it.
	h.getter.invalidateDiscovery()
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
