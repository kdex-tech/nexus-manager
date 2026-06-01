package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Regression test for issue #19: reconciling a KDexHost whose desired
// KDexInternalHost is byte-stable must not write to the KDexInternalHost on
// every pass. An unconditional write bumps resourceVersion (or at minimum
// spends an API call) on every periodic resync, fanning out downstream
// watchers for no semantic reason.
var _ = Describe("KDexInternalHost idempotent write", func() {
	Context("When the desired KDexInternalHost is unchanged", func() {
		var ctx context.Context
		var testNamespace string

		BeforeEach(func() {
			ctx = context.Background()
			testNamespace = fmt.Sprintf("test-idempotent-%d", time.Now().UnixNano())
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		})

		AfterEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			_ = k8sClient.Delete(ctx, ns)
		})

		It("does not rewrite the KDexInternalHost on a no-op reconcile", func() {
			const resourceName = "idempotent-host"

			// Create a real KDexHost so the apiserver (and defaulter webhook)
			// populate every spec default, then mirror that fully-defaulted
			// spec. A spec lacking those defaults would differ from the
			// persisted (defaulted) KDexInternalHost on every pass — a test
			// artifact, since in production host.Spec is already defaulted.
			seed := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{Name: "seed-host", Namespace: testNamespace},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					ModulePolicy: kdexv1alpha1.LooseModulePolicy,
					Routing: kdexv1alpha1.Routing{
						Domains: []string{"kdex.dev"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, seed)).To(Succeed())
			defaulted := &kdexv1alpha1.KDexHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "seed-host", Namespace: testNamespace}, defaulted)).To(Succeed())

			// A free-standing host with a DISTINCT name (so the manager never
			// reconciles it) carrying the fully-defaulted spec. A synthetic UID
			// lets SetControllerReference build a valid owner reference.
			host := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
					UID:       types.UID("idempotent-host-uid"),
				},
				Spec: defaulted.Spec,
			}

			// First call creates the KDexInternalHost.
			op1, _, err := hostReconciler.createOrUpdateInternalHostResource(ctx, host, nil, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(op1).To(Equal(controllerutil.OperationResultCreated))

			created := &kdexv1alpha1.KDexInternalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, created)).To(Succeed())
			rvAfterCreate := created.ResourceVersion

			// Second call with the identical host must be a no-op: no Update
			// API call (OperationResultNone) and no resourceVersion bump.
			op2, _, err := hostReconciler.createOrUpdateInternalHostResource(ctx, host, nil, nil, nil, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(op2).To(Equal(controllerutil.OperationResultNone),
				"expected no write on an unchanged reconcile, got %q", op2)

			after := &kdexv1alpha1.KDexInternalHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(rvAfterCreate),
				"KDexInternalHost resourceVersion must not change on a no-op reconcile")
		})
	})
})
