package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// An event on the cluster-reader binding wakes exactly the hosts whose
// recorded ServiceAccount it no longer holds: an out-of-band reset heals at
// once, and a grant -- which writes the binding -- wakes no one.
func TestClusterReaderBindingRequests(t *testing.T) {
	granted, dropped, never := hostWithRecordedSA("granted", ""), hostWithRecordedSA("dropped", ""), hostWithRecordedSA("", "")
	granted.Name, dropped.Name, never.Name = "granted", "dropped", "never"
	r, _ := newHelmReconciler(t, NewMockHelmClient(nil), nil, granted, dropped, never)
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: clusterReaderBindingName},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "granted", Namespace: "ns"}},
	}

	got := r.clusterReaderBindingRequests(context.Background(), binding)
	if len(got) != 1 || got[0].Name != "dropped" || got[0].Namespace != "ns" {
		t.Errorf("requests = %v, want only ns/dropped", got)
	}
	other := binding.DeepCopy()
	other.Name = "something-else"
	other.Subjects = nil
	if got := r.clusterReaderBindingRequests(context.Background(), other); len(got) != 0 {
		t.Errorf("requests for another binding = %v, want none", got)
	}
}

// A settled heal reads the binding from the cache and makes no live call; a
// missing subject is re-added through the live optimistic-lock write.
func TestHealClusterReaderSubject_ReadsTheCacheFirst(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	var liveReads int
	r.APIReader = interceptor.NewClient(fc.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*rbacv1.ClusterRoleBinding); ok {
				liveReads++
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	host := hostWithRecordedSA("my-host", HelmStatusCompleted)
	host.Namespace = "ns"

	if err := r.healClusterReaderSubject(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	if liveReads != 1 {
		t.Fatalf("missing subject: %d live reads, want 1", liveReads)
	}
	if subs := readClusterReaderSubjects(t, fc); len(subs) != 1 {
		t.Fatalf("subjects = %v, want ns/my-host", subs)
	}
	for range 3 {
		if err := r.healClusterReaderSubject(t.Context(), host); err != nil {
			t.Fatal(err)
		}
	}
	if liveReads != 1 {
		t.Errorf("settled heals made %d live reads in all, want none beyond the first write", liveReads)
	}
}

// One subject serves every host in the namespace that runs as its
// ServiceAccount: tearing down one of them, or renaming one's ServiceAccount,
// keeps the subject while another live host has it recorded.
func TestClusterReaderSubject_KeptWhileAnotherHostSharesIt(t *testing.T) {
	t.Run("teardown", func(t *testing.T) {
		r, fc, _ := newTeardownFixture(t)
		sibling := hostWithRecordedSA("my-host", "")
		sibling.Name, sibling.Finalizers = "sibling", nil
		createObjects(t, fc, sibling)
		deleteHost(t, fc, "my-host")

		if err := reconcileTeardown(t, r); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		assertFinalized(t, fc)
		if subs := readClusterReaderSubjects(t, fc); len(subs) != 1 || subs[0].Name != "my-host" {
			t.Errorf("subjects = %v, want ns/my-host kept for the sibling", subs)
		}
	})
	t.Run("terminating sibling", func(t *testing.T) {
		r, fc, _ := newTeardownFixture(t)
		sibling := hostWithRecordedSA("my-host", "")
		sibling.Name, sibling.Finalizers = "sibling", []string{"test/hold"}
		createObjects(t, fc, sibling)
		deleteHost(t, fc, "sibling")
		deleteHost(t, fc, "my-host")

		if err := reconcileTeardown(t, r); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if subs := readClusterReaderSubjects(t, fc); len(subs) != 0 {
			t.Errorf("subjects = %v, want the subject removed: the sibling is going too", subs)
		}
	})
	t.Run("rename", func(t *testing.T) {
		host := helmHost("my-host")
		host.Status.Attributes = map[string]string{AttributeHostManagerServiceAccount: "shared"}
		host.Spec.Helm = &kdexv1alpha1.HelmConfig{HostManager: &kdexv1alpha1.HostManagerHelmConfig{
			Values: `{"serviceAccount":{"name":"new"}}`,
		}}
		sibling := hostWithRecordedSA("shared", "")
		sibling.Name, sibling.Finalizers = "sibling", nil
		r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host, sibling)
		if err := r.ensureClusterReaderSubject(t.Context(), "ns", "shared"); err != nil {
			t.Fatal(err)
		}

		r.runAsyncHelmReconcile(t.Context(), "ns", "my-host", "hash", nil, discardLogger())
		subs := readClusterReaderSubjects(t, fc)
		names := []string{}
		for _, s := range subs {
			names = append(names, s.Name)
		}
		slices.Sort(names)
		if !slices.Equal(names, []string{"new", "shared"}) {
			t.Errorf("subjects = %v, want new and the sibling's shared", names)
		}
	})
}
