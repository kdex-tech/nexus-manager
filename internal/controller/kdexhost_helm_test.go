package controller

import (
	"context"
	"slices"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	helmclient "github.com/mittwald/go-helm-client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

type MockHelmClient struct {
	utils.HelmClientInterface
	InstalledCharts   []string
	UninstalledCharts []string
	ChartValues       map[string]string
}

func (m *MockHelmClient) InstallOrUpgrade(ctx context.Context, spec *helmclient.ChartSpec) error {
	m.InstalledCharts = append(m.InstalledCharts, spec.ReleaseName)
	if m.ChartValues == nil {
		m.ChartValues = make(map[string]string)
	}
	m.ChartValues[spec.ReleaseName] = spec.ValuesYaml
	return nil
}

func (m *MockHelmClient) Uninstall(releaseName string) error {
	m.UninstalledCharts = append(m.UninstalledCharts, releaseName)
	return nil
}

func (m *MockHelmClient) AddRepository(name, url string) error {
	return nil
}

var _ = Describe("KDexHost Helm Integration", func() {
	Context("When reconciling a KDexHost with Helm support", func() {
		const resourceName = "helm-host"
		ctx := context.Background()
		var mockHelmClient *MockHelmClient

		BeforeEach(func() {
			mockHelmClient = &MockHelmClient{}
			hostReconciler.HelmClientFactory = func(namespace string) (utils.HelmClientInterface, error) {
				return mockHelmClient, nil
			}
		})

		AfterEach(func() {
			cleanupResources(namespace)
		})

		It("it must install the host-manager helm chart", func() {
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

			// This test is expected to FAIL initially because the controller
			// doesn't use Helm yet.
			Eventually(func() bool {
				return slices.Contains(mockHelmClient.InstalledCharts, resourceName)
			}, "10s", "1s").Should(BeTrue(), "Expected helm chart to be installed")
		})

		It("it must install companion helm charts", func() {
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
					Helm: &kdexv1alpha1.HelmConfig{
						CompanionCharts: []kdexv1alpha1.CompanionChart{
							{
								Name:  "my-companion",
								Chart: "some-chart",
							},
						},
					},
				},
			}

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
					Helm: &kdexv1alpha1.HelmConfig{
						HostManager: &kdexv1alpha1.HostManagerHelmConfig{
							Values: "valkey:\n  enabled: false\n",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() string {
				return mockHelmClient.ChartValues[resourceName]
			}, "10s", "1s").Should(ContainSubstring("enabled: false"), "Expected valkey to be disabled via values override")
		})
	})
})
