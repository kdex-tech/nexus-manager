package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kdex-tech/dmapper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func ext(name string, weight int32, lbl map[string]string) kdexv1alpha1.KDexHostExtension {
	return kdexv1alpha1.KDexHostExtension{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: lbl},
		Spec:       kdexv1alpha1.KDexHostExtensionSpec{HostRef: corev1.LocalObjectReference{Name: "h"}, Weight: weight},
	}
}

func hostWithSelector(sel *metav1.LabelSelector) *kdexv1alpha1.KDexHost {
	return &kdexv1alpha1.KDexHost{ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "ns"}, Spec: kdexv1alpha1.KDexHostSpec{ExtensionSelector: sel}}
}

var eumLabel = map[string]string{"kdex.dev/extension": "eum"}

func names(es []kdexv1alpha1.KDexHostExtension) []string {
	out := []string{}
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func TestSelectExtensions_NilSelectorAcceptsNone(t *testing.T) {
	applied, overflow, invalid, err := selectExtensions(hostWithSelector(nil), []kdexv1alpha1.KDexHostExtension{ext("a", 0, eumLabel)})
	require.NoError(t, err)
	assert.Empty(t, applied)
	assert.Empty(t, overflow)
	assert.Empty(t, invalid)
}

func TestSelectExtensions_FiltersAndOrders(t *testing.T) {
	deleting := ext("d", 0, eumLabel)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	otherHost := ext("o", 0, eumLabel)
	otherHost.Spec.HostRef.Name = "other"
	applied, _, _, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{MatchLabels: eumLabel}), []kdexv1alpha1.KDexHostExtension{
		ext("zeta", 10, eumLabel), ext("beta", 10, eumLabel), ext("alpha", 20, eumLabel), ext("low", -5, eumLabel),
		ext("unlabelled", 0, nil), deleting, otherHost,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"low", "beta", "zeta", "alpha"}, names(applied), "weight ascending, then name; unlabelled/deleting/other-host excluded")
}

func TestSelectExtensions_EmptySelectorAcceptsAll(t *testing.T) {
	applied, _, _, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{}), []kdexv1alpha1.KDexHostExtension{ext("a", 0, nil)})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, names(applied))
}

func TestSelectExtensions_Overflow(t *testing.T) {
	cands := []kdexv1alpha1.KDexHostExtension{}
	for i := range maxHostExtensions + 2 {
		cands = append(cands, ext(fmt.Sprintf("e%03d", i), 0, eumLabel))
	}
	applied, overflow, _, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{MatchLabels: eumLabel}), cands)
	require.NoError(t, err)
	assert.Len(t, applied, maxHostExtensions)
	assert.Equal(t, []string{"e032", "e033"}, names(overflow))
}

func withRule(e kdexv1alpha1.KDexHostExtension, expr string) kdexv1alpha1.KDexHostExtension {
	e.Spec.ClaimMappings = []dmapper.MappingRule{{SourceExpression: expr, TargetPropPath: "entitlements"}}
	return e
}

// An extension whose claimMappings do not compile would make host-manager's
// mapper build fail and freeze the host's previous auth config. It is excluded
// before the cap, so it never takes a slot from a valid extension.
func TestSelectExtensions_InvalidClaimMappingsExcludedBeforeCap(t *testing.T) {
	bad := withRule(ext("aaa-bad", -1000, eumLabel), "self.(")
	cands := []kdexv1alpha1.KDexHostExtension{bad}
	for i := range maxHostExtensions {
		cands = append(cands, withRule(ext(fmt.Sprintf("e%03d", i), 0, eumLabel), "self.x"))
	}
	applied, overflow, invalid, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{MatchLabels: eumLabel}), cands)
	require.NoError(t, err)
	assert.Len(t, applied, maxHostExtensions)
	assert.NotContains(t, names(applied), "aaa-bad")
	assert.Empty(t, overflow, "the invalid extension must not consume a slot")
	require.Len(t, invalid, 1)
	assert.Equal(t, "aaa-bad", invalid[0].Name)
	assert.Error(t, invalid[0].err)
}

// Only selected extensions are compiled: an unselected one is NotSelected,
// whatever its CEL.
func TestSelectExtensions_UnselectedInvalidIsNotReportedInvalid(t *testing.T) {
	_, _, invalid, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{MatchLabels: eumLabel}),
		[]kdexv1alpha1.KDexHostExtension{withRule(ext("bad", 0, nil), "self.(")})
	require.NoError(t, err)
	assert.Empty(t, invalid)
}

func TestResolveExtensions_ExcludesInvalidClaimMappings(t *testing.T) {
	good := withRule(ext("good", 0, eumLabel), "self.x")
	good.Generation = 2
	bad := withRule(ext("bad", 0, eumLabel), "self.(")
	r := newExtensionReconciler(t, interceptor.Funcs{}, &good, &bad)

	host := hostWithSelector(&metav1.LabelSelector{MatchLabels: eumLabel})
	got, err := r.resolveExtensions(context.Background(), host)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "good", got[0].Name)
	assert.Equal(t, map[string]string{"good" + extensionGenerationSuffix: "2"}, host.Status.Attributes)
}

func TestSelectExtensions_InvalidSelector(t *testing.T) {
	_, _, _, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Bogus"}}}), nil)
	assert.Error(t, err)
}

func newExtensionReconciler(t *testing.T, fns interceptor.Funcs, objs ...client.Object) *KDexHostReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, kdexv1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&kdexv1alpha1.KDexHostExtension{}, hostIndexKey, indexExtensionByHostRef).
		WithInterceptorFuncs(fns).
		Build()
	return &KDexHostReconciler{Client: fc, Scheme: scheme}
}

func TestResolveExtensions_InvalidSelectorFailsClosed(t *testing.T) {
	e := ext("a", 0, eumLabel)
	r := newExtensionReconciler(t, interceptor.Funcs{}, &e)
	host := hostWithSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Bogus"}}})
	host.Status.Attributes = map[string]string{"x" + extensionGenerationSuffix: "3", "other": "keep"}

	got, err := r.resolveExtensions(context.Background(), host)
	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorIs(t, err, errInvalidExtensionSelector)
	assert.Equal(t, map[string]string{"other": "keep"}, host.Status.Attributes)
}

func TestResolveExtensions_ListErrorIsNotSelectorError(t *testing.T) {
	boom := errors.New("boom")
	r := newExtensionReconciler(t, interceptor.Funcs{
		List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return boom },
	})
	_, err := r.resolveExtensions(context.Background(), hostWithSelector(&metav1.LabelSelector{}))
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, errInvalidExtensionSelector)
}

func TestExtensionsForSibling_EnqueuesAllExtensionsOfSameHost(t *testing.T) {
	a, b, c := ext("a", 0, nil), ext("b", 1, nil), ext("c", 2, nil)
	other := ext("o", 0, nil)
	other.Spec.HostRef.Name = "other"
	elsewhere := ext("e", 0, nil)
	elsewhere.Namespace = "ns2"

	scheme := runtime.NewScheme()
	require.NoError(t, kdexv1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&a, &b, &c, &other, &elsewhere).
		WithIndex(&kdexv1alpha1.KDexHostExtension{}, hostIndexKey, indexExtensionByHostRef).
		Build()
	r := &KDexHostExtensionReconciler{Client: fc, Scheme: scheme}

	got := r.extensionsForSibling(context.Background(), &b)
	var names []string
	for _, req := range got {
		assert.Equal(t, "ns", req.Namespace)
		names = append(names, req.Name)
	}
	assert.ElementsMatch(t, []string{"a", "b", "c"}, names)
}
