package webhook

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

func translationWith(lang string, kv map[string]string) *kdexv1alpha1.KDexTranslation {
	return &kdexv1alpha1.KDexTranslation{Spec: kdexv1alpha1.KDexNamespacedTranslationSpec{
		KDexTranslationSpec: kdexv1alpha1.KDexTranslationSpec{Translations: []kdexv1alpha1.Translation{
			{Lang: lang, KeysAndValues: kv},
		}},
	}}
}

// host-manager compiles every value into a golang.org/x/text message catalog;
// a value that does not compile is rejected at admission, naming the language,
// the key and the parse error.
func TestKDexTranslationValidator_RejectsValueThatDoesNotCompile(t *testing.T) {
	v := &KDexTranslationValidator[*kdexv1alpha1.KDexTranslation]{}

	for _, tc := range []struct {
		value   string
		wantErr string
	}{
		{value: "Price: ${", wantErr: "missing '}'"},
		{value: "Use ${x(abc)} here", wantErr: `invalid number "abc"`},
	} {
		t.Run(tc.value, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), translationWith("fr", map[string]string{
				"good": "Bon",
				"bad":  tc.value,
			}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "fr")
			assert.Contains(t, err.Error(), `"bad"`)
			assert.Contains(t, err.Error(), tc.wantErr)

			_, err = v.ValidateUpdate(context.Background(), nil, translationWith("fr", map[string]string{"bad": tc.value}))
			require.Error(t, err)
		})
	}
}

func TestKDexTranslationValidator_AcceptsValueThatCompiles(t *testing.T) {
	v := &KDexTranslationValidator[*kdexv1alpha1.KDexTranslation]{}
	_, err := v.ValidateCreate(context.Background(), translationWith("en", map[string]string{
		"price":   "Price: %s",
		"literal": "Costs $5 {not a placeholder}",
	}))
	assert.NoError(t, err)
}

func TestKDexClusterTranslationValidator_RejectsValueThatDoesNotCompile(t *testing.T) {
	v := &KDexTranslationValidator[*kdexv1alpha1.KDexClusterTranslation]{}
	_, err := v.ValidateCreate(context.Background(), &kdexv1alpha1.KDexClusterTranslation{
		Spec: kdexv1alpha1.KDexTranslationSpec{Translations: []kdexv1alpha1.Translation{
			{Lang: "en", KeysAndValues: map[string]string{"bad": "Price: ${"}},
		}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing '}'")
}

// The bundled default translation must itself pass admission.
func TestBundledDefaultTranslationCompiles(t *testing.T) {
	raw, err := os.ReadFile("../../config/bundled/kdex-default-translation.yaml")
	require.NoError(t, err)
	var tr kdexv1alpha1.KDexClusterTranslation
	require.NoError(t, yaml.Unmarshal(raw, &tr))
	require.NotEmpty(t, tr.Spec.Translations)

	v := &KDexTranslationValidator[*kdexv1alpha1.KDexClusterTranslation]{}
	_, err = v.ValidateCreate(context.Background(), &tr)
	assert.NoError(t, err)
}
