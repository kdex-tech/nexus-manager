package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KDexHost.spec.auth.apiToken.maxTTL bounds every API token the host mints
// (host-manager#238). Its CEL rule only runs in a real apiserver, so this is
// where a bad value is proven refused. Dry-run keeps the hosts out of the
// reconcilers.
var _ = Describe("KDexHost apiToken.maxTTL", func() {
	ctx := context.Background()

	hostWith := func(maxTTL string) *kdexv1alpha1.KDexHost {
		return &kdexv1alpha1.KDexHost{
			ObjectMeta: metav1.ObjectMeta{Name: "maxttl-host", Namespace: namespace},
			Spec: kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing:      kdexv1alpha1.Routing{Domains: []string{"maxttl.example.test"}},
				Auth:         &kdexv1alpha1.Auth{APIToken: &kdexv1alpha1.APIToken{MaxTTL: maxTTL}},
			},
		}
	}

	It("accepts a positive Go duration", func() {
		Expect(k8sClient.Create(ctx, hostWith("2160h"), client.DryRunAll)).To(Succeed())
	})

	It("accepts an unset maximum", func() {
		Expect(k8sClient.Create(ctx, hostWith(""), client.DryRunAll)).To(Succeed())
	})

	DescribeTable("refuses a value that is not a positive Go duration",
		func(maxTTL string) {
			err := k8sClient.Create(ctx, hostWith(maxTTL), client.DryRunAll)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("maxTTL"))
		},
		Entry("zero", "0s"),
		Entry("negative", "-1h"),
		Entry("a day unit Go does not parse", "90d"),
		Entry("not a duration", "forever"),
	)
})
