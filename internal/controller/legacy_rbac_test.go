package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// helmOwnedMeta is the metadata Helm stamps on what release release in
// namespace releaseNS installed.
func helmOwnedMeta(name, namespace, release, releaseNS string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: namespace,
		UID:       types.UID("uid-" + namespace + "-" + name),
		Annotations: map[string]string{
			"meta.helm.sh/release-name":      release,
			"meta.helm.sh/release-namespace": releaseNS,
		},
	}
}

// legacyClusterRole is the ClusterRole a pre-redesign host-manager chart
// rendered for host in ns.
func legacyClusterRole(host, ns string) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{ObjectMeta: helmOwnedMeta(host, "", host, ns)}
}

// legacyClusterRoleBinding is the ClusterRoleBinding a pre-redesign
// host-manager chart rendered for host in ns.
func legacyClusterRoleBinding(host, ns string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: helmOwnedMeta(host+"-"+ns, "", host, ns),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: host},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: host, Namespace: ns}},
	}
}

// chartRoleBinding is the namespaced RoleBinding the redesigned host-manager
// chart renders: its presence proves the upgrade landed.
func chartRoleBinding(host, ns string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: helmOwnedMeta(host, ns, host, ns),
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: host},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: host, Namespace: ns}},
	}
}

// rolledOutDeployment is the host-manager Deployment the redesigned chart
// renders, fully rolled out.
func rolledOutDeployment(host, ns string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: host, Namespace: ns, Generation: 2},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
		},
	}
}

// upgraded makes the cluster hold what proves the redesigned chart replaced
// the legacy grant: its RoleBinding and its rolled-out Deployment.
func upgraded(t *testing.T, c client.Client, host, ns string) {
	t.Helper()
	createObjects(t, c, chartRoleBinding(host, ns), rolledOutDeployment(host, ns))
}

// legacyRecord is what a never-upgraded host-manager release's record lists
// outside its namespace.
func legacyRecord(host, ns string) []utils.ReleaseObject {
	return []utils.ReleaseObject{
		{Kind: "ServiceAccount", Name: host},
		{Kind: "ClusterRole", Name: host},
		{Kind: "ClusterRoleBinding", Name: host + "-" + ns},
		{Kind: "Deployment", Name: host},
	}
}

func createObjects(t *testing.T, c client.Client, objs ...client.Object) {
	t.Helper()
	for _, o := range objs {
		if err := c.Create(context.Background(), o); err != nil {
			t.Fatalf("create %T %s: %v", o, o.GetName(), err)
		}
	}
}

func assertExists(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); err != nil {
		t.Errorf("%T %s: %v, want it kept", obj, obj.GetName(), err)
	}
}

func assertDeleted(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
		t.Errorf("%T %s get = %v, want NotFound", obj, obj.GetName(), err)
	}
}

// reconcilable makes a fake cluster hold what a full non-deleting Reconcile
// resolves for every host.
func reconcilable(t *testing.T, c client.Client) {
	t.Helper()
	createObjects(t, c, &kdexv1alpha1.KDexClusterTranslation{
		ObjectMeta: metav1.ObjectMeta{Name: "kdex-default-translation"},
		Spec: kdexv1alpha1.KDexTranslationSpec{Translations: []kdexv1alpha1.Translation{
			{Lang: "en", KeysAndValues: map[string]string{"brandName": "KDex Tech"}},
		}},
		Status: kdexv1alpha1.KDexObjectStatus{Conditions: []metav1.Condition{{
			Type: string(kdexv1alpha1.ConditionTypeReady), Status: metav1.ConditionTrue,
			Reason: string(kdexv1alpha1.ConditionReasonReconcileSuccess), LastTransitionTime: metav1.Now(),
		}}},
	})
}

func hostIn(name, ns string) *kdexv1alpha1.KDexHost {
	h := helmHost(name)
	h.Namespace = ns
	return h
}

func TestCleanupLegacy_WaitsForNewRoleBinding(t *testing.T) {
	host := helmHost("my-host")
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	role, binding := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
	createObjects(t, fc, role, binding)

	if _, err := r.cleanupLegacyRBAC(context.Background(), host); err != nil {
		t.Fatalf("cleanupLegacyRBAC: %v", err)
	}
	assertExists(t, fc, role)
	assertExists(t, fc, binding)
	cond := meta.FindStatusCondition(host.Status.Conditions, "LegacyRBACPresent")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("LegacyRBACPresent = %+v, want True", cond)
	}
	for _, want := range []string{"ClusterRole/my-host", "ClusterRoleBinding/my-host-ns"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("condition message %q does not name %s", cond.Message, want)
		}
	}
}

func TestCleanupLegacy_DeletesAfterUpgrade(t *testing.T) {
	host := helmHost("my-host")
	var deletes []client.DeleteOptions
	funcs := &interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			o := client.DeleteOptions{}
			o.ApplyOptions(opts)
			deletes = append(deletes, o)
			return c.Delete(ctx, obj, opts...)
		},
	}
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), funcs, host)
	role, binding := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
	createObjects(t, fc, role, binding)
	ctx := context.Background()

	// Before the upgrade the condition is raised ...
	if _, err := r.cleanupLegacyRBAC(ctx, host); err != nil {
		t.Fatalf("cleanupLegacyRBAC: %v", err)
	}
	if !meta.IsStatusConditionTrue(host.Status.Conditions, "LegacyRBACPresent") {
		t.Fatal("LegacyRBACPresent not raised before the upgrade")
	}

	// ... and the rolled-out upgrade lets the cleanup go ahead.
	upgraded(t, fc, "my-host", "ns")
	if _, err := r.cleanupLegacyRBAC(ctx, host); err != nil {
		t.Fatalf("cleanupLegacyRBAC: %v", err)
	}
	assertDeleted(t, fc, legacyClusterRole("my-host", "ns"))
	assertDeleted(t, fc, legacyClusterRoleBinding("my-host", "ns"))
	if cond := meta.FindStatusCondition(host.Status.Conditions, "LegacyRBACPresent"); cond != nil {
		t.Errorf("LegacyRBACPresent = %+v, want it cleared", cond)
	}
	// Each delete is pinned to the object it matched.
	if len(deletes) != 2 {
		t.Fatalf("got %d deletes, want 2", len(deletes))
	}
	wantUIDs := []types.UID{role.UID, binding.UID}
	for i, d := range deletes {
		if d.Preconditions == nil || d.Preconditions.UID == nil || *d.Preconditions.UID != wantUIDs[i] {
			t.Errorf("delete %d preconditions = %+v, want UID %s", i, d.Preconditions, wantUIDs[i])
		}
	}
}

// Same-named hosts in two namespaces: host rsi in a must never delete the
// legacy objects of rsi in b, even with its own upgrade done.
func TestCleanupLegacy_SameNameOtherNamespaceUntouched(t *testing.T) {
	hostA, hostB := hostIn("rsi", "a"), hostIn("rsi", "b")
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, hostA, hostB)
	role, binding := legacyClusterRole("rsi", "b"), legacyClusterRoleBinding("rsi", "b")
	createObjects(t, fc, role, binding)
	upgraded(t, fc, "rsi", "a")

	if _, err := r.cleanupLegacyRBAC(context.Background(), hostA); err != nil {
		t.Fatalf("cleanupLegacyRBAC: %v", err)
	}
	assertExists(t, fc, role)
	assertExists(t, fc, binding)
	if cond := meta.FindStatusCondition(hostA.Status.Conditions, "LegacyRBACPresent"); cond != nil {
		t.Errorf("host a LegacyRBACPresent = %+v, want none: nothing of a's remains", cond)
	}
}

// The binding name <host>-<ns> is ambiguous: host rsi in a-b and host rsi-a in
// b both yield rsi-a-b. Only the Helm annotations and the roleRef tell them
// apart.
func TestLegacyRBAC_BindingNameCollisionAcrossHosts(t *testing.T) {
	host := hostIn("rsi", "a-b")
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	others := legacyClusterRoleBinding("rsi-a", "b")
	createObjects(t, fc, others)

	role, binding, err := r.legacyRBAC(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	if role != nil || binding != nil {
		t.Errorf("legacyRBAC = %v, %v; want neither: %s belongs to rsi-a in b", role, binding, others.Name)
	}
}

func TestLegacyRBAC_MatchesOnlyItsRelease(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*rbacv1.ClusterRole, *rbacv1.ClusterRoleBinding)
	}{
		{"no annotations", func(cr *rbacv1.ClusterRole, crb *rbacv1.ClusterRoleBinding) {
			cr.Annotations, crb.Annotations = nil, nil
		}},
		{"another release name", func(cr *rbacv1.ClusterRole, crb *rbacv1.ClusterRoleBinding) {
			cr.Annotations["meta.helm.sh/release-name"] = "other"
			crb.Annotations["meta.helm.sh/release-name"] = "other"
		}},
		{"another release namespace", func(cr *rbacv1.ClusterRole, crb *rbacv1.ClusterRoleBinding) {
			cr.Annotations["meta.helm.sh/release-namespace"] = "other"
			crb.Annotations["meta.helm.sh/release-namespace"] = "other"
		}},
		{"binding to a Role", func(cr *rbacv1.ClusterRole, crb *rbacv1.ClusterRoleBinding) {
			cr.Annotations = nil
			crb.RoleRef.Kind = "Role"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := helmHost("my-host")
			r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
			cr, crb := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
			tc.mutate(cr, crb)
			createObjects(t, fc, cr, crb)
			role, binding, err := r.legacyRBAC(context.Background(), host)
			if err != nil {
				t.Fatal(err)
			}
			if role != nil || binding != nil {
				t.Errorf("legacyRBAC = %v, %v; want neither", role, binding)
			}
		})
	}
}

// The cleanup is outside the release-hash gate: a host whose release is
// healthy and unchanged starts no helm run, and is still cleaned up -- and the
// condition reaches the host's status through Reconcile's own patch.
func TestCleanupLegacy_RunsWhenHashUnchanged(t *testing.T) {
	host := hostWithRecordedSA("my-host", HelmStatusCompleted)
	mock := NewMockHelmClient(nil)
	r, fc := newHelmReconciler(t, mock, nil, host)
	reconcilable(t, fc)
	// Hashed as the cluster holds the host, which is what Reconcile hashes.
	stored := getHost(t, fc, "my-host")
	hash, err := r.computeHelmReleaseHash(stored)
	if err != nil {
		t.Fatal(err)
	}
	stored.Status.Attributes[AttributeHelmReleaseHash] = hash
	if err := fc.Status().Update(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	role, binding := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
	createObjects(t, fc, role, binding)
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !meta.IsStatusConditionTrue(getHost(t, fc, "my-host").Status.Conditions, "LegacyRBACPresent") {
		t.Error("LegacyRBACPresent not on the host's status before the upgrade")
	}

	upgraded(t, fc, "my-host", "ns")
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := mock.Installed(); len(got) != 0 {
		t.Fatalf("installed %v: the hash gate did not hold, so the test proves nothing", got)
	}
	assertDeleted(t, fc, role)
	assertDeleted(t, fc, binding)
	if cond := meta.FindStatusCondition(getHost(t, fc, "my-host").Status.Conditions, "LegacyRBACPresent"); cond != nil {
		t.Errorf("LegacyRBACPresent = %+v on the host's status, want it cleared", cond)
	}
}

// A cleanup that cannot read the legacy objects says so on the host and the
// reconcile carries on.
func TestCleanupLegacy_ErrorDoesNotBlockReconcile(t *testing.T) {
	host := hostWithRecordedSA("my-host", HelmStatusCompleted)
	funcs := &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*rbacv1.ClusterRole); ok {
				return apierrors.NewForbidden(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterroles"}, key.Name, nil)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), funcs, host)
	reconcilable(t, fc)
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if err := fc.Get(t.Context(), key, &kdexv1alpha1.KDexInternalHost{}); err != nil {
		t.Errorf("internal host: %v, want the reconcile to have gone on to write it", err)
	}
	cond := meta.FindStatusCondition(getHost(t, fc, "my-host").Status.Conditions, "LegacyRBACPresent")
	if cond == nil || cond.Status != metav1.ConditionUnknown || cond.Reason != "LegacyRBACReadFailed" {
		t.Errorf("LegacyRBACPresent = %+v, want Unknown, LegacyRBACReadFailed", cond)
	}
}

// A proof that cannot be read keeps the legacy RBAC, under a reason of its
// own: the legacy objects are known to be there.
func TestCleanupLegacy_ProofReadFailureKeepsLegacy(t *testing.T) {
	host := helmHost("my-host")
	funcs := &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*rbacv1.RoleBinding); ok {
				return apierrors.NewForbidden(schema.GroupResource{Group: rbacv1.GroupName, Resource: "rolebindings"}, key.Name, nil)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), funcs, host)
	role, binding := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
	createObjects(t, fc, role, binding)
	upgraded(t, fc, "my-host", "ns")

	if _, err := r.cleanupLegacyRBAC(context.Background(), host); err == nil {
		t.Error("cleanupLegacyRBAC succeeded without its proof")
	}
	assertExists(t, fc, role)
	assertExists(t, fc, binding)
	cond := meta.FindStatusCondition(host.Status.Conditions, "LegacyRBACPresent")
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "UpgradeProofReadFailed" {
		t.Errorf("LegacyRBACPresent = %+v, want True, UpgradeProofReadFailed", cond)
	}
}

// The new RoleBinding alone is not the proof: until host-manager's Deployment
// has rolled out, a pod may still run on the legacy grant.
func TestCleanupLegacy_WaitsForRollout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deploy func() *appsv1.Deployment
	}{
		{"no deployment", nil},
		{"spec not yet observed", func() *appsv1.Deployment {
			d := rolledOutDeployment("my-host", "ns")
			d.Status.ObservedGeneration = 1
			return d
		}},
		{"replicas not yet updated", func() *appsv1.Deployment {
			d := rolledOutDeployment("my-host", "ns")
			d.Spec.Replicas = ptr.To[int32](2)
			d.Status.Replicas = 2
			return d
		}},
		{"old replicas still terminating", func() *appsv1.Deployment {
			d := rolledOutDeployment("my-host", "ns")
			d.Status.Replicas = 2
			return d
		}},
		{"none available", func() *appsv1.Deployment {
			d := rolledOutDeployment("my-host", "ns")
			d.Status.AvailableReplicas = 0
			return d
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := helmHost("my-host")
			r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
			role, binding := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
			createObjects(t, fc, role, binding, chartRoleBinding("my-host", "ns"))
			if tc.deploy != nil {
				createObjects(t, fc, tc.deploy())
			}

			if _, err := r.cleanupLegacyRBAC(context.Background(), host); err != nil {
				t.Fatalf("cleanupLegacyRBAC: %v", err)
			}
			assertExists(t, fc, role)
			assertExists(t, fc, binding)
			if !meta.IsStatusConditionTrue(host.Status.Conditions, "LegacyRBACPresent") {
				t.Error("LegacyRBACPresent not True while the rollout is incomplete")
			}
		})
	}
}

// With HOST_MANAGER_CLUSTER_ROLE set, the legacy chart rendered no
// ClusterRole/<host>: its binding bound the shared role. The binding is the
// release's and goes; the shared role is not, and stays.
func TestCleanupLegacy_BindingToASharedRole(t *testing.T) {
	host := helmHost("my-host")
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	shared := &rbacv1.ClusterRole{ObjectMeta: helmOwnedMeta("kcnas-operator-host-manager", "", "kcnas-operator", "kcnas-system")}
	binding := legacyClusterRoleBinding("my-host", "ns")
	binding.RoleRef.Name = shared.Name
	createObjects(t, fc, shared, binding)
	upgraded(t, fc, "my-host", "ns")

	if _, err := r.cleanupLegacyRBAC(context.Background(), host); err != nil {
		t.Fatalf("cleanupLegacyRBAC: %v", err)
	}
	assertDeleted(t, fc, binding)
	assertExists(t, fc, shared)
	if cond := meta.FindStatusCondition(host.Status.Conditions, "LegacyRBACPresent"); cond != nil {
		t.Errorf("LegacyRBACPresent = %+v, want it removed", cond)
	}
}

// identityHelmClients hands out one Helm client per identity, each counting
// its own uninstalls over one shared set of releases.
type identityHelmClients struct {
	mu        sync.Mutex
	releases  *MockHelmClient
	installer *uninstallCountingHelmClient
	operator  *uninstallCountingHelmClient
}

func (f *identityHelmClients) install(r *KDexHostReconciler) {
	r.HelmClientFactory = func(_ string, _ kdexv1alpha1.Secrets, _ logr.Logger, identity utils.HelmIdentity) (utils.HelmClientInterface, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if identity == utils.HelmAsOperator {
			return f.operator, nil
		}
		return f.installer, nil
	}
}

func newIdentityHelmClients(releases ...string) *identityHelmClients {
	labels := map[string]map[string]string{}
	for _, rel := range releases {
		labels[rel] = nil
	}
	shared := NewMockHelmClient(labels)
	return &identityHelmClients{
		releases:  shared,
		installer: &uninstallCountingHelmClient{MockHelmClient: shared},
		operator:  &uninstallCountingHelmClient{MockHelmClient: shared},
	}
}

// A host never upgraded under the new nexus still has the legacy ClusterRole
// in its release record. The installer cannot Get it, so its uninstall would
// fail on Forbidden: the host-manager release is uninstalled as the operator,
// the legacy objects left are then deleted, and the finalizer released.
func TestTeardown_LegacyRecordUninstallsAsOperator(t *testing.T) {
	r, fc, _, _ := newTeardownFixture(t)
	clients := newIdentityHelmClients("my-host")
	clients.releases.recordObjects("my-host", legacyRecord("my-host", "ns")...)
	clients.install(r)
	role, binding := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
	createObjects(t, fc, role, binding)
	deleteHost(t, fc, "my-host")

	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(clients.operator.calls, []string{"my-host"}) {
		t.Errorf("operator uninstalls = %v, want [my-host]", clients.operator.calls)
	}
	if slices.Contains(clients.installer.calls, "my-host") {
		t.Errorf("installer uninstalls = %v, want the host release left to the operator", clients.installer.calls)
	}
	assertDeleted(t, fc, role)
	assertDeleted(t, fc, binding)
	if err := fc.Get(context.Background(), key, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host get = %v, want it finalized", err)
	}
	assertInstallerGone(t, fc)
}

// A failed operator uninstall keeps both the legacy objects -- the record
// still lists them -- and the finalizer.
func TestTeardown_LegacyKeptWhenOperatorUninstallFails(t *testing.T) {
	r, fc, _, _ := newTeardownFixture(t)
	clients := newIdentityHelmClients("my-host")
	clients.releases.recordObjects("my-host", legacyRecord("my-host", "ns")...)
	clients.releases.failUninstall = map[string]struct{}{"my-host": {}}
	clients.install(r)
	role, binding := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
	createObjects(t, fc, role, binding)
	deleteHost(t, fc, "my-host")

	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("Reconcile succeeded over a failed uninstall")
	}
	assertExists(t, fc, role)
	assertExists(t, fc, binding)
	if !controllerutil.ContainsFinalizer(getHost(t, fc, "my-host"), hostFinalizerName) {
		t.Error("finalizer released over a failed uninstall")
	}
}

// A terminating namespace removes the release, but not the cluster-scoped
// legacy objects: teardown deletes them, or nothing ever would.
func TestTeardown_NamespaceTerminatingDeletesLegacy(t *testing.T) {
	r, fc, mock, _ := newTeardownFixture(t)
	role, binding := legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")
	createObjects(t, fc, role, binding)
	terminateNamespace(t, fc, "ns")
	deleteHost(t, fc, "my-host")

	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(mock.calls) != 0 {
		t.Errorf("Uninstall called for %v in a terminating namespace, want no call", mock.calls)
	}
	assertDeleted(t, fc, role)
	assertDeleted(t, fc, binding)
	if err := fc.Get(context.Background(), key, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host get = %v, want it finalized", err)
	}
}

// The record decides, not the cluster: an operator uninstall that deleted the
// legacy objects and then failed on another keeps the record whole, and the
// next pass -- finding no legacy object -- still uninstalls as the operator.
func TestTeardown_RetriesAsOperatorAfterAPartialUninstall(t *testing.T) {
	r, fc, _, _ := newTeardownFixture(t)
	clients := newIdentityHelmClients("my-host")
	clients.releases.recordObjects("my-host", legacyRecord("my-host", "ns")...)
	failed := false
	clients.operator.before = func(string) error {
		if failed {
			return nil
		}
		failed = true
		for _, obj := range []client.Object{legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")} {
			if err := fc.Delete(context.Background(), obj); err != nil {
				t.Errorf("delete %s: %v", obj.GetName(), err)
			}
		}
		return errors.New("timed out waiting for the Deployment's deletion")
	}
	clients.install(r)
	createObjects(t, fc, legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns"))
	deleteHost(t, fc, "my-host")
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("Reconcile succeeded over a failed uninstall")
	}
	if !controllerutil.ContainsFinalizer(getHost(t, fc, "my-host"), hostFinalizerName) {
		t.Fatal("finalizer released over a failed uninstall")
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if !slices.Equal(clients.operator.calls, []string{"my-host", "my-host"}) {
		t.Errorf("operator uninstalls = %v, want my-host twice", clients.operator.calls)
	}
	if slices.Contains(clients.installer.calls, "my-host") {
		t.Errorf("installer uninstalls = %v, want the host release left to the operator", clients.installer.calls)
	}
	if err := fc.Get(context.Background(), key, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host get = %v, want it finalized", err)
	}
}

// The record is a Secret anyone writing Secrets in the namespace can forge,
// and an operator uninstall deletes what it lists. The operator is used only
// for the host's own legacy RBAC; anything else outside the namespace is
// refused.
func TestHostReleaseUninstallIdentity(t *testing.T) {
	foreignRole := legacyClusterRole("my-host", "ns")
	foreignRole.Annotations = nil
	for _, tc := range []struct {
		name    string
		record  []utils.ReleaseObject
		live    []client.Object
		want    utils.HelmIdentity
		wantErr bool
	}{
		{name: "nothing recorded", want: utils.HelmAsInstaller},
		{name: "namespaced only", record: []utils.ReleaseObject{
			{Kind: "RoleBinding", Name: "my-host", Namespace: "ns"}, {Kind: "Deployment", Name: "my-host"},
		}, want: utils.HelmAsInstaller},
		{name: "legacy RBAC, live", record: legacyRecord("my-host", "ns"),
			live: []client.Object{legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns")},
			want: utils.HelmAsOperator},
		{name: "legacy RBAC, deleted by hand", record: legacyRecord("my-host", "ns"), want: utils.HelmAsOperator},
		{name: "legacy RBAC name held by another object", record: legacyRecord("my-host", "ns"),
			live: []client.Object{foreignRole}, wantErr: true},
		{name: "another ClusterRole", record: []utils.ReleaseObject{{Kind: "ClusterRole", Name: "cluster-admin"}}, wantErr: true},
		{name: "a CRD", record: []utils.ReleaseObject{{Kind: "CustomResourceDefinition", Name: "kdexhosts.kdex.dev"}}, wantErr: true},
		{name: "another namespace", record: []utils.ReleaseObject{{Kind: "Deployment", Name: "coredns", Namespace: "kube-system"}}, wantErr: true},
		{name: "a List", record: []utils.ReleaseObject{{Kind: "List"}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := helmHost("my-host")
			mock := NewMockHelmClient(map[string]map[string]string{"my-host": nil})
			mock.recordObjects("my-host", tc.record...)
			r, fc := newHelmReconciler(t, mock, nil, host)
			createObjects(t, fc, tc.live...)

			got, err := r.hostReleaseUninstallIdentity(context.Background(), mock, host)
			if tc.wantErr {
				if err == nil {
					t.Errorf("identity = %v, want a refusal", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("identity = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// A refused record keeps the finalizer and uninstalls nothing as the operator.
func TestTeardown_ForgedRecordIsRefused(t *testing.T) {
	r, fc, _, _ := newTeardownFixture(t)
	clients := newIdentityHelmClients("my-host")
	clients.releases.recordObjects("my-host", utils.ReleaseObject{Kind: "ClusterRole", Name: "cluster-admin"})
	clients.install(r)
	deleteHost(t, fc, "my-host")
	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
		t.Fatal("Reconcile succeeded over a forged record")
	}
	if len(clients.operator.calls) != 0 || len(clients.installer.calls) != 0 {
		t.Errorf("uninstalls operator=%v installer=%v, want none", clients.operator.calls, clients.installer.calls)
	}
	if !controllerutil.ContainsFinalizer(getHost(t, fc, "my-host"), hostFinalizerName) {
		t.Error("finalizer released over a forged record")
	}
}

// Against a real API server, which enforces delete preconditions (the fake
// client does not check a UID): a legacy object deleted and re-created after
// it was matched is a different object, and is not deleted blindly.
var _ = Describe("Legacy RBAC cleanup against the API server", func() {
	It("does not delete a ClusterRole re-created since it was matched", func() {
		host := &kdexv1alpha1.KDexHost{ObjectMeta: metav1.ObjectMeta{Name: "legacy-uid", Namespace: "legacy-uid-ns"}}
		newRole := func() *rbacv1.ClusterRole {
			cr := legacyClusterRole(host.Name, host.Namespace)
			cr.UID = ""
			return cr
		}
		Expect(k8sClient.Create(ctx, newRole())).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, newRole())
		})

		role, binding, err := hostReconciler.legacyRBAC(ctx, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(role).NotTo(BeNil())
		Expect(binding).To(BeNil())

		Expect(k8sClient.Delete(ctx, newRole())).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: host.Name}, &rbacv1.ClusterRole{}))
		}).Should(BeTrue())
		Expect(k8sClient.Create(ctx, newRole())).To(Succeed())

		Expect(hostReconciler.deleteLegacyRBAC(ctx, role, binding)).NotTo(Succeed())
		recreated := &rbacv1.ClusterRole{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: host.Name}, recreated)).To(Succeed())
		Expect(recreated.UID).NotTo(Equal(role.UID))

		// Matched afresh, it goes.
		role, binding, err = hostReconciler.legacyRBAC(ctx, host)
		Expect(err).NotTo(HaveOccurred())
		Expect(hostReconciler.deleteLegacyRBAC(ctx, role, binding)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: host.Name}, &rbacv1.ClusterRole{}))
		}).Should(BeTrue())
	})
})

// newSettledHost is a host whose reconcile settles -- release healthy and
// unchanged, host-manager's Deployment Available -- so any requeue its
// Reconcile returns is the legacy cleanup's. deploy is that Deployment.
func newSettledHost(t *testing.T, funcs *interceptor.Funcs, deploy *appsv1.Deployment) (*KDexHostReconciler, client.Client, types.NamespacedName) {
	t.Helper()
	host := hostWithRecordedSA("my-host", HelmStatusCompleted)
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), funcs, host)
	r.RequeueDelay = 7 * time.Second
	reconcilable(t, fc)
	stored := getHost(t, fc, "my-host")
	hash, err := r.computeHelmReleaseHash(stored)
	if err != nil {
		t.Fatal(err)
	}
	stored.Status.Attributes[AttributeHelmReleaseHash] = hash
	if err := fc.Status().Update(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	// Available stays True through a rolling update.
	deploy.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
	createObjects(t, fc, chartRoleBinding("my-host", "ns"), deploy)
	return r, fc, types.NamespacedName{Namespace: "ns", Name: "my-host"}
}

// rollingDeployment is host-manager's Deployment mid-rollout: Available, its
// new spec not yet observed.
func rollingDeployment() *appsv1.Deployment {
	d := rolledOutDeployment("my-host", "ns")
	d.Status.ObservedGeneration = 1
	return d
}

// Nothing watches the Deployment, and Available stays True through the
// rollout, so no event follows its end: the wait requeues itself, or the
// legacy grant outlives the rollout until something unrelated wakes the host.
func TestCleanupLegacy_RequeuesWhileWaitingForRollout(t *testing.T) {
	r, fc, key := newSettledHost(t, nil, rollingDeployment())
	createObjects(t, fc, legacyClusterRole("my-host", "ns"), legacyClusterRoleBinding("my-host", "ns"))

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != r.RequeueDelay {
		t.Errorf("RequeueAfter = %v, want %v while the cleanup waits", res.RequeueAfter, r.RequeueDelay)
	}

	// The rollout ends, with no event: the requeued pass cleans up and stops
	// requeueing.
	d := &appsv1.Deployment{}
	if err := fc.Get(t.Context(), key, d); err != nil {
		t.Fatal(err)
	}
	d.Status.ObservedGeneration = d.Generation
	if err := fc.Status().Update(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	res, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	assertDeleted(t, fc, legacyClusterRole("my-host", "ns"))
	assertDeleted(t, fc, legacyClusterRoleBinding("my-host", "ns"))
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v after the cleanup, want none", res.RequeueAfter)
	}
}

func TestCleanupLegacy_NoRequeueWithoutLegacy(t *testing.T) {
	r, _, key := newSettledHost(t, nil, rollingDeployment())
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v with no legacy RBAC, want none", res.RequeueAfter)
	}
}

// A legacy read that failed leaves the RBAC possibly there: look again.
func TestCleanupLegacy_RequeuesAfterAFailedRead(t *testing.T) {
	funcs := &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*rbacv1.ClusterRole); ok {
				return apierrors.NewForbidden(schema.GroupResource{Group: rbacv1.GroupName, Resource: "clusterroles"}, key.Name, nil)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	r, _, key := newSettledHost(t, funcs, rolledOutDeployment("my-host", "ns"))
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != r.RequeueDelay {
		t.Errorf("RequeueAfter = %v, want %v after a failed legacy read", res.RequeueAfter, r.RequeueDelay)
	}
}

// The cleanup's requeue joins whatever the rest of Reconcile returns: the
// sooner non-zero requeue wins, and an error is never traded for a requeue.
func TestRequeueForLegacyRBAC(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		res  ctrl.Result
		err  error
		want ctrl.Result
	}{
		{"no requeue", ctrl.Result{}, nil, ctrl.Result{RequeueAfter: 7 * time.Second}},
		{"a later requeue", ctrl.Result{RequeueAfter: time.Minute}, nil, ctrl.Result{RequeueAfter: 7 * time.Second}},
		{"a sooner requeue", ctrl.Result{RequeueAfter: time.Second}, nil, ctrl.Result{RequeueAfter: time.Second}},
		{"an error", ctrl.Result{}, boom, ctrl.Result{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requeueForLegacyRBAC(tc.res, tc.err, 7*time.Second); got != tc.want {
				t.Errorf("requeueForLegacyRBAC = %+v, want %+v", got, tc.want)
			}
		})
	}
}
