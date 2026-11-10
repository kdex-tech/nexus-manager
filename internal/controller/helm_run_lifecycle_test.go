package controller

import (
	"context"
	"encoding/json"
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
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
)

// A fetch can outlast teardown's wait for it, and teardown has gone ahead by
// the time it returns. The chart it brought back is installed only if the host
// is still wanted once it is in hand.
func TestHelmReconcile_InstallsNothingOnceTheHostIsTerminatingAfterTheFetch(t *testing.T) {
	host := recorded(helmHost("my-host", "x1", "x2"))
	host.Finalizers = []string{hostFinalizerName}
	// Blocks before the chart is in hand: the fetch.
	mock := newBlockingHelmClient("x2", nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	staleHostCache(r, fc, notYetDeleted, interceptor.Funcs{})

	runInBackground(t, context.Background(), r, mock, "my-host")
	deleteHost(t, fc, "my-host")
	close(mock.proceed)
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})

	if slices.Contains(mock.Installed(), "x2") {
		t.Errorf("installed = %v: x2 was installed once its fetch returned for a terminating host", mock.Installed())
	}
}

// The pre-prune check is what spares a host that began terminating after its
// last install the release listing, not only the uninstalls.
func TestHelmReconcile_PrunesNothingForAHostThatBeganTerminatingAfterItsLastInstall(t *testing.T) {
	host := recorded(helmHost("my-host", "x1"), "x1", "dropped")
	host.Finalizers = []string{hostFinalizerName}
	var fc client.Client
	mock := &hookedHelmClient{
		MockHelmClient: NewMockHelmClient(map[string]map[string]string{"dropped": {LabelCompanionOf: "my-host"}}),
		afterInstall: func(name string) {
			if name == "x1" {
				deleteHost(t, fc, "my-host")
			}
		},
	}
	var r *KDexHostReconciler
	r, fc = newHelmReconciler(t, mock, nil, host)
	staleHostCache(r, fc, notYetDeleted, interceptor.Funcs{})

	r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())

	if n := mock.lists.Load(); n != 0 {
		t.Errorf("listed releases %d times for a terminating host, want none", n)
	}
	if u := mock.Uninstalled(); len(u) != 0 {
		t.Errorf("uninstalled = %v for a terminating host, want nothing", u)
	}
}

// Hashes repeat: a spec edited and edited back brings back the hash of an
// operation that may still be running. That operation finishing must not take
// away the active entry of the one that superseded its successor.
func TestRegisterHelmRun_AFinishedRunLeavesItsSuccessorActive(t *testing.T) {
	r := &KDexHostReconciler{}
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
	noop := func() {}

	first, ok := r.registerHelmRun(key, noop, "H1")
	if !ok {
		t.Fatal("first registration refused")
	}
	second, ok := r.registerHelmRun(key, noop, "H2")
	if !ok {
		t.Fatal("second registration refused")
	}
	third, ok := r.registerHelmRun(key, noop, "H1")
	if !ok {
		t.Fatal("third registration refused")
	}
	defer third()

	first()
	second()
	if !r.isHelmOperationActive(key, "H1") {
		t.Error("the first H1 operation finishing took the active entry of the H1 operation now running")
	}
	if !r.helmRunInFlight(key) {
		t.Error("the run now going is not counted")
	}
}

// The cache can still hold a host's terminating predecessor after the host was
// recreated under the same name. Acting on it would tear down the new host's
// releases and strip its finalizer.
func TestTeardown_LeavesARecreatedHostAlone(t *testing.T) {
	host := recorded(helmHost("my-host", "a"), "a")
	host.UID = "new"
	host.Finalizers = []string{hostFinalizerName}
	mock := NewMockHelmClient(map[string]map[string]string{
		"my-host": {},
		"a":       {LabelCompanionOf: "my-host"},
	})
	r, fc := newHelmReconciler(t, mock, nil, host)
	staleHostCache(r, fc, func(h *kdexv1alpha1.KDexHost) {
		now := metav1.Now()
		h.UID = "old"
		h.DeletionTimestamp = &now
	}, interceptor.Funcs{})

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "my-host"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if u := mock.Uninstalled(); len(u) != 0 {
		t.Errorf("uninstalled %v of the recreated host, want nothing", u)
	}
	if !controllerutil.ContainsFinalizer(getHost(t, fc, "my-host"), hostFinalizerName) {
		t.Error("the recreated host's finalizer was removed")
	}
}

// A read that fails once the run holds its render slot is a failed run, not a
// host that is gone: left in_progress, a zombie re-run would find it.
func TestHelmReconcile_ReportsAFailedReadOnceItHoldsASlot(t *testing.T) {
	host := recorded(helmHost("my-host", "x1"))
	host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusInProgress
	mock := NewMockHelmClient(nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	var failed atomic.Bool
	r.APIReader = interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if failed.CompareAndSwap(false, true) {
				return apierrors.NewServiceUnavailable("etcd is having a moment")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())

	got := getHost(t, fc, "my-host")
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusFailed {
		t.Errorf("helm status = %q, want %q", s, HelmStatusFailed)
	}
	if e := got.Status.Attributes[AttributeHelmReleaseError]; !strings.Contains(e, "etcd is having a moment") {
		t.Errorf("helm error = %q, want the read's error", e)
	}
	if len(mock.Installed()) != 0 {
		t.Errorf("installed = %v without a successful read of the host", mock.Installed())
	}
}

// The run is counted from the moment it is started, not from when its
// goroutine first runs: a teardown in between must already see it. With one
// P, the goroutine cannot run before this one yields.
func TestStartHelmRun_CountsTheRunBeforeItsGoroutineRuns(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	mock := newBlockingHelmClient("my-host", nil)
	r, _ := newHelmReconciler(t, mock, nil, helmHost("my-host"))
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}

	ctx, cancel := context.WithCancel(context.Background())
	if !r.startHelmRun(ctx, cancel, "ns", "my-host", "hash", nil, logr.Discard()) {
		t.Fatal("startHelmRun registered nothing")
	}
	counted := r.helmRunInFlight(key)
	again := r.startHelmRun(ctx, cancel, "ns", "my-host", "hash", nil, logr.Discard())
	<-mock.entered
	close(mock.proceed)
	awaitNoHelmRun(t, r, key)

	if !counted {
		t.Error("the run is not counted as soon as startHelmRun returns")
	}
	if again {
		t.Error("a second run for the same hash was started")
	}
}

// The in-progress patch carries the companion record as the cluster holds
// it. Rewriting it, even into an equivalent form, is a write to the record
// that only the helm operation is meant to make.
func TestReconcileHelmReleases_CarriesTheRecordAsItIs(t *testing.T) {
	var first []byte
	funcs := &interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if first == nil {
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				first = data
			}
			return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
		},
	}
	host := helmHost("my-host", "a", "b")
	host.Status.Attributes = map[string]string{AttributeCompanionReleases: "b,a"}
	r, _ := newHelmReconciler(t, NewMockHelmClient(nil), funcs, host)

	if _, err := r.reconcileHelmReleases(context.Background(), host.DeepCopy(), nil, logr.Discard()); err != nil {
		t.Fatalf("reconcileHelmReleases: %v", err)
	}
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})

	if first == nil {
		t.Fatal("no in-progress patch was written")
	}
	if strings.Contains(string(first), AttributeCompanionReleases) {
		t.Errorf("the in-progress patch %s rewrites the companion record", first)
	}
}

// A host that fails the same way, run after run, waits longer between runs:
// each holds the fleet's render slot for as long as its fetch takes, and a
// tenant's failing chart would otherwise have it most of the time.
func TestHelmRetryBackoff(t *testing.T) {
	base := 15 * time.Second
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{0, base}, {1, base}, {2, 2 * base}, {3, 4 * base}, {4, 8 * base},
		{100, helmRetryBackoffCap},
	} {
		if got := helmRetryBackoff(base, tc.failures); got != tc.want {
			t.Errorf("helmRetryBackoff(%s, %d) = %s, want %s", base, tc.failures, got, tc.want)
		}
	}
	if got := helmRetryBackoff(0, 5); got != 0 {
		t.Errorf("helmRetryBackoff(0, 5) = %s, want 0: no RequeueDelay, no backoff", got)
	}
}

// stampRun stamps host with what reconcileHelmReleases compares to tell a
// retry from a new run: the spec's hash and the resolved Secrets' hash.
func stampRun(t *testing.T, host *kdexv1alpha1.KDexHost, status string, attempts int, lastAttempt time.Time) *kdexv1alpha1.KDexHost {
	t.Helper()
	probe, _ := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	hash, err := probe.computeHelmReleaseHash(host)
	if err != nil {
		t.Fatal(err)
	}
	host.Status.Attributes[AttributeHelmReleaseStatus] = status
	host.Status.Attributes[AttributeHelmReleaseHash] = hash
	host.Status.Attributes[AttributeHelmReleaseSecretsHash] = utils.HashSecrets(nil)
	host.Status.Attributes[AttributeHelmReleaseAttempts] = strconv.Itoa(attempts)
	host.Status.Attributes[AttributeHelmReleaseLastAttempt] = lastAttempt.UTC().Format(time.RFC3339)
	return host
}

func TestHelmReconcile_BacksOffRepeatedFailures(t *testing.T) {
	host := stampRun(t, recorded(helmHost("my-host")), HelmStatusFailed, 3, time.Now().Add(-2*time.Minute))
	r, _ := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	r.RequeueDelay = time.Minute

	op, err := r.reconcileHelmReleases(context.Background(), host.DeepCopy(), nil, logr.Discard())
	if err != nil {
		t.Fatalf("reconcileHelmReleases: %v", err)
	}
	if op != controllerutil.OperationResultNone {
		t.Error("retried two minutes after a third attempt failed, want it to wait four")
	}
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})
}

// A run that kills the operator -- a chart that runs it out of memory --
// records no failure: the host is left in_progress, and a restarted operator
// finds a zombie. Restarting it at once would crash-loop the operator. Each
// attempt is counted when it starts, and a zombie backs off like a failure.
func TestHelmReconcile_BacksOffACrashLoop(t *testing.T) {
	host := stampRun(t, recorded(helmHost("my-host")), HelmStatusInProgress, 2, time.Now())
	r, _ := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	r.RequeueDelay = time.Minute

	op, err := r.reconcileHelmReleases(context.Background(), host.DeepCopy(), nil, logr.Discard())
	if err != nil {
		t.Fatalf("reconcileHelmReleases: %v", err)
	}
	if op != controllerutil.OperationResultNone {
		t.Error("restarted a zombie the moment its second attempt began, want it to back off")
	}
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})
}

func TestHelmReconcile_CountsAnAttemptWhenItStarts(t *testing.T) {
	host := stampRun(t, recorded(helmHost("my-host")), HelmStatusFailed, 2, time.Now().Add(-time.Hour))
	mock := newBlockingHelmClient("my-host", nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}

	if _, err := r.reconcileHelmReleases(context.Background(), host.DeepCopy(), nil, logr.Discard()); err != nil {
		t.Fatalf("reconcileHelmReleases: %v", err)
	}
	<-mock.entered
	running := getHost(t, fc, "my-host").Status.Attributes
	close(mock.proceed)
	awaitNoHelmRun(t, r, key)

	if n := running[AttributeHelmReleaseAttempts]; n != "3" {
		t.Errorf("attempts while the third runs = %q, want 3", n)
	}
	if _, ok := getHost(t, fc, "my-host").Status.Attributes[AttributeHelmReleaseAttempts]; ok {
		t.Error("attempts kept after a success, want them cleared")
	}
}

// A failed attempt was counted when it started; failing does not count it
// again.
func TestHelmReconcile_CountsAFailedAttemptOnce(t *testing.T) {
	host := stampRun(t, recorded(helmHost("my-host")), HelmStatusFailed, 2, time.Now().Add(-time.Hour))
	mock := NewMockHelmClient(nil)
	mock.SetFailInstall(true, "still failing")
	r, fc := newHelmReconciler(t, mock, nil, host)

	if _, err := r.reconcileHelmReleases(context.Background(), host.DeepCopy(), nil, logr.Discard()); err != nil {
		t.Fatalf("reconcileHelmReleases: %v", err)
	}
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})

	if n := getHost(t, fc, "my-host").Status.Attributes[AttributeHelmReleaseAttempts]; n != "3" {
		t.Errorf("attempts after a third failure = %q, want 3", n)
	}
}

// Attempts are counted per spec and per resolved Secrets: fixing the
// registry's Secret, or the spec, is a new run, not one more retry to wait out.
func TestHelmReconcile_StartsCountingAgainForNewSecretsOrSpec(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale func(*kdexv1alpha1.KDexHost)
	}{
		{"secrets", func(h *kdexv1alpha1.KDexHost) { h.Status.Attributes[AttributeHelmReleaseSecretsHash] = "older secrets" }},
		{"spec", func(h *kdexv1alpha1.KDexHost) { h.Status.Attributes[AttributeHelmReleaseHash] = "an older spec" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := stampRun(t, recorded(helmHost("my-host")), HelmStatusFailed, 5, time.Now().Add(-2*time.Minute))
			tc.stale(host)
			mock := newBlockingHelmClient("my-host", nil)
			r, fc := newHelmReconciler(t, mock, nil, host)
			r.RequeueDelay = time.Minute

			op, err := r.reconcileHelmReleases(context.Background(), host.DeepCopy(), nil, logr.Discard())
			if err != nil {
				t.Fatalf("reconcileHelmReleases: %v", err)
			}
			if op != controllerutil.OperationResultUpdated {
				t.Fatal("waited out the backoff of a different run")
			}
			<-mock.entered
			n := getHost(t, fc, "my-host").Status.Attributes[AttributeHelmReleaseAttempts]
			close(mock.proceed)
			awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})
			if n != "1" {
				t.Errorf("attempts of the new run = %q, want 1", n)
			}
		})
	}
}

// A waiting teardown looks again soon, whatever RequeueDelay says: nothing
// else wakes it. And it leaves Degraded as the last run reported it.
func TestTeardownWaitPollsSoonAndLeavesDegradedAlone(t *testing.T) {
	host := recorded(helmHost("my-host", "x1"))
	host.Finalizers = []string{hostFinalizerName}
	kdexv1alpha1.SetConditions(&host.Status.Conditions, kdexv1alpha1.ConditionStatuses{Degraded: metav1.ConditionTrue},
		kdexv1alpha1.ConditionReasonReconcileError, "the last run failed")
	mock := newBlockingHelmClient("x1", nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	r.RequeueDelay = 15 * time.Second
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}

	runInBackground(t, context.Background(), r, mock, "my-host")
	deleteHost(t, fc, "my-host")
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	close(mock.proceed)
	awaitNoHelmRun(t, r, key)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != helmTeardownPoll {
		t.Errorf("RequeueAfter = %s, want %s", res.RequeueAfter, helmTeardownPoll)
	}
	if !meta.IsStatusConditionTrue(getHost(t, fc, "my-host").Status.Conditions, string(kdexv1alpha1.ConditionTypeDegraded)) {
		t.Error("the waiting teardown cleared Degraded")
	}
}

// decodingOverReader reads hosts as a real API client does: decoding into the
// object it is given, which keeps whatever the JSON leaves out -- a deletion
// timestamp, labels, map keys -- from what the object held before.
func decodingOverReader(fc client.Client) client.Reader {
	return interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*kdexv1alpha1.KDexHost); !ok {
				return c.Get(ctx, key, obj, opts...)
			}
			live := &kdexv1alpha1.KDexHost{}
			if err := c.Get(ctx, key, live, opts...); err != nil {
				return err
			}
			data, err := json.Marshal(live)
			if err != nil {
				return err
			}
			return json.Unmarshal(data, obj)
		},
	})
}

// Teardown decides on the host as the cluster holds it, read into an object of
// its own: decoded over the cached host, a deletion the cluster no longer
// shows would survive from the cache.
func TestTeardown_ReadsTheClusterHostIntoAFreshObject(t *testing.T) {
	host := recorded(helmHost("my-host", "a"), "a")
	host.UID = "same"
	host.Finalizers = []string{hostFinalizerName}
	mock := NewMockHelmClient(map[string]map[string]string{
		"my-host": {},
		"a":       {LabelCompanionOf: "my-host"},
	})
	r, fc := newHelmReconciler(t, mock, nil, host)
	staleHostCache(r, fc, func(h *kdexv1alpha1.KDexHost) {
		now := metav1.Now()
		h.DeletionTimestamp = &now
	}, interceptor.Funcs{})
	r.APIReader = decodingOverReader(fc)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "my-host"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if u := mock.Uninstalled(); len(u) != 0 {
		t.Errorf("uninstalled %v of a host the cluster does not show terminating", u)
	}
	if !controllerutil.ContainsFinalizer(getHost(t, fc, "my-host"), hostFinalizerName) {
		t.Error("the finalizer of a host the cluster does not show terminating was removed")
	}
}

// The helm operation owns the helm.release. attributes. A reconcile's status
// patch takes them as last read, so it never writes one: a key the operation
// deleted --
// a success clears last-attempt and attempts -- is not restored from the
// reconcile's own copy, which may have set it when the operation started.
func TestTakeHelmAttributes(t *testing.T) {
	snapshot := map[string]string{
		AttributeHelmReleaseLastAttempt: "when the run started",
		AttributeHelmReleaseAttempts:    "1",
		AttributeHelmReleaseStatus:      HelmStatusInProgress,
		"theme.generation":              "7",
	}
	cluster := map[string]string{
		AttributeHelmReleaseStatus: HelmStatusCompleted,
		AttributeCompanionReleases: "a",
	}

	takeHelmAttributes(snapshot, cluster)

	want := map[string]string{
		AttributeHelmReleaseStatus: HelmStatusCompleted,
		AttributeCompanionReleases: "a",
		"theme.generation":         "7",
	}
	if !maps.Equal(snapshot, want) {
		t.Errorf("attributes = %v, want %v", snapshot, want)
	}
}

// The run's Secrets are read afresh on every reconcile. Two reads of the same
// Secrets are the same run, and keep its backoff.
func TestHelmReconcile_BacksOffAcrossReadsOfTheSameSecrets(t *testing.T) {
	host := stampRun(t, recorded(helmHost("my-host")), HelmStatusFailed, 3, time.Now().Add(-2*time.Minute))
	host.Status.Attributes[AttributeHelmReleaseSecretsHash] = utils.HashSecrets(secretsWithPassword("1", "pw"))
	r, _ := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	r.RequeueDelay = time.Minute

	op, err := r.reconcileHelmReleases(context.Background(), host.DeepCopy(), secretsWithPassword("1", "pw"), logr.Discard())
	if err != nil {
		t.Fatalf("reconcileHelmReleases: %v", err)
	}
	if op != controllerutil.OperationResultNone {
		t.Error("a fresh read of the same Secrets was taken for new ones, and the backoff skipped")
	}
	awaitNoHelmRun(t, r, types.NamespacedName{Namespace: "ns", Name: "my-host"})
}
