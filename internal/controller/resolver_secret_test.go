package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// TestResolveSecretPropagatesNonNotFoundError is a regression test for
// https://github.com/kdex-tech/nexus-manager/issues/6 - ResolveSecret used to
// swallow any non-NotFound error from client.Get and return an empty Secret
// with a nil error.
func TestResolveSecretPropagatesNonNotFoundError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}

	sentinel := errors.New("api server is having a bad day")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return sentinel
			},
		}).
		Build()

	referrer := &kdexv1alpha1.KDexApp{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "ns"},
	}
	var conditions []metav1.Condition
	secretRef := &corev1.LocalObjectReference{Name: "creds"}

	secret, shouldReturn, _, err := ResolveSecret(
		context.Background(), c, referrer, &conditions, secretRef, time.Second)

	if err == nil {
		t.Fatalf("expected a non-nil error to be propagated, got nil (secret=%v)", secret)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the underlying error to be propagated, got %v", err)
	}
	if !shouldReturn {
		t.Fatalf("expected shouldReturn=true so the caller requeues, got false")
	}
	if secret != nil {
		t.Fatalf("expected a nil secret on error, got %v", secret)
	}
}
