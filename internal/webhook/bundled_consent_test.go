package webhook

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/render"
	"sigs.k8s.io/yaml"
)

const (
	bundledConsentPagePath   = "../../config/bundled/kdex-default-utility-page-consent.yaml"
	bundledTranslationPath   = "../../config/bundled/kdex-default-translation.yaml"
	bundledKustomizationPath = "../../config/bundled/kustomization.yaml"
)

func TestBundledConsentPage(t *testing.T) {
	raw, err := os.ReadFile(bundledConsentPagePath)
	require.NoError(t, err)

	var page kdexv1alpha1.KDexClusterUtilityPage
	require.NoError(t, yaml.UnmarshalStrict(raw, &page))

	assert.Equal(t, KDexDefaultUtilityPageConsent, page.Name)
	assert.Equal(t, "kdex-default-utility-page-consent", page.Name)
	assert.Equal(t, kdexv1alpha1.ConsentUtilityPageType, page.Spec.Type)
	require.NotNil(t, page.Spec.PageArchetypeRef)
	assert.Equal(t, "kdex-default-page-archetype-utility", page.Spec.PageArchetypeRef.Name)
	assert.Nil(t, page.Spec.ScriptLibraryRef)

	require.NotEmpty(t, page.Spec.ContentEntries)
	var main string
	for _, e := range page.Spec.ContentEntries {
		assert.NotEmpty(t, e.RawHTML, "slot %q must be rawHTML", e.Slot)
		assert.Empty(t, e.AppRef, "slot %q must not reference an app", e.Slot)
		assert.NotContains(t, strings.ToLower(e.RawHTML), "<script", "slot %q", e.Slot)
		require.NoError(t, render.ValidateContent("consent-"+e.Slot, e.RawHTML))
		if e.Slot == "main" {
			main = e.RawHTML
		}
	}
	require.NotEmpty(t, main, "main slot required")
	assert.Equal(t, 1, strings.Count(main, ".Extra.consent.Block"))
}

func TestBundledTranslationHasConsentKeys(t *testing.T) {
	raw, err := os.ReadFile(bundledTranslationPath)
	require.NoError(t, err)

	var tr kdexv1alpha1.KDexClusterTranslation
	require.NoError(t, yaml.Unmarshal(raw, &tr))

	byLang := map[string]map[string]string{}
	for _, x := range tr.Spec.Translations {
		byLang[x.Lang] = x.KeysAndValues
	}
	for _, lang := range []string{"en", "de", "es", "fr", "it", "pt", "ru", "zh"} {
		kv, ok := byLang[lang]
		require.True(t, ok, "language %s missing", lang)
		for _, key := range render.ConsentMessageKeys() {
			v := kv[key]
			assert.NotEmpty(t, v, "%s/%s", lang, key)
			en := render.ConsentMessages[key]
			if lang == "en" {
				assert.Equal(t, en, v, "%s/%s", lang, key)
			}
			assert.Equal(t, strings.Count(en, "%s"), strings.Count(v, "%s"), "%s/%s placeholders", lang, key)
		}
	}
}

func TestBundledKustomizationListsConsentPage(t *testing.T) {
	raw, err := os.ReadFile(bundledKustomizationPath)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "- kdex-default-utility-page-consent.yaml")
}
