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

// TestKDexAppReconcileIsStableWhenSettled is a regression test for
// https://github.com/kdex-tech/nexus-manager/issues/31 - the KDexApp reconciler
// rewrote the status (bumping condition lastTransitionTime) on every pass even
// when nothing transitioned, so each pass bumped resourceVersion and re-fired
// the For(KDexApp) watch, producing a self-perpetuating reconcile storm that
// pegged downstream host-manager CPU.
//
// A settled KDexApp re-reconciled with no change must not issue another
// Status().Update() (its resourceVersion must stay put).
func TestKDexAppReconcileIsStableWhenSettled(t *testing.T) {
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

	key := types.NamespacedName{Name: "app", Namespace: "ns"}
	reconcile := func() {
		t.Helper()
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile returned unexpected error: %v", err)
		}
	}

	// First reconcile settles the app to Ready=True and writes status once.
	reconcile()

	settled := &kdexv1alpha1.KDexApp{}
	if err := c.Get(context.Background(), key, settled); err != nil {
		t.Fatalf("failed to get app after first reconcile: %v", err)
	}
	if !meta_IsReady(settled) {
		t.Fatalf("expected app to be Ready=True after first reconcile, conditions: %+v", settled.Status.Conditions)
	}
	settledRV := settled.ResourceVersion

	// Re-reconcile the already-settled app several times. Nothing has changed,
	// so no further status write (and no resourceVersion bump) must occur.
	for i := 0; i < 3; i++ {
		reconcile()

		got := &kdexv1alpha1.KDexApp{}
		if err := c.Get(context.Background(), key, got); err != nil {
			t.Fatalf("failed to get app after re-reconcile %d: %v", i, err)
		}
		if got.ResourceVersion != settledRV {
			t.Fatalf("re-reconcile %d rewrote a settled KDexApp: resourceVersion %s -> %s (self-loop / status churn)",
				i, settledRV, got.ResourceVersion)
		}
	}
}

func meta_IsReady(app *kdexv1alpha1.KDexApp) bool {
	for _, c := range app.Status.Conditions {
		if c.Type == string(kdexv1alpha1.ConditionTypeReady) {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}
