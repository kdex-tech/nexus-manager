package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KDexHost.spec.routing.hsts configures Strict-Transport-Security
// (host-manager#248). Its bounds and the preload CEL rule only run in a real
// apiserver, so this is where a bad value is proven refused. Dry-run keeps the
// hosts out of the reconcilers.
var _ = Describe("KDexHost routing.hsts", func() {
	ctx := context.Background()

	hostWith := func(h *kdexv1alpha1.HSTS) *kdexv1alpha1.KDexHost {
		return &kdexv1alpha1.KDexHost{
			ObjectMeta: metav1.ObjectMeta{Name: "hsts-host", Namespace: namespace},
			Spec: kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing: kdexv1alpha1.Routing{
					Domains: []string{"hsts.example.test"},
					HSTS:    h,
				},
			},
		}
	}

	DescribeTable("accepts",
		func(h *kdexv1alpha1.HSTS) {
			Expect(k8sClient.Create(ctx, hostWith(h), client.DryRunAll)).To(Succeed())
		},
		Entry("an unset hsts", nil),
		Entry("an empty hsts", &kdexv1alpha1.HSTS{}),
		Entry("maxAge zero", &kdexv1alpha1.HSTS{MaxAge: new(int32(0))}),
		Entry("maxAge at the ceiling", &kdexv1alpha1.HSTS{MaxAge: new(int32(63072000))}),
		Entry("preload with an unset maxAge", &kdexv1alpha1.HSTS{IncludeSubDomains: true, Preload: true}),
		Entry("preload at the minimum maxAge", &kdexv1alpha1.HSTS{
			MaxAge: new(int32(31536000)), IncludeSubDomains: true, Preload: true,
		}),
	)

	DescribeTable("refuses",
		func(h *kdexv1alpha1.HSTS, want string) {
			err := k8sClient.Create(ctx, hostWith(h), client.DryRunAll)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(want))
		},
		Entry("preload without includeSubDomains",
			&kdexv1alpha1.HSTS{Preload: true}, "preload requires includeSubDomains"),
		Entry("preload with a maxAge under a year",
			&kdexv1alpha1.HSTS{MaxAge: new(int32(86400)), IncludeSubDomains: true, Preload: true},
			"preload requires includeSubDomains"),
		Entry("a negative maxAge", &kdexv1alpha1.HSTS{MaxAge: new(int32(-1))}, "maxAge"),
		Entry("a maxAge over the ceiling", &kdexv1alpha1.HSTS{MaxAge: new(int32(63072001))}, "maxAge"),
	)
})
