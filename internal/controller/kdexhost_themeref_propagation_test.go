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
)

// Regression test for issue #2: a KDexHost whose ThemeRef points at a
// KDexTheme that does not exist must still mirror the rest of its spec to the
// KDexInternalHost. Previously the reconciler short-circuited on the missing
// theme and never reached the mirror logic, so unrelated spec changes (e.g. a
// host-manager version bump carrying security fixes) were stranded.
var _ = Describe("KDexHost spec propagation with unresolved ThemeRef", func() {
	Context("When ThemeRef points at a non-existent theme", func() {
		var ctx context.Context
		var testNamespace string

		BeforeEach(func() {
			ctx = context.Background()
			testNamespace = fmt.Sprintf("test-themeref-%d", time.Now().UnixNano())
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		})

		AfterEach(func() {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNamespace}}
			_ = k8sClient.Delete(ctx, ns)
		})

		It("mirrors unrelated spec changes to KDexInternalHost despite the missing theme", func() {
			const resourceName = "themeref-host"
			host := &kdexv1alpha1.KDexHost{
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
					// A reference that will never resolve in this test.
					ThemeRef: &kdexv1alpha1.KDexObjectReference{
						Kind: "KDexTheme",
						Name: "non-existent-theme",
					},
					// The "unrelated" spec field that must propagate even while
					// the theme is missing (models a security patch version bump).
					Helm: &kdexv1alpha1.HelmConfig{
						HostManager: &kdexv1alpha1.HostManagerHelmConfig{
							Version: "9.9.9",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, host)).To(Succeed())

			// The reconciler must create the KDexInternalHost and mirror the
			// host-manager version, even though the theme cannot be resolved.
			Eventually(func(g Gomega) {
				internalHost := &kdexv1alpha1.KDexInternalHost{}
				g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: testNamespace}, internalHost)).To(Succeed())
				g.Expect(internalHost.Spec.KDexHostSpec.Helm).NotTo(BeNil())
				g.Expect(internalHost.Spec.KDexHostSpec.Helm.HostManager).NotTo(BeNil())
				g.Expect(internalHost.Spec.KDexHostSpec.Helm.HostManager.Version).To(Equal("9.9.9"))
			}, "15s", "500ms").Should(Succeed())
		})
	})
})
