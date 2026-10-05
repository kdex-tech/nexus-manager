package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/kdex-tech/dmapper"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

func newExtension(name, host string, lbl map[string]string, weight int32) *kdexv1alpha1.KDexHostExtension {
	return &kdexv1alpha1.KDexHostExtension{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: lbl},
		Spec: kdexv1alpha1.KDexHostExtensionSpec{
			HostRef: corev1.LocalObjectReference{Name: host},
			Weight:  weight,
			ClaimMappings: []dmapper.MappingRule{{
				SourceExpression: "has(self.extra_grants) ? self.extra_grants : []",
				TargetPropPath:   "entitlements",
			}},
			AnonymousEntitlements: []string{"functions:/" + name + ":read"},
		},
	}
}

var _ = Describe("KDexHostExtension validation", func() {
	ctx := context.Background()
	var suffix int64
	BeforeEach(func() { suffix = time.Now().UnixNano() })
	AfterEach(func() { cleanupResources(namespace) })

	It("accepts a valid extension", func() {
		Expect(k8sClient.Create(ctx, newExtension(fmt.Sprintf("ok-%d", suffix), "h", nil, 0))).To(Succeed())
	})

	DescribeTable("rejects",
		func(mutate func(*kdexv1alpha1.KDexHostExtension), want string) {
			e := newExtension(fmt.Sprintf("bad-%d", suffix), "h", nil, 0)
			mutate(e)
			err := k8sClient.Create(ctx, e)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(want))
		},
		Entry("empty hostRef", func(e *kdexv1alpha1.KDexHostExtension) { e.Spec.HostRef.Name = "" }, "hostRef.name must not be empty"),
		Entry("weight above 1000", func(e *kdexv1alpha1.KDexHostExtension) { e.Spec.Weight = 1001 }, "spec.weight"),
		Entry("merge: Replace", func(e *kdexv1alpha1.KDexHostExtension) { e.Spec.ClaimMappings[0].Merge = dmapper.MergeReplace }, "cannot use merge: Replace"),
		Entry("reserved target aud", func(e *kdexv1alpha1.KDexHostExtension) { e.Spec.ClaimMappings[0].TargetPropPath = "aud" }, "reserved token claim"),
		Entry("reserved target under scope.", func(e *kdexv1alpha1.KDexHostExtension) { e.Spec.ClaimMappings[0].TargetPropPath = "scope.x" }, "reserved token claim"),
		Entry("anonymous wildcard name", func(e *kdexv1alpha1.KDexHostExtension) { e.Spec.AnonymousEntitlements = []string{"pages:*:read"} }, "name other than empty or *"),
		Entry("anonymous empty name", func(e *kdexv1alpha1.KDexHostExtension) { e.Spec.AnonymousEntitlements = []string{"pages::read"} }, "name other than empty or *"),
		Entry("anonymous two segments", func(e *kdexv1alpha1.KDexHostExtension) { e.Spec.AnonymousEntitlements = []string{"pages:read"} }, "name other than empty or *"),
	)

	It("rejects an invalid extensionSelector on a KDexHost", func() {
		host := &kdexv1alpha1.KDexHost{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("sel-%d", suffix), Namespace: namespace},
			Spec: kdexv1alpha1.KDexHostSpec{
				BrandName: "KDex Tech", Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{Domains: []string{"sel.example.test"}},
				ExtensionSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "kdex.dev/extension", Operator: "Bogus"},
				}},
			},
		}
		err := k8sClient.Create(ctx, host)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.extensionSelector"))
	})
})
