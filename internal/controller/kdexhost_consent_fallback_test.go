package controller

import (
	"context"
	"testing"

	"github.com/kdex-tech/nexus-manager/internal/webhook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestUtilityPageRef_NilConsentRefFallsBackToTheDefault(t *testing.T) {
	for name, host := range map[string]*kdexv1alpha1.KDexHost{
		"no utilityPages": {},
		"no consentRef":   {Spec: kdexv1alpha1.KDexHostSpec{UtilityPages: &kdexv1alpha1.UtilityPages{}}},
	} {
		t.Run(name, func(t *testing.T) {
			ref := utilityPageRef(host, kdexv1alpha1.ConsentUtilityPageType)
			require.NotNil(t, ref)
			assert.Equal(t, "KDexClusterUtilityPage", ref.Kind)
			assert.Equal(t, webhook.KDexDefaultUtilityPageConsent, ref.Name)

			assert.Nil(t, utilityPageRef(host, kdexv1alpha1.LoginUtilityPageType), "only Consent falls back")
		})
	}

	explicit := &kdexv1alpha1.KDexObjectReference{Kind: "KDexUtilityPage", Name: "mine"}
	host := &kdexv1alpha1.KDexHost{Spec: kdexv1alpha1.KDexHostSpec{UtilityPages: &kdexv1alpha1.UtilityPages{ConsentRef: explicit}}}
	assert.Same(t, explicit, utilityPageRef(host, kdexv1alpha1.ConsentUtilityPageType))
}

func TestDefaultConsentPageRequests_EnqueuesOnlyHostsWithoutAConsentRef(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, kdexv1alpha1.AddToScheme(scheme))
	host := func(ns, name string, pages *kdexv1alpha1.UtilityPages) *kdexv1alpha1.KDexHost {
		return &kdexv1alpha1.KDexHost{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: kdexv1alpha1.KDexHostSpec{UtilityPages: pages}}
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		host("a", "legacy", nil),
		host("b", "no-consent", &kdexv1alpha1.UtilityPages{}),
		host("a", "explicit", &kdexv1alpha1.UtilityPages{ConsentRef: &kdexv1alpha1.KDexObjectReference{Kind: "KDexUtilityPage", Name: "x"}}),
	).Build()
	r := &KDexHostReconciler{Client: fc, Scheme: scheme}

	defaultPage := &kdexv1alpha1.KDexClusterUtilityPage{ObjectMeta: metav1.ObjectMeta{Name: webhook.KDexDefaultUtilityPageConsent}}
	got := r.defaultConsentPageRequests(context.Background(), defaultPage)
	assert.ElementsMatch(t, []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: "a", Name: "legacy"}},
		{NamespacedName: types.NamespacedName{Namespace: "b", Name: "no-consent"}},
	}, got)

	otherPage := &kdexv1alpha1.KDexClusterUtilityPage{ObjectMeta: metav1.ObjectMeta{Name: webhook.KDexDefaultUtilityPageLogin}}
	assert.Empty(t, r.defaultConsentPageRequests(context.Background(), otherPage))
}
