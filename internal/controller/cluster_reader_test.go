package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func discardLogger() logr.Logger { return logr.Discard() }

func readClusterReaderSubjects(t *testing.T, c client.Client) []rbacv1.Subject {
	t.Helper()
	b := &rbacv1.ClusterRoleBinding{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: clusterReaderBindingName}, b); err != nil {
		t.Fatal(err)
	}
	return b.Subjects
}

func TestClusterReaderSubjects(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	ctx := context.Background()

	for range 2 {
		if err := r.ensureClusterReaderSubject(ctx, "ns-a", "sa"); err != nil {
			t.Fatal(err)
		}
	}
	if got := readClusterReaderSubjects(t, fc); len(got) != 1 {
		t.Fatalf("subjects after ensure twice = %v, want one", got)
	}
	if err := r.ensureClusterReaderSubject(ctx, "ns-b", "sa"); err != nil {
		t.Fatal(err)
	}
	if got := readClusterReaderSubjects(t, fc); len(got) != 2 {
		t.Fatalf("subjects = %v, want two", got)
	}
	for range 2 {
		if err := r.removeClusterReaderSubject(ctx, "ns-a", "sa"); err != nil {
			t.Fatal(err)
		}
	}
	got := readClusterReaderSubjects(t, fc)
	if len(got) != 1 || got[0].Namespace != "ns-b" || got[0].Kind != "ServiceAccount" || got[0].APIGroup != "" {
		t.Fatalf("subjects = %v, want only ServiceAccount ns-b/sa", got)
	}
}

func TestClusterReaderSubjects_MissingBindingIsAnError(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	if err := fc.Delete(context.Background(), &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterReaderBindingName}}); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureClusterReaderSubject(context.Background(), "ns", "sa"); err == nil {
		t.Fatal("want an error when the chart-owned binding is missing")
	}
	// A missing binding holds no subject: removal has nothing to do, and a
	// teardown must not wedge on it.
	if err := r.removeClusterReaderSubject(context.Background(), "ns", "sa"); err != nil {
		t.Errorf("remove from a missing binding = %v, want nil", err)
	}
	err := fc.Get(context.Background(), client.ObjectKey{Name: clusterReaderBindingName}, &rbacv1.ClusterRoleBinding{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("binding was created: %v", err)
	}
}
