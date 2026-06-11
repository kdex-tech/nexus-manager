package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func defaultSecretTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add kdex scheme: %v", err)
	}
	return scheme
}

func secret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// TestDefaultSecretRequests covers the cluster-default npm credential watch
// (kdex-tech/nexus-manager#4): a change to the configured default Secret must
// requeue every dependent resource, while unrelated Secret changes and an
// unconfigured default are no-ops.
func TestDefaultSecretRequests(t *testing.T) {
	scheme := defaultSecretTestScheme(t)

	libA := &kdexv1alpha1.KDexScriptLibrary{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns1"}}
	libB := &kdexv1alpha1.KDexScriptLibrary{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns2"}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(libA, libB).
		Build()

	t.Run("matches the configured default secret and enqueues all items", func(t *testing.T) {
		defaultRef := &kdexv1alpha1.KDexObjectReference{Kind: "Secret", Name: "cluster-npm", Namespace: "operator"}
		got := defaultSecretRequests(
			context.Background(), c, &kdexv1alpha1.KDexScriptLibraryList{}, defaultRef,
			secret("cluster-npm", "operator"),
		)
		if len(got) != 2 {
			t.Fatalf("expected 2 requests, got %d (%v)", len(got), got)
		}
	})

	t.Run("ignores a secret whose name does not match", func(t *testing.T) {
		defaultRef := &kdexv1alpha1.KDexObjectReference{Kind: "Secret", Name: "cluster-npm", Namespace: "operator"}
		got := defaultSecretRequests(
			context.Background(), c, &kdexv1alpha1.KDexScriptLibraryList{}, defaultRef,
			secret("some-other-secret", "operator"),
		)
		if got != nil {
			t.Fatalf("expected nil for non-matching name, got %v", got)
		}
	})

	t.Run("ignores a name match in the wrong namespace when namespace is explicit", func(t *testing.T) {
		defaultRef := &kdexv1alpha1.KDexObjectReference{Kind: "Secret", Name: "cluster-npm", Namespace: "operator"}
		got := defaultSecretRequests(
			context.Background(), c, &kdexv1alpha1.KDexScriptLibraryList{}, defaultRef,
			secret("cluster-npm", "somewhere-else"),
		)
		if got != nil {
			t.Fatalf("expected nil for name match in wrong namespace, got %v", got)
		}
	})

	t.Run("name-only match when default ref omits namespace", func(t *testing.T) {
		defaultRef := &kdexv1alpha1.KDexObjectReference{Kind: "Secret", Name: "cluster-npm"}
		got := defaultSecretRequests(
			context.Background(), c, &kdexv1alpha1.KDexScriptLibraryList{}, defaultRef,
			secret("cluster-npm", "any-namespace"),
		)
		if len(got) != 2 {
			t.Fatalf("expected 2 requests for name-only match, got %d (%v)", len(got), got)
		}
	})

	t.Run("nil default ref is a no-op", func(t *testing.T) {
		got := defaultSecretRequests(
			context.Background(), c, &kdexv1alpha1.KDexScriptLibraryList{}, nil,
			secret("cluster-npm", "operator"),
		)
		if got != nil {
			t.Fatalf("expected nil when no default secret is configured, got %v", got)
		}
	})
}
