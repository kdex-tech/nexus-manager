/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// This file is a regression test suite for
// https://github.com/kdex-tech/nexus-manager/issues/31 across the 8
// controllers that shared kdexapp's pre-fix shape: a transient "Reconciling"
// condition pulsed unconditionally at the top of every reconcile, followed by
// an unconditional deferred Status().Update(). The pulse-then-settle bumped
// each condition's LastTransitionTime on every pass, so the status write was
// never a no-op: resourceVersion churned and the controller's own For()
// watch (no GenerationChangedPredicate) re-fired, sustaining a reconcile
// storm that also re-enqueued every downstream host-manager page.
//
// Each test below settles a resource to Ready=True on the first reconcile
// (exactly one status write), then reconciles it 3 more times with nothing
// changed and asserts no further Status().Update() call occurs.

// newStatusWriteCounter builds a fake client wrapped with an interceptor that
// counts calls to the status subresource Update, alongside the scheme used
// to build it.
func newStatusWriteCounter(t *testing.T, objs ...client.Object) (client.Client, *int) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add kdex scheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(
			&kdexv1alpha1.KDexTheme{},
			&kdexv1alpha1.KDexClusterTheme{},
			&kdexv1alpha1.KDexPageFooter{},
			&kdexv1alpha1.KDexClusterPageFooter{},
			&kdexv1alpha1.KDexPageHeader{},
			&kdexv1alpha1.KDexClusterPageHeader{},
			&kdexv1alpha1.KDexPageNavigation{},
			&kdexv1alpha1.KDexClusterPageNavigation{},
			&kdexv1alpha1.KDexScriptLibrary{},
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			&kdexv1alpha1.KDexTranslation{},
			&kdexv1alpha1.KDexClusterTranslation{},
			&kdexv1alpha1.KDexPageArchetype{},
			&kdexv1alpha1.KDexClusterPageArchetype{},
			&kdexv1alpha1.KDexUtilityPage{},
			&kdexv1alpha1.KDexClusterUtilityPage{},
		)

	writes := 0

	c := interceptor.NewClient(builder.Build(), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" {
				writes++
			}
			return cli.Status().Update(ctx, obj, opts...)
		},
	})

	return c, &writes
}

// assertNoStormAfterSettling reconciles once (expecting exactly one status
// write and a Ready=True result) then reconciles 3 more times, asserting no
// additional status write occurs.
func assertNoStormAfterSettling(
	t *testing.T,
	writes *int,
	reconcile func() (ctrl.Result, error),
	getConditions func() []metav1.Condition,
) {
	t.Helper()

	if _, err := reconcile(); err != nil {
		t.Fatalf("first reconcile returned unexpected error: %v", err)
	}

	if got := *writes; got != 1 {
		t.Fatalf("expected exactly 1 status write after first reconcile, got %d", got)
	}

	if !isReadyTrue(getConditions()) {
		t.Fatalf("expected Ready=True after first reconcile, conditions: %+v", getConditions())
	}

	for i := 0; i < 3; i++ {
		if _, err := reconcile(); err != nil {
			t.Fatalf("re-reconcile %d returned unexpected error: %v", i, err)
		}

		if got := *writes; got != 1 {
			t.Fatalf("re-reconcile %d issued an extra status write (self-loop / status churn): total writes = %d", i, got)
		}
	}
}

func isReadyTrue(conditions []metav1.Condition) bool {
	for _, c := range conditions {
		if c.Type == string(kdexv1alpha1.ConditionTypeReady) {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func TestKDexThemeReconcileIsStableWhenSettled(t *testing.T) {
	theme := &kdexv1alpha1.KDexTheme{
		ObjectMeta: metav1.ObjectMeta{Name: "theme", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexThemeSpec{
			Assets: kdexv1alpha1.Assets{
				{
					Attributes: map[string]string{"rel": "stylesheet"},
					LinkHref:   "http://kdex.dev/style.css",
				},
			},
		},
	}

	c, writes := newStatusWriteCounter(t, theme)
	r := &KDexThemeReconciler{Client: c}
	key := types.NamespacedName{Name: "theme", Namespace: "ns"}

	assertNoStormAfterSettling(t, writes,
		func() (ctrl.Result, error) {
			return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		},
		func() []metav1.Condition {
			got := &kdexv1alpha1.KDexTheme{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatalf("failed to get theme: %v", err)
			}
			return got.Status.Conditions
		},
	)
}

func TestKDexPageFooterReconcileIsStableWhenSettled(t *testing.T) {
	footer := &kdexv1alpha1.KDexPageFooter{
		ObjectMeta: metav1.ObjectMeta{Name: "footer", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexPageFooterSpec{
			Content: "<footer>[[ .Footer ]]</footer>",
		},
	}

	c, writes := newStatusWriteCounter(t, footer)
	r := &KDexPageFooterReconciler{Client: c}
	key := types.NamespacedName{Name: "footer", Namespace: "ns"}

	assertNoStormAfterSettling(t, writes,
		func() (ctrl.Result, error) {
			return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		},
		func() []metav1.Condition {
			got := &kdexv1alpha1.KDexPageFooter{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatalf("failed to get footer: %v", err)
			}
			return got.Status.Conditions
		},
	)
}

func TestKDexPageHeaderReconcileIsStableWhenSettled(t *testing.T) {
	header := &kdexv1alpha1.KDexPageHeader{
		ObjectMeta: metav1.ObjectMeta{Name: "header", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexPageHeaderSpec{
			Content: "<header>[[ .Header ]]</header>",
		},
	}

	c, writes := newStatusWriteCounter(t, header)
	r := &KDexPageHeaderReconciler{Client: c}
	key := types.NamespacedName{Name: "header", Namespace: "ns"}

	assertNoStormAfterSettling(t, writes,
		func() (ctrl.Result, error) {
			return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		},
		func() []metav1.Condition {
			got := &kdexv1alpha1.KDexPageHeader{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatalf("failed to get header: %v", err)
			}
			return got.Status.Conditions
		},
	)
}

func TestKDexPageNavigationReconcileIsStableWhenSettled(t *testing.T) {
	navigation := &kdexv1alpha1.KDexPageNavigation{
		ObjectMeta: metav1.ObjectMeta{Name: "nav", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexPageNavigationSpec{
			Content: "<nav>[[ .Navigation.main ]]</nav>",
		},
	}

	c, writes := newStatusWriteCounter(t, navigation)
	r := &KDexPageNavigationReconciler{Client: c}
	key := types.NamespacedName{Name: "nav", Namespace: "ns"}

	assertNoStormAfterSettling(t, writes,
		func() (ctrl.Result, error) {
			return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		},
		func() []metav1.Condition {
			got := &kdexv1alpha1.KDexPageNavigation{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatalf("failed to get navigation: %v", err)
			}
			return got.Status.Conditions
		},
	)
}

func TestKDexScriptLibraryReconcileIsStableWhenSettled(t *testing.T) {
	scriptLibrary := &kdexv1alpha1.KDexScriptLibrary{
		ObjectMeta: metav1.ObjectMeta{Name: "scriptlib", Namespace: "ns"},
		Spec:       kdexv1alpha1.KDexScriptLibrarySpec{
			// No PackageReference: the reconciler skips validation entirely
			// when spec.PackageReference is nil.
		},
	}

	c, writes := newStatusWriteCounter(t, scriptLibrary)
	r := &KDexScriptLibraryReconciler{
		Client: c,
		PackageValidatorFactory: func(_ string, _ *corev1.Secret) (npm.PackageValidator, error) {
			return &MockRegistry{}, nil
		},
	}
	key := types.NamespacedName{Name: "scriptlib", Namespace: "ns"}

	assertNoStormAfterSettling(t, writes,
		func() (ctrl.Result, error) {
			return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		},
		func() []metav1.Condition {
			got := &kdexv1alpha1.KDexScriptLibrary{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatalf("failed to get scriptlibrary: %v", err)
			}
			return got.Status.Conditions
		},
	)
}

func TestKDexTranslationReconcileIsStableWhenSettled(t *testing.T) {
	translation := &kdexv1alpha1.KDexTranslation{
		ObjectMeta: metav1.ObjectMeta{Name: "translation", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexTranslationSpec{
			Translations: []kdexv1alpha1.Translation{
				{Lang: "en", KeysAndValues: map[string]string{"hello": "Hello"}},
			},
		},
	}

	c, writes := newStatusWriteCounter(t, translation)
	r := &KDexTranslationReconciler{Client: c}
	key := types.NamespacedName{Name: "translation", Namespace: "ns"}

	assertNoStormAfterSettling(t, writes,
		func() (ctrl.Result, error) {
			return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		},
		func() []metav1.Condition {
			got := &kdexv1alpha1.KDexTranslation{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatalf("failed to get translation: %v", err)
			}
			return got.Status.Conditions
		},
	)
}

func TestKDexPageArchetypeReconcileIsStableWhenSettled(t *testing.T) {
	archetype := &kdexv1alpha1.KDexPageArchetype{
		ObjectMeta: metav1.ObjectMeta{Name: "archetype", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexPageArchetypeSpec{
			Content: "<html>[[ .Content.main ]]</html>",
		},
	}

	c, writes := newStatusWriteCounter(t, archetype)
	r := &KDexPageArchetypeReconciler{Client: c}
	key := types.NamespacedName{Name: "archetype", Namespace: "ns"}

	assertNoStormAfterSettling(t, writes,
		func() (ctrl.Result, error) {
			return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		},
		func() []metav1.Condition {
			got := &kdexv1alpha1.KDexPageArchetype{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatalf("failed to get archetype: %v", err)
			}
			return got.Status.Conditions
		},
	)
}

func TestKDexUtilityPageReconcileIsStableWhenSettled(t *testing.T) {
	utilityPage := &kdexv1alpha1.KDexUtilityPage{
		ObjectMeta: metav1.ObjectMeta{Name: "utilpage", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexUtilityPageSpec{
			Type: kdexv1alpha1.KDexUtilityPageType("Error"),
			ContentEntries: []kdexv1alpha1.ContentEntry{
				{
					Slot: "main",
					ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
						RawHTML: "<p>Something went wrong.</p>",
					},
				},
			},
		},
	}

	c, writes := newStatusWriteCounter(t, utilityPage)
	r := &KDexUtilityPageReconciler{Client: c}
	key := types.NamespacedName{Name: "utilpage", Namespace: "ns"}

	assertNoStormAfterSettling(t, writes,
		func() (ctrl.Result, error) {
			return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		},
		func() []metav1.Condition {
			got := &kdexv1alpha1.KDexUtilityPage{}
			if err := c.Get(context.Background(), key, got); err != nil {
				t.Fatalf("failed to get utility page: %v", err)
			}
			return got.Status.Conditions
		},
	)
}
