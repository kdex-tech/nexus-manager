package controller

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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

// reconcileTeardown reconciles the terminating host my-host in ns once.
func reconcileTeardown(t *testing.T, r *KDexHostReconciler) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "my-host"}})
	return err
}

func assertFinalized(t *testing.T, c client.Client) {
	t.Helper()
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "my-host"}, &kdexv1alpha1.KDexHost{}); !apierrors.IsNotFound(err) {
		t.Errorf("host get = %v, want it finalized", err)
	}
}

// withHostIndexes registers the indexes the host reconciler lists by -- its
// teardown's internal pages and translations, a full pass's attached
// translations and extensions -- as SetupWithManager does. b must already
// have its scheme.
func withHostIndexes(b *fake.ClientBuilder) *fake.ClientBuilder {
	return b.
		WithIndex(&kdexv1alpha1.KDexInternalUtilityPage{}, hostIndexKey, indexInternalUtilityPageByHost).
		WithIndex(&kdexv1alpha1.KDexInternalTranslation{}, hostIndexKey, indexInternalTranslationByHost).
		WithIndex(&kdexv1alpha1.KDexTranslation{}, hostIndexKey, indexTranslationByHostRef).
		WithIndex(&kdexv1alpha1.KDexHostExtension{}, hostIndexKey, indexExtensionByHostRef)
}

// hookedHelmClient runs its hooks around a MockHelmClient's calls, standing in
// for whatever else happens in the cluster meanwhile, and records every
// install it was asked for -- whether or not it then went ahead -- and every
// listing.
type hookedHelmClient struct {
	*MockHelmClient
	afterInstall func(releaseName string)
	onUninstall  func(releaseName string)

	mu        sync.Mutex
	attempted []string
	lists     atomic.Int32
}

func (h *hookedHelmClient) InstallOrUpgrade(spec *utils.ChartSpec) error {
	h.mu.Lock()
	h.attempted = append(h.attempted, spec.ReleaseName)
	h.mu.Unlock()
	err := h.MockHelmClient.InstallOrUpgrade(spec)
	if h.afterInstall != nil {
		h.afterInstall(spec.ReleaseName)
	}
	return err
}

func (h *hookedHelmClient) Uninstall(releaseName string) error {
	if h.onUninstall != nil {
		h.onUninstall(releaseName)
	}
	return h.MockHelmClient.Uninstall(releaseName)
}

func (h *hookedHelmClient) ListReleases(labels map[string]string) ([]string, error) {
	h.lists.Add(1)
	return h.MockHelmClient.ListReleases(labels)
}

// Attempted is every release an install was asked for, in order.
func (h *hookedHelmClient) Attempted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.attempted)
}

// hostNamespace is a namespace an administrator enabled for KDex hosts.
func hostNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name,
		Labels: map[string]string{LabelHostNamespace: HostNamespaceEnabled}}}
}
