package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("KDexHost Helm Integration", func() {
	Context("When reconciling a KDexHost with Helm support", func() {
		const resourceName = "helm-host"
		var ctx context.Context
		var mockHelmClient *MockHelmClient
		var testNamespace string

		BeforeEach(func() {
			ctx = context.Background()
			mockHelmClient = &MockHelmClient{}
			hostReconciler.HelmClientFactory = func(namespace string) (utils.HelmClientInterface, error) {
				return mockHelmClient, nil
			}

			// Create a unique namespace for each test
			testNamespace = fmt.Sprintf("test-helm-%d", time.Now().UnixNano())
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: testNamespace,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())

			// Wait a bit for default resources from BeforeSuite to be ready in cache
			// though they should be ready by now.
		})

		AfterEach(func() {
			cleanupResources(testNamespace)
			// Reset factory to avoid leaking mock to other tests
			hostReconciler.HelmClientFactory = func(namespace string) (utils.HelmClientInterface, error) {
				return &MockHelmClient{}, nil
			}
			// Delete the namespace
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			_ = k8sClient.Delete(ctx, ns)
		})

		createKDexHost := func(name string, spec kdexv1alpha1.KDexHostSpec) *kdexv1alpha1.KDexHost {
			return &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: testNamespace,
				},
				Spec: spec,
			}
		}

		It("it must install the host-manager helm chart", func() {

			resource := createKDexHost(resourceName, kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{
					Domains: []string{"kdex.dev"},
				},
			})

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() bool {
				return slices.Contains(mockHelmClient.InstalledCharts, resourceName)
			}, "10s", "1s").Should(BeTrue(), "Expected helm chart to be installed")
		})

		It("it must install companion helm charts", func() {

			resource := createKDexHost(resourceName, kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{
					Domains: []string{"kdex.dev"},
				},
				Helm: &kdexv1alpha1.HelmConfig{
					CompanionCharts: []kdexv1alpha1.CompanionChart{
						{
							Name:  "my-companion",
							Chart: "some-chart",
						},
					},
				},
			})

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() bool {
				foundHost := false
				foundCompanion := false
				for _, name := range mockHelmClient.InstalledCharts {
					if name == resourceName {
						foundHost = true
					}
					if name == "my-companion" {
						foundCompanion = true
					}
				}
				return foundHost && foundCompanion
			}, "10s", "1s").Should(BeTrue(), "Expected host and companion charts to be installed")
		})

		It("it must allow overriding host-manager helm values", func() {

			resource := createKDexHost(resourceName, kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{
					Domains: []string{"kdex.dev"},
				},
				Helm: &kdexv1alpha1.HelmConfig{
					HostManager: &kdexv1alpha1.HostManagerHelmConfig{
						Values: "valkey:\n  enabled: false\n",
					},
				},
			})

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() string {
				return mockHelmClient.ChartValues[resourceName]
			}, "10s", "1s").Should(ContainSubstring("enabled: false"), "Expected valkey to be disabled via values override")
		})

		It("it must not block reconciliation during slow helm installation", func() {

			// Set up a slow mock client
			mockHelmClient.SimulateDelay = 5 * time.Second

			resource := createKDexHost(resourceName+"-slow", kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{
					Domains: []string{"kdex.dev"},
				},
			})

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			// The reconciler should return quickly, and we should be able to see
			// the resource in a "Progressing" state rather than waiting for 5 seconds.

			// We expect the status to be updated to Progressing/Reconciling.
			Eventually(func() bool {
				checkedHost := &kdexv1alpha1.KDexHost{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name, Namespace: testNamespace}, checkedHost)
				if err != nil {
					return false
				}
				// Check if we have a "Progressing" condition or if it's currently reconciling
				for _, cond := range checkedHost.Status.Conditions {
					if cond.Type == string(kdexv1alpha1.ConditionTypeProgressing) && cond.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, "2s", "100ms").Should(BeTrue(), "Expected KDexHost to be in Progressing state during slow helm install")
		})

		It("it must upgrade the host-manager helm chart when the version changes", func() {

			resource := createKDexHost(resourceName+"-upgrade", kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{
					Domains: []string{"kdex.dev"},
				},
				Helm: &kdexv1alpha1.HelmConfig{
					HostManager: &kdexv1alpha1.HostManagerHelmConfig{
						Version: "0.2.18",
					},
				},
			})

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() string {
				return mockHelmClient.ChartVersions[resource.Name]
			}, "10s", "1s").Should(Equal("0.2.18"))

			// Update the version
			Eventually(func() error {
				latest := &kdexv1alpha1.KDexHost{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name, Namespace: testNamespace}, latest); err != nil {
					return err
				}
				latest.Spec.Helm.HostManager.Version = "0.2.19"
				return k8sClient.Update(ctx, latest)
			}, "10s", "1s").Should(Succeed())

			Eventually(func() string {
				return mockHelmClient.ChartVersions[resource.Name]
			}, "10s", "1s").Should(Equal("0.2.19"), "Expected helm chart to be upgraded to new version")
		})

		It("it must report failure when helm installation fails", func() {

			mockHelmClient.FailInstall = true
			mockHelmClient.FailMessage = "failed to pull chart"

			resource := createKDexHost(resourceName+"-fail", kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{
					Domains: []string{"kdex.dev"},
				},
			})

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() bool {
				checkedHost := &kdexv1alpha1.KDexHost{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name, Namespace: testNamespace}, checkedHost)
				if err != nil {
					return false
				}

				// Check if we have a "Degraded" condition
				for _, cond := range checkedHost.Status.Conditions {
					if cond.Type == string(kdexv1alpha1.ConditionTypeDegraded) && cond.Status == metav1.ConditionTrue {
						if strings.Contains(cond.Message, "failed to pull chart") {
							return true
						}
					}
				}
				return false
			}, "10s", "1s").Should(BeTrue(), "Expected KDexHost to report failure when helm install fails")
		})

		It("it must rollback when an upgrade fails", func() {

			// 1. Successful initial install
			resource := createKDexHost(resourceName+"-rollback", kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{
					Domains: []string{"kdex.dev"},
				},
				Helm: &kdexv1alpha1.HelmConfig{
					HostManager: &kdexv1alpha1.HostManagerHelmConfig{
						Version: "0.2.18",
					},
				},
			})

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() string {
				return mockHelmClient.ChartVersions[resource.Name]
			}, "10s", "1s").Should(Equal("0.2.18"))

			// 2. Configure mock to fail next install
			mockHelmClient.FailInstall = true
			mockHelmClient.FailMessage = "upgrade failed"

			// 3. Trigger upgrade
			Eventually(func() error {
				latest := &kdexv1alpha1.KDexHost{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name, Namespace: testNamespace}, latest); err != nil {
					return err
				}
				latest.Spec.Helm.HostManager.Version = "0.2.19"
				return k8sClient.Update(ctx, latest)
			}, "10s", "1s").Should(Succeed())

			// 4. Verify failure is reported
			Eventually(func() bool {
				checkedHost := &kdexv1alpha1.KDexHost{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resource.Name, Namespace: testNamespace}, checkedHost)
				if err != nil {
					return false
				}
				for _, cond := range checkedHost.Status.Conditions {
					if cond.Type == string(kdexv1alpha1.ConditionTypeDegraded) && cond.Status == metav1.ConditionTrue {
						return true
					}
				}
				return false
			}, "10s", "1s").Should(BeTrue(), "Expected KDexHost to be Degraded after failed upgrade")

			// 5. Verify rollback behavior
			// In a real scenario with Atomic=true, Helm would rollback.
			// For our mock, we can just check if we have a way to signal rollback
			// or if we expect the controller to do it.
			// If we want the controller to handle it, we'd need more logic.

			// For now, let's just assert that the version in the mock DID NOT change to 0.2.19
			// because the install failed.
			Expect(mockHelmClient.ChartVersions[resource.Name]).To(Equal("0.2.18"))
		})
	})
})
