package controller

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"slices"
)

// staleHostCache puts an informer cache that lags the cluster in front of r:
// every host its Client reads comes back as stale leaves it, while its
// APIReader reads the cluster as it is. funcs intercepts the rest of Client.
func staleHostCache(r *KDexHostReconciler, fc client.Client, stale func(cached *kdexv1alpha1.KDexHost), funcs interceptor.Funcs) {
	funcs.Get = func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if h, ok := obj.(*kdexv1alpha1.KDexHost); ok {
			stale(h)
		}
		return nil
	}
	r.APIReader = fc
	r.Client = interceptor.NewClient(fc.(client.WithWatch), funcs)
}

// asOf makes a cached host show snapshot's status and resourceVersion: the
// cache has not yet seen the writes since.
func asOf(snapshot *kdexv1alpha1.KDexHost) func(*kdexv1alpha1.KDexHost) {
	return func(h *kdexv1alpha1.KDexHost) {
		h.ResourceVersion = snapshot.ResourceVersion
		h.Status = *snapshot.Status.DeepCopy()
	}
}

// notYetDeleted makes a cached host show no deletion: the cache has not yet
// seen it begin.
func notYetDeleted(h *kdexv1alpha1.KDexHost) {
	h.DeletionTimestamp = nil
}

// deleteHost begins the deletion of a host that holds a finalizer.
func deleteHost(t *testing.T, c client.Client, name string) {
	t.Helper()
	if err := c.Delete(context.Background(), getHost(t, c, name)); err != nil {
		t.Fatal(err)
	}
}

// awaitNoHelmRun waits for every helm run for the host to return.
func awaitNoHelmRun(t *testing.T, r *KDexHostReconciler, key types.NamespacedName) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.helmRunInFlight(key) {
		if time.Now().After(deadline) {
			t.Fatal("the helm run never returned")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockingHelmClient holds the first install of one release until proceed is
// closed, standing in for a helm call that cannot be cancelled: Helm takes no
// context.
type blockingHelmClient struct {
	*MockHelmClient
	release string
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func newBlockingHelmClient(release string, releaseLabels map[string]map[string]string) *blockingHelmClient {
	return &blockingHelmClient{
		MockHelmClient: NewMockHelmClient(releaseLabels),
		release:        release,
		entered:        make(chan struct{}),
		proceed:        make(chan struct{}),
	}
}

func (b *blockingHelmClient) InstallOrUpgrade(spec *utils.ChartSpec) error {
	if spec.ReleaseName == b.release {
		b.once.Do(func() {
			close(b.entered)
			<-b.proceed
		})
	}
	return b.MockHelmClient.InstallOrUpgrade(spec)
}

// runInBackground runs one helm operation on its own goroutine, as a
// reconcile would, and returns once it is blocked in mock.
func runInBackground(t *testing.T, ctx context.Context, r *KDexHostReconciler, mock *blockingHelmClient, host string) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	if !r.startHelmRun(ctx, cancel, "ns", host, "hash", nil, logr.Discard()) {
		t.Fatal("startHelmRun registered nothing")
	}
	<-mock.entered
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
	staleHostCache(r, fc, func(h *kdexv1alpha1.KDexHost) { stale.DeepCopyInto(h) }, interceptor.Funcs{})

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

// Teardown lists the releases labelled as the host's. One a helm operation is
// still installing has no release record yet, so it is not among them; were
// teardown to finish and drop the finalizer, the install would then land
// labelled for a host that no longer exists. Teardown waits for the operation,
// says so on the host, and does nothing else meanwhile. Once the operation is
// done it tears down by the record as the cluster holds it -- the cache has
// yet to see the operation record x1 and x2.
func TestTeardownWaitsForAnInFlightHelmOperation(t *testing.T) {
	host := recorded(helmHost("my-host", "x1", "x2"))
	host.Finalizers = []string{hostFinalizerName}
	mock := newBlockingHelmClient("x2", nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	var lists atomic.Int32
	staleHostCache(r, fc, asOf(getHost(t, fc, "my-host")), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return c.List(ctx, list, opts...)
		},
	})
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}

	runInBackground(t, context.Background(), r, mock, "my-host")
	deleteHost(t, fc, "my-host")
	lists.Store(0)
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("teardown did not requeue while the helm operation was in flight")
	}
	waiting := getHost(t, fc, "my-host")
	if !controllerutil.ContainsFinalizer(waiting, hostFinalizerName) {
		t.Fatal("finalizer removed while the helm operation was still installing x2")
	}
	if u := mock.Uninstalled(); len(u) != 0 {
		t.Errorf("uninstalled %v while the helm operation was in flight, want nothing yet", u)
	}
	if n := lists.Load(); n != 0 {
		t.Errorf("a waiting teardown listed %d times, want it to check for the operation first", n)
	}
	cond := meta.FindStatusCondition(waiting.Status.Conditions, string(kdexv1alpha1.ConditionTypeProgressing))
	if cond == nil || !strings.Contains(cond.Message, "in-flight Helm operation") {
		t.Errorf("Progressing condition = %v, want it to say deletion waits on an in-flight Helm operation", cond)
	}

	close(mock.proceed)
	awaitNoHelmRun(t, r, key)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := fc.Get(context.Background(), key, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host still present after the helm operation finished (get = %v), want it finalized", err)
	}
	got := mock.Uninstalled()
	slices.Sort(got)
	// x2 was being fetched when the host began terminating, so it was never
	// installed: there is no x2 to uninstall.
	if want := []string{"my-host", "x1"}; !slices.Equal(got, want) {
		t.Errorf("uninstalled = %v, want %v", got, want)
	}
}

// The wait is defence in depth, not a promise: an operation stuck in a Helm
// call past every deadline Helm and the operator give it must not hold a
// deletion -- of the host, or of its namespace -- forever.
func TestTeardownStopsWaitingOnceTheCapPasses(t *testing.T) {
	host := recorded(helmHost("my-host", "a"), "a")
	host.Finalizers = []string{hostFinalizerName}
	long := metav1.NewTime(time.Now().Add(-(helmTeardownWaitCap + time.Minute)))
	host.DeletionTimestamp = &long
	mock := NewMockHelmClient(map[string]map[string]string{"a": {LabelCompanionOf: "my-host"}})
	r, fc := newHelmReconciler(t, mock, nil, host)
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
	_, cancel := context.WithCancel(context.Background())
	done, ok := r.registerHelmRun(key, cancel, "stuck")
	if !ok {
		t.Fatal("registerHelmRun registered nothing")
	}
	defer done()

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := fc.Get(context.Background(), key, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host still present past the wait cap (get = %v), want it finalized", err)
	}
	if !slices.Contains(mock.Uninstalled(), "a") {
		t.Errorf("uninstalled = %v, want it to include a", mock.Uninstalled())
	}
}

// Helm cannot be interrupted mid-install, but a terminating host can stop the
// operation installing anything more -- seen live, not through a cache that
// has yet to see the deletion.
func TestHelmReconcile_StopsInstallingOnceTheHostIsTerminating(t *testing.T) {
	host := recorded(helmHost("my-host", "x1", "x2", "x3"))
	host.Finalizers = []string{hostFinalizerName}
	mock := newBlockingHelmClient("x2", nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	staleHostCache(r, fc, notYetDeleted, interceptor.Funcs{})

	runInBackground(t, context.Background(), r, mock, "my-host")
	deleteHost(t, fc, "my-host")
	close(mock.proceed)
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})

	if slices.Contains(mock.Installed(), "x3") {
		t.Errorf("installed = %v: x3 was installed after the host began terminating", mock.Installed())
	}
}

// A run queued behind the render slot may find, once it holds one, that its
// host began terminating while it waited. It must see that live.
func TestHelmReconcile_AbandonsARunWhoseHostBeganTerminatingWhileQueued(t *testing.T) {
	host := recorded(helmHost("my-host", "x1"))
	host.Finalizers = []string{hostFinalizerName}
	now := metav1.Now()
	host.DeletionTimestamp = &now
	mock := NewMockHelmClient(nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	staleHostCache(r, fc, notYetDeleted, interceptor.Funcs{})

	r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())

	if got := mock.Installed(); len(got) != 0 {
		t.Errorf("installed = %v for a terminating host, want nothing", got)
	}
}

// Teardown uninstalls every recorded release anyway, so a run whose host began
// terminating prunes nothing: each prune would only delay the teardown.
func TestHelmReconcile_SkipsThePruneOnceTheHostIsTerminating(t *testing.T) {
	host := recorded(helmHost("my-host", "a"), "a", "dropped")
	host.Finalizers = []string{hostFinalizerName}
	mock := newBlockingHelmClient("a", map[string]map[string]string{"dropped": {LabelCompanionOf: "my-host"}})
	r, fc := newHelmReconciler(t, mock, nil, host)
	staleHostCache(r, fc, notYetDeleted, interceptor.Funcs{})

	runInBackground(t, context.Background(), r, mock, "my-host")
	deleteHost(t, fc, "my-host")
	close(mock.proceed)
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})

	if got := mock.Uninstalled(); len(got) != 0 {
		t.Errorf("uninstalled = %v after the host began terminating, want nothing", got)
	}
}

// A prune already under way stops at the next release once its host is seen
// terminating: each uninstall can take utils.HelmCallTimeout, and teardown,
// waiting on this run, removes the rest anyway.
func TestHelmReconcile_StopsPruningOnceTheHostIsTerminating(t *testing.T) {
	host := recorded(helmHost("my-host"), "d1", "d2")
	host.Finalizers = []string{hostFinalizerName}
	var fc client.Client
	mock := &hookedHelmClient{
		MockHelmClient: NewMockHelmClient(map[string]map[string]string{
			"d1": {LabelCompanionOf: "my-host"},
			"d2": {LabelCompanionOf: "my-host"},
		}),
		onUninstall: func(name string) {
			if name == "d1" {
				deleteHost(t, fc, "my-host")
			}
		},
	}
	var r *KDexHostReconciler
	r, fc = newHelmReconciler(t, mock, nil, host)
	staleHostCache(r, fc, notYetDeleted, interceptor.Funcs{})

	r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())

	if got := mock.Uninstalled(); !slices.Equal(got, []string{"d1"}) {
		t.Errorf("uninstalled = %v, want only d1: d2 was pruned after the host began terminating", got)
	}
}

// A check that cannot read the host is a failed run, reported as one, not an
// operation left in_progress for a zombie re-run to find.
func TestHelmReconcile_ReportsAFailedTerminatingCheck(t *testing.T) {
	host := recorded(helmHost("my-host", "x1", "x2"))
	host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusInProgress
	mock := newBlockingHelmClient("x1", nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	var failNext atomic.Bool
	r.APIReader = interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failNext.CompareAndSwap(true, false) {
				return apierrors.NewServiceUnavailable("etcd is having a moment")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	runInBackground(t, context.Background(), r, mock, "my-host")
	failNext.Store(true)
	close(mock.proceed)
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})

	got := getHost(t, fc, "my-host")
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusFailed {
		t.Errorf("helm status = %q, want %q", s, HelmStatusFailed)
	}
	if e := got.Status.Attributes[AttributeHelmReleaseError]; !strings.Contains(e, "etcd is having a moment") {
		t.Errorf("helm error = %q, want the read's error", e)
	}
	if slices.Contains(mock.Installed(), "x2") {
		t.Errorf("installed = %v: x2 was installed without knowing whether the host is terminating", mock.Installed())
	}
}

// A superseded or cancelled operation stops installing, and quietly: its
// successor, or the teardown, owns the host's status now.
func TestHelmReconcile_StopsQuietlyOnceCancelled(t *testing.T) {
	host := recorded(helmHost("my-host", "x1", "x2"))
	host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusInProgress
	mock := newBlockingHelmClient("x1", nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	ctx, cancel := context.WithCancel(context.Background())

	runInBackground(t, ctx, r, mock, "my-host")
	cancel()
	close(mock.proceed)
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})

	if slices.Contains(mock.Installed(), "x2") {
		t.Errorf("installed = %v: x2 was installed after the operation was cancelled", mock.Installed())
	}
	if s := getHost(t, fc, "my-host").Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusInProgress {
		t.Errorf("helm status = %q, want it left %q for the successor", s, HelmStatusInProgress)
	}
}
