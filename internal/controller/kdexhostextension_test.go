package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/kdex-tech/dmapper"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

var _ = Describe("KDexHostExtension attach", func() {
	ctx := context.Background()
	var hostA, hostB string
	eum := map[string]string{"kdex.dev/extension": "eum"}

	BeforeEach(func() {
		s := time.Now().UnixNano()
		hostA, hostB = fmt.Sprintf("hx-a-%d", s), fmt.Sprintf("hx-b-%d", s)
	})
	AfterEach(func() { cleanupResources(namespace) })

	host := func(name string, sel *metav1.LabelSelector) *kdexv1alpha1.KDexHost {
		return &kdexv1alpha1.KDexHost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kdexv1alpha1.KDexHostSpec{
				BrandName: "KDex Tech", Organization: "KDex Tech Inc.",
				Routing:           kdexv1alpha1.Routing{Domains: []string{name + ".example.test"}},
				ExtensionSelector: sel,
			},
		}
	}
	applied := func(h string) func() []string {
		return func() []string {
			ih := &kdexv1alpha1.KDexInternalHost{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: h}, ih); err != nil {
				return nil
			}
			out := []string{}
			for _, e := range ih.Spec.Extensions {
				out = append(out, e.Name)
			}
			return out
		}
	}
	update := func(obj client.Object, mutate func()) {
		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
				return err
			}
			mutate()
			return k8sClient.Update(ctx, obj)
		}, "10s").Should(Succeed())
	}

	It("applies nothing without a selector, then attaches in order once selected", func() {
		h := host(hostA, nil)
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		Expect(k8sClient.Create(ctx, newExtension("zeta", hostA, eum, 10))).To(Succeed())
		Expect(k8sClient.Create(ctx, newExtension("alpha", hostA, eum, 10))).To(Succeed())
		Expect(k8sClient.Create(ctx, newExtension("first", hostA, eum, -1))).To(Succeed())

		Consistently(applied(hostA), "3s", "500ms").Should(BeEmpty(), "no selector accepts none")

		update(h, func() { h.Spec.ExtensionSelector = &metav1.LabelSelector{MatchLabels: eum} })
		Eventually(applied(hostA), "20s", "500ms").Should(Equal([]string{"first", "alpha", "zeta"}))

		// Removing the selector detaches everything.
		update(h, func() { h.Spec.ExtensionSelector = nil })
		Eventually(applied(hostA), "20s", "500ms").Should(BeEmpty())
	})

	It("follows relabel, hostRef move and delete", func() {
		Expect(k8sClient.Create(ctx, host(hostA, &metav1.LabelSelector{MatchLabels: eum}))).To(Succeed())
		Expect(k8sClient.Create(ctx, host(hostB, &metav1.LabelSelector{MatchLabels: eum}))).To(Succeed())
		e := newExtension("mover", hostA, eum, 0)
		Expect(k8sClient.Create(ctx, e)).To(Succeed())
		Eventually(applied(hostA), "20s", "500ms").Should(Equal([]string{"mover"}))

		update(e, func() { e.Labels = nil })
		Eventually(applied(hostA), "20s", "500ms").Should(BeEmpty(), "relabel detaches")

		update(e, func() { e.Labels = eum; e.Spec.HostRef.Name = hostB })
		Eventually(applied(hostB), "20s", "500ms").Should(Equal([]string{"mover"}))
		Eventually(applied(hostA), "20s", "500ms").Should(BeEmpty())

		Expect(k8sClient.Delete(ctx, e)).To(Succeed())
		Eventually(applied(hostB), "20s", "500ms").Should(BeEmpty(), "delete detaches")
	})
})

var _ = Describe("KDexHostExtension status", func() {
	ctx := context.Background()
	var hostName string
	eum := map[string]string{"kdex.dev/extension": "eum"}
	BeforeEach(func() { hostName = fmt.Sprintf("hx-st-%d", time.Now().UnixNano()) })
	AfterEach(func() { cleanupResources(namespace) })

	reason := func(name string) func() string {
		return func() string {
			e := &kdexv1alpha1.KDexHostExtension{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, e); err != nil {
				return ""
			}
			c := meta.FindStatusCondition(e.Status.Conditions, "Attached")
			if c == nil {
				return ""
			}
			return c.Reason
		}
	}

	It("reports HostNotFound, NotSelected, then Attached", func() {
		Expect(k8sClient.Create(ctx, newExtension("st", hostName, eum, 0))).To(Succeed())
		Eventually(reason("st"), "20s", "500ms").Should(Equal("HostNotFound"))

		h := &kdexv1alpha1.KDexHost{
			ObjectMeta: metav1.ObjectMeta{Name: hostName, Namespace: namespace},
			Spec: kdexv1alpha1.KDexHostSpec{BrandName: "KDex Tech", Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{Domains: []string{hostName + ".example.test"}}},
		}
		Expect(k8sClient.Create(ctx, h)).To(Succeed())
		Eventually(reason("st"), "20s", "500ms").Should(Equal("NotSelected"))

		Eventually(func() error {
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(h), h); err != nil {
				return err
			}
			h.Spec.ExtensionSelector = &metav1.LabelSelector{MatchLabels: eum}
			return k8sClient.Update(ctx, h)
		}, "10s").Should(Succeed())
		Eventually(reason("st"), "20s", "500ms").Should(Equal("Attached"))
	})
})
