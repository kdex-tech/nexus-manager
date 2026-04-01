package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("KDexHost Configuration Change Integration", func() {
	Context("When the operator configuration changes", func() {
		const resourceName = "config-change-host"
		var ctx context.Context
		var mockHelmClient *MockHelmClient
		var testNamespace string

		BeforeEach(func() {
			ctx = context.Background()
			mockHelmClient = &MockHelmClient{}
			hostReconciler.HelmClientFactory = func(namespace string, serviceAccountSecrets kdexv1alpha1.Secrets, logger logr.Logger) (utils.HelmClientInterface, error) {
				return mockHelmClient, nil
			}

			// Create a unique namespace for each test
			testNamespace = fmt.Sprintf("test-config-%d", time.Now().UnixNano())
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: testNamespace,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		})

		AfterEach(func() {
			// Restore original configuration defaults if needed
			// (Assuming the test suite might rely on defaults)

			// Delete the namespace
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			_ = k8sClient.Delete(ctx, ns)
		})

		It("must upgrade the host-manager helm chart when the global default version changes", func() {
			// 1. Set initial global default version
			originalConfig := hostReconciler.Configuration.DeepCopy()
			hostReconciler.Configuration.HostDefault.Chart.Version = "1.0.0"

			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: testNamespace,
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

			// 2. Verify initial installation with version 1.0.0
			Eventually(func() string {
				return mockHelmClient.ChartVersions[resourceName]
			}, "10s", "500ms").Should(Equal("1.0.0"))

			// 3. Update the global configuration version
			hostReconciler.Configuration.HostDefault.Chart.Version = "1.1.0"

			// 4. Trigger a reconciliation by updating an annotation (does NOT bump generation)
			Eventually(func() error {
				latest := &kdexv1alpha1.KDexHost{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, latest); err != nil {
					return err
				}
				if latest.Annotations == nil {
					latest.Annotations = make(map[string]string)
				}
				latest.Annotations["trigger"] = "reconcile"
				return k8sClient.Update(ctx, latest)
			}, "10s", "500ms").Should(Succeed())

			// 5. Verify the generation DID NOT change
			latest := &kdexv1alpha1.KDexHost{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, latest)).To(Succeed())
			Expect(latest.Generation).To(Equal(resource.Generation))

			// 6. Verify that Helm upgrade was triggered anyway because the configuration hash changed
			Eventually(func() string {
				return mockHelmClient.ChartVersions[resourceName]
			}, "10s", "500ms").Should(Equal("1.1.0"), "Expected helm chart to be upgraded to new global default version")

			// Cleanup: restore config
			hostReconciler.Configuration = *originalConfig
		})
	})
})
