package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// spec.origin and spec.backend are mutually exclusive, enforced by CEL on the
// CRD. Origin became optional in kdex-crds#19, which moves the rule's footing
// twice: `has(self.origin.executable)` now errors on an object that omits
// origin entirely, and every KDexFunction stored before that change carries a
// phantom `origin: {}` — including the service-backed ones, which have a real
// backend. The middle case below is the one that matters: collapse the rule to
// `!(has(self.origin) && has(self.backend))` and every one of those stored
// objects becomes unwritable.
var _ = Describe("KDexFunction origin/backend mutex", func() {
	ctx := context.Background()

	newFunction := func(name string) *kdexv1alpha1.KDexFunction {
		return &kdexv1alpha1.KDexFunction{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kdexv1alpha1.KDexFunctionSpec{
				HostRef: corev1.LocalObjectReference{Name: "some-host"},
				API: kdexv1alpha1.API{
					BasePath: "/v1/things",
					Paths: map[string]kdexv1alpha1.PathItem{
						"/v1/things": {},
					},
				},
			},
		}
	}

	serviceBackend := &kdexv1alpha1.FunctionBackend{
		Type: "Service",
		Service: &kdexv1alpha1.ServiceBackend{
			Name: "things",
			Port: intstr.FromInt32(8080),
		},
	}

	AfterEach(func() { cleanupResources(namespace) })

	It("accepts a backend when origin is absent", func() {
		fn := newFunction("mutex-backend-only")
		fn.Spec.Backend = serviceBackend

		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
	})

	It("accepts a backend alongside the empty origin legacy objects still carry", func() {
		fn := newFunction("mutex-phantom-origin")
		fn.Spec.Backend = serviceBackend
		fn.Spec.Origin = &kdexv1alpha1.FunctionOrigin{}

		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
	})

	It("rejects a backend alongside a populated origin", func() {
		fn := newFunction("mutex-both")
		fn.Spec.Backend = serviceBackend
		fn.Spec.Origin = &kdexv1alpha1.FunctionOrigin{
			Source: &kdexv1alpha1.Source{
				Repository: "https://example.com/repo.git",
				Revision:   "main",
			},
		}

		err := k8sClient.Create(ctx, fn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})

	It("keeps an origin-only function valid", func() {
		fn := newFunction("mutex-origin-only")
		fn.Spec.Origin = &kdexv1alpha1.FunctionOrigin{
			Source: &kdexv1alpha1.Source{
				Repository: "https://example.com/repo.git",
				Revision:   "main",
			},
		}

		Expect(k8sClient.Create(ctx, fn)).To(Succeed())
	})
})
