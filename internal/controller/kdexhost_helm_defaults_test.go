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
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
)

// Covers kdex-tech/nexus-manager#5: NexusConfiguration.HostDefault.Chart.Values
// is a cluster-wide channel for top-level host-manager chart values, merged
// UNDER the per-host spec.helm.hostManager.values so per-host always wins.
var _ = Describe("KDexHost Helm chart-value defaults", func() {
	Context("When NexusConfiguration seeds top-level chart values", func() {
		var ctx context.Context
		var mockHelmClient *MockHelmClient
		var testNamespace string
		var originalConfig *configurationSnapshot

		BeforeEach(func() {
			ctx = context.Background()
			mockHelmClient = &MockHelmClient{}
			hostReconciler.HelmClientFactory = func(namespace string, serviceAccountSecrets kdexv1alpha1.Secrets, logger logr.Logger) (utils.HelmClientInterface, error) {
				return mockHelmClient, nil
			}

			originalConfig = snapshotConfiguration()

			testNamespace = fmt.Sprintf("test-helm-defaults-%d", time.Now().UnixNano())
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		})

		AfterEach(func() {
			originalConfig.restore()
			hostReconciler.HelmClientFactory = func(namespace string, serviceAccountSecrets kdexv1alpha1.Secrets, logger logr.Logger) (utils.HelmClientInterface, error) {
				return &MockHelmClient{}, nil
			}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			_ = k8sClient.Delete(ctx, ns)
		})

		It("applies the default top-level values when the host does not override them", func() {
			hostReconciler.Configuration.HostDefault.Chart.Values = &runtime.RawExtension{
				Raw: []byte(`{"resources":{"requests":{"memory":"256Mi"}}}`),
			}

			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{Name: "defaults-host", Namespace: testNamespace},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing:      kdexv1alpha1.Routing{Domains: []string{"kdex.dev"}},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() any {
				return chartValueAt(mockHelmClient, "defaults-host", "resources", "requests", "memory")
			}, "10s", "500ms").Should(Equal("256Mi"), "Expected the cluster-default resources block to seed the chart values")
		})

		It("lets a per-host value win over the cluster default", func() {
			hostReconciler.Configuration.HostDefault.Chart.Values = &runtime.RawExtension{
				Raw: []byte(`{"resources":{"requests":{"memory":"256Mi"}}}`),
			}

			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{Name: "override-host", Namespace: testNamespace},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing:      kdexv1alpha1.Routing{Domains: []string{"kdex.dev"}},
					Helm: &kdexv1alpha1.HelmConfig{
						HostManager: &kdexv1alpha1.HostManagerHelmConfig{
							Values: "resources:\n  requests:\n    memory: 512Mi\n",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			Eventually(func() any {
				return chartValueAt(mockHelmClient, "override-host", "resources", "requests", "memory")
			}, "10s", "500ms").Should(Equal("512Mi"), "Expected the per-host value to win over the cluster default")
		})
	})
})

// chartValueAt walks the recorded chart values for a release down a path of map
// keys, returning nil if any segment is missing or not a map.
func chartValueAt(m *MockHelmClient, release string, path ...string) any {
	v, ok := m.ChartValues[release]
	if !ok {
		return nil
	}
	cur := v
	for _, key := range path {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = asMap[key]
		if !ok {
			return nil
		}
	}
	return cur
}

type configurationSnapshot struct {
	config *configuration.NexusConfiguration
}

func snapshotConfiguration() *configurationSnapshot {
	return &configurationSnapshot{config: hostReconciler.Configuration.DeepCopy()}
}

func (s *configurationSnapshot) restore() {
	hostReconciler.Configuration = *s.config
}
