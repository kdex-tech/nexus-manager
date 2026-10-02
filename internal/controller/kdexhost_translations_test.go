package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func trSource(kind, ns, name string) translationSource {
	return translationSource{Kind: kind, Namespace: ns, Name: name}
}

func sourceNames(ss []translationSource) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func TestOrderTranslationSources_PrecedenceLowestToHighest(t *testing.T) {
	def := trSource("KDexClusterTranslation", "", "kdex-default-translation")
	self := []translationSource{trSource("KDexTranslation", "site", "zeta"), trSource("KDexTranslation", "site", "alpha")}
	declared := []translationSource{trSource("KDexTranslation", "site", "brand"), trSource("KDexClusterTranslation", "", "shared")}

	ordered, collision := orderTranslationSources(&def, self, declared)

	assert.Equal(t, []string{"kdex-default-translation", "alpha", "zeta", "brand", "shared"}, sourceNames(ordered))
	assert.Empty(t, collision)
}

func TestOrderTranslationSources_DeclaredPositionWinsOverSelfAttached(t *testing.T) {
	self := []translationSource{trSource("KDexTranslation", "site", "brand"), trSource("KDexTranslation", "site", "extra")}
	declared := []translationSource{trSource("KDexTranslation", "site", "brand")}

	ordered, collision := orderTranslationSources(nil, self, declared)

	assert.Equal(t, []string{"extra", "brand"}, sourceNames(ordered), "brand keeps only its host-declared (higher) position")
	assert.Empty(t, collision, "the same object twice is a duplicate, not a collision")
}

func TestOrderTranslationSources_NameCollisionKeepsHigherPrecedence(t *testing.T) {
	def := trSource("KDexClusterTranslation", "", "kdex-default-translation")
	self := []translationSource{trSource("KDexTranslation", "site", "kdex-default-translation")}

	ordered, collision := orderTranslationSources(&def, self, nil)

	assert.Len(t, ordered, 1)
	assert.Equal(t, "KDexTranslation", ordered[0].Kind, "the self-attached source outranks the default")
	assert.Contains(t, collision, "KDexClusterTranslation//kdex-default-translation")
	assert.Contains(t, collision, "KDexTranslation/site/kdex-default-translation")
}

func TestOrderTranslationSources_NilDefaultAndEmpty(t *testing.T) {
	ordered, collision := orderTranslationSources(nil, nil, nil)
	assert.Empty(t, ordered)
	assert.Empty(t, collision)
}

const trNS = "site"

func readyStatus() kdexv1alpha1.KDexObjectStatus {
	st := kdexv1alpha1.KDexObjectStatus{}
	kdexv1alpha1.SetConditions(&st.Conditions, kdexv1alpha1.ConditionStatuses{
		Degraded: metav1.ConditionFalse, Progressing: metav1.ConditionFalse, Ready: metav1.ConditionTrue,
	}, kdexv1alpha1.ConditionReasonReconcileSuccess, "ready")
	return st
}

func trSpec(key, value string) kdexv1alpha1.KDexTranslationSpec {
	return kdexv1alpha1.KDexTranslationSpec{Translations: []kdexv1alpha1.Translation{
		{Lang: "en", KeysAndValues: map[string]string{key: value}},
	}}
}

func nsTranslation(name, hostRef string, ready bool) *kdexv1alpha1.KDexTranslation {
	tr := &kdexv1alpha1.KDexTranslation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: trNS, Generation: 1},
		Spec:       kdexv1alpha1.KDexNamespacedTranslationSpec{KDexTranslationSpec: trSpec("k", name)},
	}
	if hostRef != "" {
		tr.Spec.HostRef = &corev1.LocalObjectReference{Name: hostRef}
	}
	if ready {
		tr.Status = readyStatus()
	}
	return tr
}

func defaultClusterTranslation() *kdexv1alpha1.KDexClusterTranslation {
	return &kdexv1alpha1.KDexClusterTranslation{
		ObjectMeta: metav1.ObjectMeta{Name: "kdex-default-translation", Generation: 1},
		Spec:       trSpec("k", "default"),
		Status:     readyStatus(),
	}
}

func trHost(refs ...kdexv1alpha1.KDexObjectReference) *kdexv1alpha1.KDexHost {
	return &kdexv1alpha1.KDexHost{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: trNS, UID: types.UID("host-uid")},
		Spec:       kdexv1alpha1.KDexHostSpec{TranslationRefs: refs},
	}
}

func newTranslationReconciler(t *testing.T, objs ...client.Object) (*KDexHostReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, kdexv1alpha1.AddToScheme(scheme))
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&kdexv1alpha1.KDexInternalTranslation{}).
		WithIndex(&kdexv1alpha1.KDexTranslation{}, hostIndexKey, indexTranslationByHostRef).
		WithIndex(&kdexv1alpha1.KDexInternalTranslation{}, hostIndexKey, indexInternalTranslationByHost).
		Build()
	return &KDexHostReconciler{Client: fc, Scheme: scheme, RequeueDelay: time.Second}, fc
}

func refNames(refs []corev1.LocalObjectReference) []string {
	out := []string{}
	for _, r := range refs {
		out = append(out, r.Name)
	}
	return out
}

func TestResolveTranslations_UnionsSelfAttachedInPrecedenceOrder(t *testing.T) {
	host := trHost(kdexv1alpha1.KDexObjectReference{Kind: "KDexTranslation", Name: "declared"})
	r, fc := newTranslationReconciler(t, host, defaultClusterTranslation(),
		nsTranslation("declared", "", true),
		nsTranslation("b-attached", "web", true),
		nsTranslation("a-attached", "web", true),
		nsTranslation("other-host", "elsewhere", true),
	)

	refs, collision, shouldReturn, _, err := r.resolveTranslations(context.Background(), host)

	require.NoError(t, err)
	require.False(t, shouldReturn)
	assert.Empty(t, collision)
	assert.Equal(t, []string{"web-kdex-default-translation", "web-a-attached", "web-b-attached", "web-declared"}, refNames(refs))

	it := &kdexv1alpha1.KDexInternalTranslation{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: trNS, Name: "web-a-attached"}, it))
	assert.Equal(t, "web", it.Spec.HostRef.Name)
	assert.Equal(t, "a-attached", it.Spec.Translations[0].KeysAndValues["k"])
}

func TestResolveTranslations_NotReadySelfAttachedRequeues(t *testing.T) {
	host := trHost()
	r, fc := newTranslationReconciler(t, host, defaultClusterTranslation(), nsTranslation("fresh", "web", false))

	_, _, shouldReturn, res, err := r.resolveTranslations(context.Background(), host)

	require.NoError(t, err)
	assert.True(t, shouldReturn)
	assert.Equal(t, time.Second, res.RequeueAfter)
	list := &kdexv1alpha1.KDexInternalTranslationList{}
	require.NoError(t, fc.List(context.Background(), list))
	assert.Empty(t, list.Items, "nothing is published until every attached translation is Ready")
}

func TestResolveTranslations_PrunesOnlyControlledStaleCopies(t *testing.T) {
	host := trHost()
	controller := true
	stale := &kdexv1alpha1.KDexInternalTranslation{
		ObjectMeta: metav1.ObjectMeta{Name: "web-gone", Namespace: trNS, OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "kdex.dev/v1alpha1", Kind: "KDexHost", Name: "web", UID: "host-uid", Controller: &controller,
		}}},
		Spec: kdexv1alpha1.KDexInternalTranslationSpec{KDexTranslationSpec: trSpec("k", "gone"), HostRef: corev1.LocalObjectReference{Name: "web"}},
	}
	foreign := &kdexv1alpha1.KDexInternalTranslation{
		ObjectMeta: metav1.ObjectMeta{Name: "web-handmade", Namespace: trNS},
		Spec:       kdexv1alpha1.KDexInternalTranslationSpec{KDexTranslationSpec: trSpec("k", "hand"), HostRef: corev1.LocalObjectReference{Name: "web"}},
	}
	host.Status.Attributes = map[string]string{"gone.translation.generation": "3", "ingress": "x"}
	r, fc := newTranslationReconciler(t, host, defaultClusterTranslation(), stale, foreign)

	_, _, shouldReturn, _, err := r.resolveTranslations(context.Background(), host)
	require.NoError(t, err)
	require.False(t, shouldReturn)

	err = fc.Get(context.Background(), types.NamespacedName{Namespace: trNS, Name: "web-gone"}, &kdexv1alpha1.KDexInternalTranslation{})
	assert.True(t, apierrors.IsNotFound(err), "a controlled copy no longer desired is pruned")
	assert.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: trNS, Name: "web-handmade"}, &kdexv1alpha1.KDexInternalTranslation{}),
		"an internal translation this host does not control is never pruned")

	assert.NotContains(t, host.Status.Attributes, "gone.translation.generation")
	assert.Equal(t, "x", host.Status.Attributes["ingress"], "unrelated attributes are untouched")
	assert.Equal(t, "1", host.Status.Attributes["kdex-default-translation.translation.generation"])
}

func TestResolveTranslations_NameCollisionIsReported(t *testing.T) {
	host := trHost()
	r, fc := newTranslationReconciler(t, host, defaultClusterTranslation(), nsTranslation("kdex-default-translation", "web", true))

	refs, collision, shouldReturn, _, err := r.resolveTranslations(context.Background(), host)

	require.NoError(t, err)
	require.False(t, shouldReturn)
	assert.Equal(t, []string{"web-kdex-default-translation"}, refNames(refs))
	assert.Contains(t, collision, "KDexTranslation/site/kdex-default-translation")
	assert.Contains(t, collision, "KDexClusterTranslation//kdex-default-translation")

	it := &kdexv1alpha1.KDexInternalTranslation{}
	require.NoError(t, fc.Get(context.Background(), types.NamespacedName{Namespace: trNS, Name: "web-kdex-default-translation"}, it))
	assert.Equal(t, "kdex-default-translation", it.Spec.Translations[0].KeysAndValues["k"],
		"the higher-precedence self-attached source wins the shared internal name")
}

func TestResolveTranslations_SkipsDeletingSelfAttached(t *testing.T) {
	host := trHost()
	dying := nsTranslation("dying", "web", true)
	now := metav1.Now()
	dying.DeletionTimestamp = &now
	dying.Finalizers = []string{"test.kdex.dev/hold"}
	r, fc := newTranslationReconciler(t, host, defaultClusterTranslation(), dying)

	refs, collision, shouldReturn, _, err := r.resolveTranslations(context.Background(), host)

	require.NoError(t, err)
	require.False(t, shouldReturn)
	assert.Empty(t, collision)
	assert.Equal(t, []string{"web-kdex-default-translation"}, refNames(refs))
	err = fc.Get(context.Background(), types.NamespacedName{Namespace: trNS, Name: "web-dying"}, &kdexv1alpha1.KDexInternalTranslation{})
	assert.True(t, apierrors.IsNotFound(err), "a translation being deleted is not copied")
}

func TestSetTranslationCollisionCondition(t *testing.T) {
	host := trHost()

	setTranslationCollisionCondition(host, "A and B")

	degraded := meta.FindStatusCondition(host.Status.Conditions, string(kdexv1alpha1.ConditionTypeDegraded))
	ready := meta.FindStatusCondition(host.Status.Conditions, string(kdexv1alpha1.ConditionTypeReady))
	require.NotNil(t, degraded)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, degraded.Status)
	assert.Equal(t, metav1.ConditionFalse, ready.Status)
	assert.Contains(t, degraded.Message, "translation name collision: A and B")
}
