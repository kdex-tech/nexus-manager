package controller

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
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
	applied, overflow, err := selectExtensions(hostWithSelector(nil), []kdexv1alpha1.KDexHostExtension{ext("a", 0, eumLabel)})
	require.NoError(t, err)
	assert.Empty(t, applied)
	assert.Empty(t, overflow)
}

func TestSelectExtensions_FiltersAndOrders(t *testing.T) {
	deleting := ext("d", 0, eumLabel)
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	otherHost := ext("o", 0, eumLabel)
	otherHost.Spec.HostRef.Name = "other"
	applied, _, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{MatchLabels: eumLabel}), []kdexv1alpha1.KDexHostExtension{
		ext("zeta", 10, eumLabel), ext("beta", 10, eumLabel), ext("alpha", 20, eumLabel), ext("low", -5, eumLabel),
		ext("unlabelled", 0, nil), deleting, otherHost,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"low", "beta", "zeta", "alpha"}, names(applied), "weight ascending, then name; unlabelled/deleting/other-host excluded")
}

func TestSelectExtensions_EmptySelectorAcceptsAll(t *testing.T) {
	applied, _, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{}), []kdexv1alpha1.KDexHostExtension{ext("a", 0, nil)})
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, names(applied))
}

func TestSelectExtensions_Overflow(t *testing.T) {
	cands := []kdexv1alpha1.KDexHostExtension{}
	for i := range maxHostExtensions + 2 {
		cands = append(cands, ext(fmt.Sprintf("e%03d", i), 0, eumLabel))
	}
	applied, overflow, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{MatchLabels: eumLabel}), cands)
	require.NoError(t, err)
	assert.Len(t, applied, maxHostExtensions)
	assert.Equal(t, []string{"e032", "e033"}, names(overflow))
}

func TestSelectExtensions_InvalidSelector(t *testing.T) {
	_, _, err := selectExtensions(hostWithSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "k", Operator: "Bogus"}}}), nil)
	assert.Error(t, err)
}
