package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestResolveContentsNoPanicOnNonAppEntry is a regression test for
// https://github.com/kdex-tech/nexus-manager/issues/7 - ResolveContents used
// to dereference a nil *KDexAppSpec (panic) when a content entry had neither
// rawHTML nor a reference that resolves to a KDexApp/KDexClusterApp.
func TestResolveContentsNoPanicOnNonAppEntry(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	referrer := &kdexv1alpha1.KDexUtilityPage{
		ObjectMeta: metav1.ObjectMeta{Name: "up", Namespace: "ns"},
	}
	var conditions []metav1.Condition

	// A content entry with neither RawHTML nor an AppRef.
	entries := []kdexv1alpha1.ContentEntry{{Slot: "main"}}

	contents, shouldReturn, _, err := ResolveContents(
		context.Background(), c, referrer, &conditions, entries, time.Second)

	if err == nil {
		t.Fatalf("expected an error for a content entry with no rawHTML and no appRef, got nil (contents=%v)", contents)
	}
	if !shouldReturn {
		t.Fatalf("expected shouldReturn=true so the caller stops, got false")
	}
}
