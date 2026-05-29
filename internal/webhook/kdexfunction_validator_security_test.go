package webhook

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// TestValidateOpenAPIDoesNotPanicOnMalformedSecurity is a regression test for
// https://github.com/kdex-tech/nexus-manager/issues/10 - validateOpenAPI used
// unchecked type assertions on the user-supplied "security" block, panicking
// when it wasn't an array of objects.
func TestValidateOpenAPIDoesNotPanicOnMalformedSecurity(t *testing.T) {
	v := &KDexFunctionValidator[*kdexv1alpha1.KDexFunction]{}

	cases := map[string]string{
		"security is a string":              `{"security":"not-an-array","responses":{"200":{"description":"ok"}}}`,
		"security element is not an object": `{"security":["not-an-object"],"responses":{"200":{"description":"ok"}}}`,
		"security is a number":              `{"security":42,"responses":{"200":{"description":"ok"}}}`,
	}

	for name, opJSON := range cases {
		t.Run(name, func(t *testing.T) {
			spec := &kdexv1alpha1.KDexFunctionSpec{
				API: kdexv1alpha1.API{
					Paths: map[string]kdexv1alpha1.PathItem{
						"/widgets": {
							Get: &runtime.RawExtension{Raw: []byte(opJSON)},
						},
					},
				},
			}

			// The only requirement is that this does not panic. A returned
			// validation error (from the linter) is acceptable.
			_ = v.validateOpenAPI(spec)
		})
	}
}
