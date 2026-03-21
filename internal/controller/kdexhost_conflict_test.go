package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("KDexHost Conflict Reproduction", func() {
	Context("When reconciling a KDexHost", func() {
		const resourceName = "conflict-host"
		ctx := context.Background()

		BeforeEach(func() {
			// No special setup needed, just use the real reconciler
		})

		AfterEach(func() {
			cleanupResources(namespace)
		})

		It("should not fail with conflict error during initial reconciliation", func() {
			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{"kdex.dev"},
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			serviceAccount := &corev1.ServiceAccount{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resource.Name,
					Namespace: namespace,
				},
			}
			Eventually(func() error {
				err := k8sClient.Create(ctx, serviceAccount)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "5s").Should(Succeed())

			// We wait for the resource to be processed.
			// If there's a conflict error in the reconciler, it usually shows up in the logs
			// and might cause the resource to never reach a stable state or take longer.

			// To catch the conflict error, we can check if the status update eventually succeeds.
			// But the goal is to SEE the error in the logs if we run with high verbosity.

			Eventually(func() bool {
				checkedHost := &kdexv1alpha1.KDexHost{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: namespace}, checkedHost)
				if err != nil {
					return false
				}
				// If it has a finalizer, the first part of reconcile finished.
				return len(checkedHost.Finalizers) > 0
			}, "10s", "1s").Should(BeTrue())
		})
	})
})
