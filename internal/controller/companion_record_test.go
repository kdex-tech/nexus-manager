package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// newHelmReconciler builds a reconciler over a fake cluster holding hosts,
// whose status writes pass through funcs when it is given.
func newHelmReconciler(t *testing.T, mock utils.HelmClientInterface, funcs *interceptor.Funcs, hosts ...*kdexv1alpha1.KDexHost) (*KDexHostReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := configuration.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, len(hosts))
	for _, h := range hosts {
		objs = append(objs, h)
	}
	// The indexes let a teardown, which lists a host's internal pages and
	// translations by them, run against this cluster too.
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&kdexv1alpha1.KDexHost{}).
		WithIndex(&kdexv1alpha1.KDexInternalUtilityPage{}, hostIndexKey, func(client.Object) []string { return nil }).
		WithIndex(&kdexv1alpha1.KDexInternalTranslation{}, hostIndexKey, func(client.Object) []string { return nil })
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	fc := b.Build()
	return &KDexHostReconciler{
		Client:                fc,
		ControllerID:          "test",
		Ctx:                   context.Background(),
		Configuration:         configuration.LoadConfiguration("/nonexistent-config.yaml", scheme),
		Scheme:                scheme,
		HelmRenderConcurrency: 1,
		HelmClientFactory: func(string, kdexv1alpha1.Secrets, logr.Logger) (utils.HelmClientInterface, error) {
			return mock, nil
		},
	}, fc
}

// getHost reads a host in "ns" back from the fake cluster.
func getHost(t *testing.T, c client.Client, name string) *kdexv1alpha1.KDexHost {
	t.Helper()
	got := &kdexv1alpha1.KDexHost{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// helmReconcile runs one asynchronous helm reconcile of host over a fake
// cluster holding hosts, and returns the host as it then stands.
func helmReconcile(t *testing.T, mock utils.HelmClientInterface, host string, hosts ...*kdexv1alpha1.KDexHost) *kdexv1alpha1.KDexHost {
	t.Helper()
	r, fc := newHelmReconciler(t, mock, nil, hosts...)
	r.runAsyncHelmReconcile(context.Background(), "ns", host, "hash", nil, logr.Discard())
	return getHost(t, fc, host)
}

func helmHost(name string, companions ...string) *kdexv1alpha1.KDexHost {
	h := hostWithCompanions(name, companions...)
	h.Spec.BrandName = "KDex Tech"
	h.Spec.Organization = "KDex Tech Inc."
	h.Spec.ModulePolicy = kdexv1alpha1.LooseModulePolicy
	return h
}

// The record is the authority a later prune or teardown acts on, so a
// reconcile records every companion it installed, and drops what it pruned.
func TestHelmReconcile_RecordsInstalledCompanionsAndForgetsPrunedOnes(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"dropped": {LabelCompanionOf: "my-host"},
	})
	host := recorded(helmHost("my-host", "b", "a"), "dropped")

	got := helmReconcile(t, mock, "my-host", host)

	if rec := got.Status.Attributes[AttributeCompanionReleases]; rec != "a,b" {
		t.Errorf("recorded companions = %q, want %q", rec, "a,b")
	}
	if !slices.Equal(mock.Uninstalled(), []string{"dropped"}) {
		t.Errorf("uninstalled = %v, want [dropped]", mock.Uninstalled())
	}
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusCompleted {
		t.Errorf("helm status = %q, want %q (%s)", s, HelmStatusCompleted, got.Status.Attributes[AttributeHelmReleaseError])
	}
}

// A companion named after another KDexHost would be installed over that host's
// own release. The reconcile refuses it, installs no companion, and says why.
func TestHelmReconcile_RefusesACompanionNamedAfterAKDexHost(t *testing.T) {
	mock := NewMockHelmClient(nil)
	other := helmHost("other-host")
	other.Namespace = "ns"

	got := helmReconcile(t, mock, "my-host", helmHost("my-host", "ok", "other-host"), other)

	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusFailed {
		t.Errorf("helm status = %q, want %q", s, HelmStatusFailed)
	}
	if e := got.Status.Attributes[AttributeHelmReleaseError]; !strings.Contains(e, "other-host") {
		t.Errorf("helm error = %q, want it to name other-host", e)
	}
	for _, name := range mock.Installed() {
		if name == "other-host" || name == "ok" {
			t.Errorf("installed %q despite the refused companion", name)
		}
	}
}

// Every reconcile's own status patch overwrites the server's status with its
// snapshot, except for the helm.release. keys the async helm operation writes.
// The companion record is one of those: outside that prefix, a reconcile that
// overlapped the operation erased it, and the next prune found nothing to do.
func TestCompanionRecordIsPreservedByTheReconcileStatusPatch(t *testing.T) {
	if !strings.HasPrefix(AttributeCompanionReleases, helmAttributePrefix) {
		t.Fatalf("%q is not a %s key, so Reconcile's deferred status patch overwrites it", AttributeCompanionReleases, helmAttributePrefix)
	}
}

// completedHost is a host whose last helm run completed for its current spec,
// so a reconcile finds nothing to run.
func completedHost(t *testing.T, r *KDexHostReconciler, host *kdexv1alpha1.KDexHost) *kdexv1alpha1.KDexHost {
	t.Helper()
	hash, err := r.computeHelmReleaseHash(host)
	if err != nil {
		t.Fatal(err)
	}
	if host.Status.Attributes == nil {
		host.Status.Attributes = map[string]string{}
	}
	host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusCompleted
	host.Status.Attributes[AttributeHelmReleaseHash] = hash
	return host
}

// A host whose companions were installed before the record existed has no
// record at all. Its hash need not change on upgrade, so no helm run records
// them, and a companion later dropped from the spec would never be pruned. The
// first reconcile seeds the record from what the spec declares, run or not.
func TestHelmReconcile_SeedsAnAbsentCompanionRecordFromTheSpec(t *testing.T) {
	probe, _ := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	legacy := completedHost(t, probe, helmHost("my-host", "b", "a", "other-host"))
	other := helmHost("other-host")
	other.Namespace = "ns"
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, legacy, other)

	op, err := r.reconcileHelmReleases(context.Background(), legacy.DeepCopy(), nil, logr.Discard())
	if err != nil {
		t.Fatalf("reconcileHelmReleases: %v", err)
	}
	if op != controllerutil.OperationResultNone {
		t.Fatalf("reconcileHelmReleases = %v, want no helm run for an unchanged hash", op)
	}
	got := getHost(t, fc, "my-host")
	rec, ok := got.Status.Attributes[AttributeCompanionReleases]
	if !ok || rec != "a,b" {
		t.Errorf("companion record = %q (present %v), want %q: declared, never a KDexHost's name", rec, ok, "a,b")
	}
}

// An empty record is a record: the operator installed nothing that survives.
// Re-seeding it from the spec would let a declared name the operator never
// installed authorize an uninstall.
func TestHelmReconcile_DoesNotReseedAnEmptyCompanionRecord(t *testing.T) {
	probe, _ := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	host := recorded(completedHost(t, probe, helmHost("my-host", "a")))
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)

	if _, err := r.reconcileHelmReleases(context.Background(), host.DeepCopy(), nil, logr.Discard()); err != nil {
		t.Fatalf("reconcileHelmReleases: %v", err)
	}
	if rec := getHost(t, fc, "my-host").Status.Attributes[AttributeCompanionReleases]; rec != "" {
		t.Errorf("companion record = %q, want it left empty", rec)
	}
}

// A name recorded before an install that failed, and then dropped from the
// spec, has no release behind it. Left in the record it would authorize the
// uninstall of whatever release someone later forges under that name with the
// host's label. A run keeps only names still declared or still labelled.
func TestHelmReconcile_ForgetsRecordedNamesWithNoReleaseOrDeclaration(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"theirs": {LabelCompanionOf: "other-host"},
	})
	host := recorded(helmHost("my-host", "a"), "a", "failed", "theirs")

	got := helmReconcile(t, mock, "my-host", host)

	if rec := got.Status.Attributes[AttributeCompanionReleases]; rec != "a" {
		t.Errorf("companion record = %q, want %q", rec, "a")
	}
	if len(mock.Uninstalled()) != 0 {
		t.Errorf("uninstalled = %v, want none", mock.Uninstalled())
	}
}

// hookedHelmClient runs onUninstall before each uninstall, standing in for
// whatever else writes the host while the prune waits on it.
type hookedHelmClient struct {
	*MockHelmClient
	onUninstall func(releaseName string)
}

func (h *hookedHelmClient) Uninstall(releaseName string) error {
	h.onUninstall(releaseName)
	return h.MockHelmClient.Uninstall(releaseName)
}

// addToRecord adds name to the host's record the way another helm operation
// for the same host would.
func addToRecord(t *testing.T, c client.Client, host, name string) {
	t.Helper()
	h := getHost(t, c, host)
	patch := client.MergeFrom(h.DeepCopy())
	h.Status.Attributes[AttributeCompanionReleases] += "," + name
	if err := c.Status().Patch(context.Background(), h, patch); err != nil {
		t.Fatal(err)
	}
}

// Two helm operations for one host can overlap: the older one's prune runs
// after it gives up its render slot. Each read the record at its start, so one
// writing back the whole record it computed drops whatever the other recorded
// in the meantime -- a companion then installed but unrecorded, never pruned.
func TestHelmReconcile_RecordWriteKeepsWhatAnOverlappingOperationRecorded(t *testing.T) {
	var r *KDexHostReconciler
	var fc client.Client
	mock := &hookedHelmClient{
		MockHelmClient: NewMockHelmClient(map[string]map[string]string{
			"dropped": {LabelCompanionOf: "my-host"},
		}),
		onUninstall: func(string) { addToRecord(t, fc, "my-host", "newer") },
	}
	r, fc = newHelmReconciler(t, mock, nil, recorded(helmHost("my-host", "a"), "a", "dropped"))

	r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())

	if rec := getHost(t, fc, "my-host").Status.Attributes[AttributeCompanionReleases]; rec != "a,newer" {
		t.Errorf("companion record = %q, want %q", rec, "a,newer")
	}
}

// The same holds for a write landing between the operation's read of the host
// and its patch: the patch must not apply over a record it did not read.
func TestHelmReconcile_RecordWriteRetriesOverAConcurrentWrite(t *testing.T) {
	var raced bool
	funcs := &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			h, ok := obj.(*kdexv1alpha1.KDexHost)
			if ok && !raced && h.Status.Attributes[AttributeHelmReleaseStatus] == HelmStatusCompleted {
				raced = true
				addToRecord(t, c, "my-host", "newer")
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
	mock := NewMockHelmClient(map[string]map[string]string{
		"dropped": {LabelCompanionOf: "my-host"},
	})
	r, fc := newHelmReconciler(t, mock, funcs, recorded(helmHost("my-host", "a"), "a", "dropped"))

	r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())

	got := getHost(t, fc, "my-host")
	if !raced {
		t.Fatal("the completing status patch was never intercepted")
	}
	if rec := got.Status.Attributes[AttributeCompanionReleases]; rec != "a,newer" {
		t.Errorf("companion record = %q, want %q", rec, "a,newer")
	}
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusCompleted {
		t.Errorf("helm status = %q, want %q", s, HelmStatusCompleted)
	}
}

// Two operations that both read the record "a": one installed "b", the other
// pruned "a". Whichever writes last, both changes stand.
func TestUpdateHelmStatus_TwoChangesFromOneStaleBaseBothSurvive(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, recorded(helmHost("my-host", "b"), "a"))

	installed := companionRecordChange{added: []string{"b"}}
	pruned := companionRecordChange{pruned: []string{"a"}}
	r.updateHelmStatus(context.Background(), "ns", "my-host", "hash", nil, &installed, logr.Discard())
	r.updateHelmStatus(context.Background(), "ns", "my-host", "hash", nil, &pruned, logr.Discard())

	if rec := getHost(t, fc, "my-host").Status.Attributes[AttributeCompanionReleases]; rec != "b" {
		t.Errorf("companion record = %q, want %q", rec, "b")
	}
}

// The manager's client reads through an informer cache. An optimistic-lock
// retry that re-reads through it can see the same stale resourceVersion every
// time, conflict every time, and lose the write: a helm outcome lost that way
// leaves the host in_progress for a zombie re-run. The read behind a guarded
// write goes to the API server.
func TestPatchHostStatus_ReadsLiveWhenTheCacheIsStale(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, recorded(helmHost("my-host", "b"), "a"))
	stale := getHost(t, fc, "my-host")
	// Something else writes the host, which the cache has yet to see.
	addToRecord(t, fc, "my-host", "newer")

	r.APIReader = fc
	r.Client = interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if h, ok := obj.(*kdexv1alpha1.KDexHost); ok && key.Name == "my-host" {
				stale.DeepCopyInto(h)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	installed := companionRecordChange{added: []string{"b"}}
	r.updateHelmStatus(context.Background(), "ns", "my-host", "hash", nil, &installed, logr.Discard())

	got := getHost(t, fc, "my-host")
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusCompleted {
		t.Errorf("helm status = %q, want %q: the outcome was lost", s, HelmStatusCompleted)
	}
	if rec := got.Status.Attributes[AttributeCompanionReleases]; rec != "a,b,newer" {
		t.Errorf("companion record = %q, want %q", rec, "a,b,newer")
	}
}

// blockingHelmClient holds the install of one release until released, standing
// in for a helm call that cannot be cancelled: Helm takes no context.
type blockingHelmClient struct {
	*MockHelmClient
	release string
	entered chan struct{}
	proceed chan struct{}
}

func (b *blockingHelmClient) InstallOrUpgrade(spec *utils.ChartSpec) error {
	if spec.ReleaseName == b.release {
		close(b.entered)
		<-b.proceed
	}
	return b.MockHelmClient.InstallOrUpgrade(spec)
}

// Teardown lists the releases labelled as the host's. One a helm operation is
// still installing has no release record yet, so it is not among them; were
// teardown to finish and drop the finalizer, the install would then land
// labelled for a host that no longer exists. Teardown waits for the operation.
func TestTeardownWaitsForAnInFlightHelmOperation(t *testing.T) {
	host := recorded(helmHost("my-host", "x1", "x2"))
	host.Finalizers = []string{hostFinalizerName}
	mock := &blockingHelmClient{
		MockHelmClient: NewMockHelmClient(nil),
		release:        "x2",
		entered:        make(chan struct{}),
		proceed:        make(chan struct{}),
	}
	r, fc := newHelmReconciler(t, mock, nil, host)
	key := client.ObjectKey{Namespace: "ns", Name: "my-host"}

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())
	}()
	<-mock.entered

	if err := fc.Delete(context.Background(), getHost(t, fc, "my-host")); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("teardown did not requeue while the helm operation was in flight")
	}
	if !controllerutil.ContainsFinalizer(getHost(t, fc, "my-host"), hostFinalizerName) {
		t.Fatal("finalizer removed while the helm operation was still installing x2")
	}
	if u := mock.Uninstalled(); len(u) != 0 {
		t.Errorf("uninstalled %v while the helm operation was in flight, want nothing yet", u)
	}

	close(mock.proceed)
	<-done
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := fc.Get(context.Background(), key, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host still present after the helm operation finished (get = %v), want it finalized", err)
	}
	got := mock.Uninstalled()
	slices.Sort(got)
	if want := []string{"my-host", "x1", "x2"}; !slices.Equal(got, want) {
		t.Errorf("uninstalled = %v, want %v", got, want)
	}
}

// Helm cannot be interrupted mid-install, but a terminating host can stop the
// operation installing anything more.
func TestHelmReconcile_StopsInstallingOnceTheHostIsTerminating(t *testing.T) {
	host := recorded(helmHost("my-host", "x1", "x2", "x3"))
	host.Finalizers = []string{hostFinalizerName}
	mock := &blockingHelmClient{
		MockHelmClient: NewMockHelmClient(nil),
		release:        "x2",
		entered:        make(chan struct{}),
		proceed:        make(chan struct{}),
	}
	r, fc := newHelmReconciler(t, mock, nil, host)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())
	}()
	<-mock.entered
	if err := fc.Delete(context.Background(), getHost(t, fc, "my-host")); err != nil {
		t.Fatal(err)
	}
	close(mock.proceed)
	<-done

	if slices.Contains(mock.Installed(), "x3") {
		t.Errorf("installed = %v: x3 was installed after the host began terminating", mock.Installed())
	}
}
