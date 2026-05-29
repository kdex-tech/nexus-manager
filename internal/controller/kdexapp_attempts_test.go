package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/npm"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestKDexAppResetsValidationAttemptsOnSuccess is a regression test for
// https://github.com/kdex-tech/nexus-manager/issues/8 - the validation.attempts
// counter was never cleared after a successful validation, so a later transient
// failure resumed from a stale count and could exhaust the retry budget early.
func TestKDexAppResetsValidationAttemptsOnSuccess(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add kdex scheme: %v", err)
	}

	app := &kdexv1alpha1.KDexApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexAppSpec{
			PackageReference: kdexv1alpha1.PackageReference{
				Name:    "@scope/widget",
				Version: "1.0.0",
			},
		},
		Status: kdexv1alpha1.KDexObjectStatus{
			// Simulate a counter left over from a prior failure episode.
			Attributes: map[string]string{"validation.attempts": "7"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(&kdexv1alpha1.KDexApp{}).
		Build()

	r := &KDexAppReconciler{
		Client: c,
		Scheme: scheme,
		PackageValidatorFactory: func(_ string, _ *corev1.Secret) (npm.PackageValidator, error) {
			return &MockRegistry{}, nil
		},
		RequeueDelay: 0,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "app", Namespace: "ns"},
	})
	if err != nil {
		t.Fatalf("reconcile returned unexpected error: %v", err)
	}

	got := &kdexv1alpha1.KDexApp{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "app", Namespace: "ns"}, got); err != nil {
		t.Fatalf("failed to get app after reconcile: %v", err)
	}

	if v, ok := got.Status.Attributes["validation.attempts"]; ok {
		t.Fatalf("expected validation.attempts to be cleared after a successful validation, got %q", v)
	}
}
