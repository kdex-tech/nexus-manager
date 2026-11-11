package controller

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	installerSAKey = client.ObjectKey{Namespace: "ns", Name: "kdex-installer"}
	installerRBKey = client.ObjectKey{Namespace: "ns", Name: "kdex-installer"}
)

// identityRecordingFactory makes r's Helm clients mock, recording the identity
// each was asked for.
type identityRecordingFactory struct {
	mu   sync.Mutex
	seen []utils.HelmIdentity
}

func (f *identityRecordingFactory) install(r *KDexHostReconciler, mock utils.HelmClientInterface) {
	r.HelmClientFactory = func(_ string, _ kdexv1alpha1.Secrets, _ logr.Logger, identity utils.HelmIdentity) (utils.HelmClientInterface, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.seen = append(f.seen, identity)
		return mock, nil
	}
}

func (f *identityRecordingFactory) identities() []utils.HelmIdentity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.seen)
}

// uninstallCountingHelmClient counts every Uninstall call, whether or not the
// release exists.
type uninstallCountingHelmClient struct {
	*MockHelmClient
	mu    sync.Mutex
	calls []string
}

func (u *uninstallCountingHelmClient) Uninstall(name string) error {
	u.mu.Lock()
	u.calls = append(u.calls, name)
	u.mu.Unlock()
	return u.MockHelmClient.Uninstall(name)
}

// terminateNamespace begins the deletion of namespace ns, held open by a
// finalizer as the namespace controller holds a namespace with content.
func terminateNamespace(t *testing.T, c client.Client, ns string) {
	t.Helper()
	ctx := context.Background()
	n := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: ns}, n); err != nil {
		t.Fatal(err)
	}
	n.Finalizers = append(n.Finalizers, "kubernetes")
	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func assertInstallerPresent(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, installerSAKey, sa); err != nil {
		t.Fatalf("installer ServiceAccount: %v", err)
	}
	if sa.Labels["app.kubernetes.io/managed-by"] != "kcnas-operator" {
		t.Errorf("ServiceAccount labels = %v, want app.kubernetes.io/managed-by=kcnas-operator", sa.Labels)
	}
	if len(sa.OwnerReferences) != 0 {
		t.Errorf("ServiceAccount ownerReferences = %v, want none", sa.OwnerReferences)
	}
	rb := &rbacv1.RoleBinding{}
	if err := c.Get(ctx, installerRBKey, rb); err != nil {
		t.Fatalf("installer RoleBinding: %v", err)
	}
	if rb.Labels["app.kubernetes.io/managed-by"] != "kcnas-operator" {
		t.Errorf("RoleBinding labels = %v, want app.kubernetes.io/managed-by=kcnas-operator", rb.Labels)
	}
	if !slices.Equal(rb.Finalizers, []string{"kdex.dev/installer"}) {
		t.Errorf("RoleBinding finalizers = %v, want [kdex.dev/installer]", rb.Finalizers)
	}
	if len(rb.OwnerReferences) != 0 {
		t.Errorf("RoleBinding ownerReferences = %v, want none", rb.OwnerReferences)
	}
	wantRef := rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "kcnas-operator-installer"}
	if rb.RoleRef != wantRef {
		t.Errorf("RoleBinding roleRef = %v, want %v", rb.RoleRef, wantRef)
	}
	wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: "kdex-installer", Namespace: "ns"}}
	if !slices.Equal(rb.Subjects, wantSubjects) {
		t.Errorf("RoleBinding subjects = %v, want %v", rb.Subjects, wantSubjects)
	}
}

func assertInstallerGone(t *testing.T, c client.Client) {
	t.Helper()
	ctx := context.Background()
	if err := c.Get(ctx, installerSAKey, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Errorf("installer ServiceAccount get = %v, want NotFound", err)
	}
	if err := c.Get(ctx, installerRBKey, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Errorf("installer RoleBinding get = %v, want NotFound", err)
	}
}

func TestEnsureInstaller_Idempotent(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	ctx := context.Background()
	for range 2 {
		if err := r.ensureInstaller(ctx, "ns"); err != nil {
			t.Fatal(err)
		}
	}
	assertInstallerPresent(t, fc)

	var sas corev1.ServiceAccountList
	if err := fc.List(ctx, &sas, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	var rbs rbacv1.RoleBindingList
	if err := fc.List(ctx, &rbs, client.InNamespace("ns")); err != nil {
		t.Fatal(err)
	}
	if len(sas.Items) != 1 || len(rbs.Items) != 1 {
		t.Errorf("got %d ServiceAccounts and %d RoleBindings, want one of each", len(sas.Items), len(rbs.Items))
	}
}

// An installer binding found with a borrowed subject, no finalizer or no label
// is healed: it grants the installer and nothing else.
func TestEnsureInstaller_HealsAnExistingBinding(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	ctx := context.Background()
	if err := fc.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kdex-installer", Namespace: "ns"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "kcnas-operator-installer"},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: "kdex-installer", Namespace: "ns"},
			{Kind: "ServiceAccount", Name: "borrower", Namespace: "ns"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureInstaller(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	assertInstallerPresent(t, fc)
}

// roleRef is immutable: a binding of the installer's name to another role is
// refused, not repurposed.
func TestEnsureInstaller_RefusesABindingToAnotherRole(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	ctx := context.Background()
	if err := fc.Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kdex-installer", Namespace: "ns"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "cluster-admin"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureInstaller(ctx, "ns"); err == nil {
		t.Fatal("want an error for an installer binding to another role")
	}
}

// A terminating namespace rejects creates: nothing is attempted, and the error
// stops a helm operation as a terminating host does.
func TestEnsureInstaller_NamespaceTerminatingCreatesNothing(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	terminateNamespace(t, fc, "ns")
	err := r.ensureInstaller(context.Background(), "ns")
	if err == nil {
		t.Fatal("want an error in a terminating namespace")
	}
	if !apierrors.IsNotFound(fc.Get(context.Background(), installerSAKey, &corev1.ServiceAccount{})) {
		t.Error("a ServiceAccount was created in a terminating namespace")
	}
	if got, err := r.namespaceTerminating(context.Background(), "ns"); err != nil || !got {
		t.Errorf("namespaceTerminating = %v, %v; want true", got, err)
	}
}

func TestReleaseInstaller_KeptForSiblingHost(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, helmHost("h1"), helmHost("h2"))
	ctx := context.Background()
	if err := r.ensureInstaller(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if err := r.releaseInstallerIfUnused(ctx, "ns", "h1"); err != nil {
		t.Fatal(err)
	}
	assertInstallerPresent(t, fc)
}

func TestReleaseInstaller_RemovedForLastHost(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, helmHost("h1"))
	ctx := context.Background()
	if err := r.ensureInstaller(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.releaseInstallerIfUnused(ctx, "ns", "h1"); err != nil {
			t.Fatal(err)
		}
	}
	assertInstallerGone(t, fc)
}

// A sibling that is itself terminating does not keep the installer: its own
// teardown re-creates it if it still has releases to uninstall.
func TestReleaseInstaller_TerminatingSiblingDoesNotKeepIt(t *testing.T) {
	h2 := helmHost("h2")
	h2.Finalizers = []string{hostFinalizerName}
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, helmHost("h1"), h2)
	ctx := context.Background()
	if err := r.ensureInstaller(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	deleteHost(t, fc, "h2")
	if err := r.releaseInstallerIfUnused(ctx, "ns", "h1"); err != nil {
		t.Fatal(err)
	}
	assertInstallerGone(t, fc)
}

// teardownHost is a host with a finalizer, an installed host-manager release
// and a recorded cluster-reader grant, in a namespace with an installer.
func teardownHost(t *testing.T) (*KDexHostReconciler, client.Client, *uninstallCountingHelmClient, *identityRecordingFactory) {
	t.Helper()
	host := helmHost("my-host")
	host.Finalizers = []string{hostFinalizerName}
	host.Status.Attributes = map[string]string{AttributeHostManagerServiceAccount: "my-host"}
	mock := &uninstallCountingHelmClient{MockHelmClient: NewMockHelmClient(map[string]map[string]string{"my-host": nil})}
	r, fc := newHelmReconciler(t, mock, nil, host)
	factory := &identityRecordingFactory{}
	factory.install(r, mock)
	ctx := context.Background()
	if err := r.ensureClusterReaderSubject(ctx, "ns", "my-host"); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureInstaller(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	return r, fc, mock, factory
}

func TestTeardown_NamespaceTerminatingSkipsUninstall(t *testing.T) {
	r, fc, mock, _ := teardownHost(t)
	terminateNamespace(t, fc, "ns")
	deleteHost(t, fc, "my-host")

	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(mock.calls) != 0 {
		t.Errorf("Uninstall called for %v in a terminating namespace, want no call", mock.calls)
	}
	if got := readClusterReaderSubjects(t, fc); len(got) != 0 {
		t.Errorf("cluster-reader subjects = %v, want the host's removed", got)
	}
	if err := fc.Get(context.Background(), key, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host get = %v, want it finalized", err)
	}
	// The installer's finalizer must not hold the namespace deletion open.
	assertInstallerGone(t, fc)
}

func TestTeardown_UninstallsAsInstallerThenReleasesIt(t *testing.T) {
	r, fc, mock, factory := teardownHost(t)
	deleteHost(t, fc, "my-host")

	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Contains(mock.Uninstalled(), "my-host") {
		t.Errorf("uninstalled %v, want my-host", mock.Uninstalled())
	}
	ids := factory.identities()
	if len(ids) == 0 || slices.ContainsFunc(ids, func(id utils.HelmIdentity) bool { return id != utils.HelmAsInstaller }) {
		t.Errorf("teardown Helm identities = %v, want only HelmAsInstaller", ids)
	}
	if got := readClusterReaderSubjects(t, fc); len(got) != 0 {
		t.Errorf("cluster-reader subjects = %v, want the host's removed", got)
	}
	if err := fc.Get(context.Background(), key, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host get = %v, want it finalized", err)
	}
	assertInstallerGone(t, fc)
}

// A host deleted before any helm run bootstrapped the installer (or after a
// sibling's teardown released it) still uninstalls: teardown re-creates the
// installer it impersonates rather than wedging on Forbidden.
func TestTeardown_BootstrapsAMissingInstaller(t *testing.T) {
	host := helmHost("my-host")
	host.Finalizers = []string{hostFinalizerName}
	mock := NewMockHelmClient(map[string]map[string]string{"my-host": nil})
	r, fc := newHelmReconciler(t, mock, nil, host, helmHost("sibling"))
	deleteHost(t, fc, "my-host")

	key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Contains(mock.Uninstalled(), "my-host") {
		t.Errorf("uninstalled %v, want my-host", mock.Uninstalled())
	}
	// The sibling still lives, so the installer stays.
	assertInstallerPresent(t, fc)
}

func TestHelmRun_UsesInstallerIdentity(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{"x": {LabelCompanionOf: "my-host"}})
	r, fc := newHelmReconciler(t, mock, nil, recorded(helmHost("my-host", "a"), "x"))
	factory := &identityRecordingFactory{}
	factory.install(r, mock)

	r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())

	got := getHost(t, fc, "my-host")
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusCompleted {
		t.Fatalf("helm status = %q (%s), want completed", s, got.Status.Attributes[AttributeHelmReleaseError])
	}
	if !slices.Contains(mock.Installed(), "my-host") || !slices.Contains(mock.Installed(), "a") {
		t.Errorf("installed %v, want my-host and a", mock.Installed())
	}
	if !slices.Contains(mock.Uninstalled(), "x") {
		t.Errorf("uninstalled %v, want the pruned x", mock.Uninstalled())
	}
	ids := factory.identities()
	if len(ids) == 0 || slices.ContainsFunc(ids, func(id utils.HelmIdentity) bool { return id != utils.HelmAsInstaller }) {
		t.Errorf("helm run identities = %v, want only HelmAsInstaller", ids)
	}
	assertInstallerPresent(t, fc)
}

// A run in a terminating namespace bootstraps nothing and installs nothing.
func TestHelmRun_NamespaceTerminatingInstallsNothing(t *testing.T) {
	mock := NewMockHelmClient(nil)
	r, fc := newHelmReconciler(t, mock, nil, helmHost("my-host"))
	terminateNamespace(t, fc, "ns")

	r.runAsyncHelmReconcile(context.Background(), "ns", "my-host", "hash", nil, logr.Discard())

	if got := mock.Installed(); len(got) != 0 {
		t.Errorf("installed %v in a terminating namespace, want nothing", got)
	}
	if !apierrors.IsNotFound(fc.Get(context.Background(), installerSAKey, &corev1.ServiceAccount{})) {
		t.Error("a ServiceAccount was created in a terminating namespace")
	}
}

func TestCacheOptions_LabelSelectedRBACKinds(t *testing.T) {
	opts := CacheOptions()
	var found []string
	for obj, by := range opts.ByObject {
		switch obj.(type) {
		case *corev1.ServiceAccount, *rbacv1.RoleBinding:
			if by.Label == nil || by.Label.String() != "app.kubernetes.io/managed-by=kcnas-operator" {
				t.Errorf("%T label selector = %v, want app.kubernetes.io/managed-by=kcnas-operator", obj, by.Label)
			}
			found = append(found, "label")
		case *rbacv1.ClusterRoleBinding:
			// Helm overwrites managed-by on the chart-shipped binding, so it
			// is selected by name.
			if by.Field == nil || by.Field.String() != "metadata.name=kcnas-operator-host-manager-cluster-reader" {
				t.Errorf("ClusterRoleBinding field selector = %v, want metadata.name=kcnas-operator-host-manager-cluster-reader", by.Field)
			}
			found = append(found, "field")
		case *corev1.Secret:
			if by.Field == nil || by.Field.String() != "type!=helm.sh/release.v1" {
				t.Errorf("Secret field selector = %v, want type!=helm.sh/release.v1", by.Field)
			}
			found = append(found, "secret")
		}
	}
	slices.Sort(found)
	if want := []string{"field", "label", "label", "secret"}; !slices.Equal(found, want) {
		t.Errorf("ByObject entries = %v, want %v", found, want)
	}
}

// Against a real API server and the manager's label-selected cache: an
// installer ServiceAccount that exists without the operator's label is
// invisible to the cache, so the create meets AlreadyExists and must adopt the
// live object rather than fail every run.
var _ = Describe("Installer bootstrap through the label-selected cache", func() {
	It("adopts an unlabelled ServiceAccount and releases both objects", func() {
		ns := "installer-adopt"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "kdex-installer", Namespace: ns},
		})).To(Succeed())

		Expect(hostReconciler.ensureInstaller(ctx, ns)).To(Succeed())
		Expect(hostReconciler.ensureInstaller(ctx, ns)).To(Succeed())

		sa := &corev1.ServiceAccount{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "kdex-installer"}, sa)).To(Succeed())
		Expect(sa.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "kcnas-operator"))
		rb := &rbacv1.RoleBinding{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "kdex-installer"}, rb)).To(Succeed())
		Expect(rb.Finalizers).To(ConsistOf("kdex.dev/installer"))
		Expect(rb.RoleRef.Name).To(Equal("kcnas-operator-installer"))

		// No KDexHost lives in the namespace.
		Expect(hostReconciler.releaseInstallerIfUnused(ctx, ns, "departing")).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "kdex-installer"}, &rbacv1.RoleBinding{})) &&
				apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "kdex-installer"}, &corev1.ServiceAccount{}))
		}).Should(BeTrue())
	})
})
