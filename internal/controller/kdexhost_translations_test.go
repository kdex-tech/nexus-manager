package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func trSource(kind, ns, name string) translationSource {
	return translationSource{Kind: kind, Namespace: ns, Name: name}
}

func sourceNames(ss []translationSource) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func TestOrderTranslationSources_PrecedenceLowestToHighest(t *testing.T) {
	def := trSource("KDexClusterTranslation", "", "kdex-default-translation")
	self := []translationSource{trSource("KDexTranslation", "site", "zeta"), trSource("KDexTranslation", "site", "alpha")}
	declared := []translationSource{trSource("KDexTranslation", "site", "brand"), trSource("KDexClusterTranslation", "", "shared")}

	ordered, collision := orderTranslationSources(&def, self, declared)

	assert.Equal(t, []string{"kdex-default-translation", "alpha", "zeta", "brand", "shared"}, sourceNames(ordered))
	assert.Empty(t, collision)
}

func TestOrderTranslationSources_DeclaredPositionWinsOverSelfAttached(t *testing.T) {
	self := []translationSource{trSource("KDexTranslation", "site", "brand"), trSource("KDexTranslation", "site", "extra")}
	declared := []translationSource{trSource("KDexTranslation", "site", "brand")}

	ordered, collision := orderTranslationSources(nil, self, declared)

	assert.Equal(t, []string{"extra", "brand"}, sourceNames(ordered), "brand keeps only its host-declared (higher) position")
	assert.Empty(t, collision, "the same object twice is a duplicate, not a collision")
}

func TestOrderTranslationSources_NameCollisionKeepsHigherPrecedence(t *testing.T) {
	def := trSource("KDexClusterTranslation", "", "kdex-default-translation")
	self := []translationSource{trSource("KDexTranslation", "site", "kdex-default-translation")}

	ordered, collision := orderTranslationSources(&def, self, nil)

	assert.Len(t, ordered, 1)
	assert.Equal(t, "KDexTranslation", ordered[0].Kind, "the self-attached source outranks the default")
	assert.Contains(t, collision, "KDexClusterTranslation//kdex-default-translation")
	assert.Contains(t, collision, "KDexTranslation/site/kdex-default-translation")
}

func TestOrderTranslationSources_NilDefaultAndEmpty(t *testing.T) {
	ordered, collision := orderTranslationSources(nil, nil, nil)
	assert.Empty(t, ordered)
	assert.Empty(t, collision)
}
