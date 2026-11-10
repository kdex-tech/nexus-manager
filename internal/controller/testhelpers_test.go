package controller

import (
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// withTeardownIndexes registers the indexes a host's teardown lists its
// internal pages and translations by, as SetupWithManager does. b must
// already have its scheme.
func withTeardownIndexes(b *fake.ClientBuilder) *fake.ClientBuilder {
	return b.
		WithIndex(&kdexv1alpha1.KDexInternalUtilityPage{}, hostIndexKey, indexInternalUtilityPageByHost).
		WithIndex(&kdexv1alpha1.KDexInternalTranslation{}, hostIndexKey, indexInternalTranslationByHost)
}
