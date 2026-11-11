package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type countingClient struct {
	client.Client
	gets int
}

func (c *countingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.gets++
	return c.Client.Get(ctx, key, obj, opts...)
}

func crossNamespaceClient(t *testing.T, objs ...client.Object) *countingClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &countingClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()}
}

func conditionMessage(t *testing.T, conds []metav1.Condition) string {
	t.Helper()
	c := meta.FindStatusCondition(conds, string(kdexv1alpha1.ConditionTypeDegraded))
	if c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded condition not true: %v", conds)
	}
	return c.Message
}

func TestResolveKDexObjectReference_OtherNamespaceRefusedBeforeGet(t *testing.T) {
	existing := &kdexv1alpha1.KDexTheme{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "b"}}
	referrer := &kdexv1alpha1.KDexHost{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "a"}}
	ref := &kdexv1alpha1.KDexObjectReference{Kind: "KDexTheme", Name: "t", Namespace: "b"}

	var messages []string
	for _, c := range []*countingClient{crossNamespaceClient(t, existing), crossNamespaceClient(t)} {
		var conds []metav1.Condition
		obj, shouldReturn, _, err := ResolveKDexObjectReference(context.Background(), c, referrer, &conds, ref, time.Second)
		if !errors.Is(err, errCrossNamespaceReference) || !shouldReturn || obj != nil {
			t.Fatalf("got obj=%v shouldReturn=%v err=%v", obj, shouldReturn, err)
		}
		if c.gets != 0 {
			t.Errorf("Get called %d times, want 0", c.gets)
		}
		messages = append(messages, conditionMessage(t, conds))
	}
	if messages[0] != messages[1] || messages[0] != "references to other namespaces are disabled" {
		t.Errorf("messages differ or unexpected: %q", messages)
	}
}

func TestResolveKDexObjectReference_SameNamespaceStillResolves(t *testing.T) {
	c := crossNamespaceClient(t)
	referrer := &kdexv1alpha1.KDexHost{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "a"}}
	ref := &kdexv1alpha1.KDexObjectReference{Kind: "KDexTheme", Name: "t", Namespace: "a"}
	var conds []metav1.Condition
	_, _, _, err := ResolveKDexObjectReference(context.Background(), c, referrer, &conds, ref, time.Second)
	if errors.Is(err, errCrossNamespaceReference) || c.gets != 1 {
		t.Fatalf("same-namespace ref refused or not fetched: gets=%d err=%v", c.gets, err)
	}
}

func TestResolveSecret_OtherNamespaceRefused(t *testing.T) {
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "b"}}
	referrer := &kdexv1alpha1.KDexApp{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "a"}}
	ref := &kdexv1alpha1.KDexObjectReference{Kind: "Secret", Name: "s", Namespace: "b"}

	var messages []string
	for _, c := range []*countingClient{crossNamespaceClient(t, existing), crossNamespaceClient(t)} {
		var conds []metav1.Condition
		secret, shouldReturn, _, err := ResolveSecret(context.Background(), c, referrer, &conds, ref, time.Second)
		if !errors.Is(err, errCrossNamespaceReference) || !shouldReturn || secret != nil {
			t.Fatalf("got secret=%v shouldReturn=%v err=%v", secret, shouldReturn, err)
		}
		if c.gets != 0 {
			t.Errorf("Get called %d times, want 0", c.gets)
		}
		messages = append(messages, conditionMessage(t, conds))
	}
	if messages[0] != messages[1] {
		t.Errorf("messages differ: %q", messages)
	}
}

func TestResolveOperatorSecret_AllowsConfiguredNamespace(t *testing.T) {
	existing := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "npm", Namespace: "kcnas-system"}}
	c := crossNamespaceClient(t, existing)
	referrer := &kdexv1alpha1.KDexApp{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "a"}}
	ref := &kdexv1alpha1.KDexObjectReference{Kind: "Secret", Name: "npm", Namespace: "kcnas-system"}

	var conds []metav1.Condition
	secret, shouldReturn, _, err := ResolveOperatorSecret(context.Background(), c, referrer, &conds, ref, time.Second)
	if err != nil || shouldReturn || secret == nil || secret.Name != "npm" {
		t.Fatalf("got secret=%v shouldReturn=%v err=%v", secret, shouldReturn, err)
	}
}

func TestHostManagerValues_ReferenceNamespacesDisabled(t *testing.T) {
	r := hashReconciler(t)
	host := baseHost()
	host.Spec.ReferenceNamespaces = []string{"b"}

	spec, err := r.buildHostManagerChartSpec(host)
	if err != nil {
		t.Fatal(err)
	}
	if got := spec.Values["referenceNamespaces"]; !equalAny(got, []any{}) {
		t.Errorf("referenceNamespaces = %v, want empty", got)
	}

	setReferenceNamespacesCondition(host)
	if !meta.IsStatusConditionTrue(host.Status.Conditions, ConditionReferenceNamespacesDisabled) {
		t.Errorf("condition not set: %v", host.Status.Conditions)
	}

	host.Spec.ReferenceNamespaces = nil
	setReferenceNamespacesCondition(host)
	if meta.FindStatusCondition(host.Status.Conditions, ConditionReferenceNamespacesDisabled) != nil {
		t.Error("condition not cleared when the list is empty")
	}
}
