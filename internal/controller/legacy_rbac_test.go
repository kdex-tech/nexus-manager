package controller

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

	if err := r.cleanupLegacyRBAC(context.Background(), host); err != nil {
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
	if err := r.cleanupLegacyRBAC(ctx, host); err != nil {
		t.Fatalf("cleanupLegacyRBAC: %v", err)
	}
	if !meta.IsStatusConditionTrue(host.Status.Conditions, "LegacyRBACPresent") {
		t.Fatal("LegacyRBACPresent not raised before the upgrade")
	}

	// ... and the upgrade's RoleBinding lets the cleanup go ahead.
	createObjects(t, fc, chartRoleBinding("my-host", "ns"))
	if err := r.cleanupLegacyRBAC(ctx, host); err != nil {
		t.Fatalf("cleanupLegacyRBAC: %v", err)
	}
	assertDeleted(t, fc, legacyClusterRole("my-host", "ns"))
	assertDeleted(t, fc, legacyClusterRoleBinding("my-host", "ns"))
	if cond := meta.FindStatusCondition(host.Status.Conditions, "LegacyRBACPresent"); cond != nil && cond.Status == metav1.ConditionTrue {
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
	createObjects(t, fc, role, binding, chartRoleBinding("rsi", "a"))

	if err := r.cleanupLegacyRBAC(context.Background(), hostA); err != nil {
		t.Fatalf("cleanupLegacyRBAC: %v", err)
	}
	assertExists(t, fc, role)
	assertExists(t, fc, binding)
	if cond := meta.FindStatusCondition(hostA.Status.Conditions, "LegacyRBACPresent"); cond != nil && cond.Status == metav1.ConditionTrue {
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
		{"binding to another role", func(cr *rbacv1.ClusterRole, crb *rbacv1.ClusterRoleBinding) {
			cr.Annotations = nil
			crb.RoleRef.Name = "cluster-admin"
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

	createObjects(t, fc, chartRoleBinding("my-host", "ns"))
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := mock.Installed(); len(got) != 0 {
		t.Fatalf("installed %v: the hash gate did not hold, so the test proves nothing", got)
	}
	assertDeleted(t, fc, role)
	assertDeleted(t, fc, binding)
	if cond := meta.FindStatusCondition(getHost(t, fc, "my-host").Status.Conditions, "LegacyRBACPresent"); cond != nil && cond.Status == metav1.ConditionTrue {
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
	if cond == nil || cond.Status != metav1.ConditionUnknown {
		t.Errorf("LegacyRBACPresent = %+v, want Unknown", cond)
	}
}

// identityHelmClients hands out one Helm client per identity.
type identityHelmClients struct {
	mu        sync.Mutex
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
	seed := func() *uninstallCountingHelmClient {
		labels := map[string]map[string]string{}
		for _, rel := range releases {
			labels[rel] = nil
		}
		return &uninstallCountingHelmClient{MockHelmClient: NewMockHelmClient(labels)}
	}
	return &identityHelmClients{installer: seed(), operator: seed()}
}

// A host never upgraded under the new nexus still has the legacy ClusterRole
// in its release record. The installer cannot Get it, so its uninstall would
// fail on Forbidden: the host-manager release is uninstalled as the operator,
// the legacy objects are then deleted, and the finalizer released.
func TestTeardown_LegacyRecordUninstallsAsOperator(t *testing.T) {
	r, fc, _, _ := newTeardownFixture(t)
	clients := newIdentityHelmClients("my-host")
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
	clients.operator.failUninstall = map[string]struct{}{"my-host": {}}
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
