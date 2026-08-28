package webhook

import (
	"context"
	"fmt"
	"maps"

	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdextranslation,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdextranslations,verbs=create;update,versions=v1alpha1,name=validate.kdextranslation.kdex.dev,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexclustertranslation,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexclustertranslations,verbs=create;update,versions=v1alpha1,name=validate.kdexclustertranslation.kdex.dev,admissionReviewVersions=v1

type KDexTranslationValidator[T runtime.Object] struct {
}

var _ admission.Validator[*kdexv1alpha1.KDexTranslation] = &KDexTranslationValidator[*kdexv1alpha1.KDexTranslation]{}

func (v *KDexTranslationValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, v.validate(ctx, obj)
}

func (v *KDexTranslationValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return nil, v.validate(ctx, newObj)
}

func (v *KDexTranslationValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

func (v *KDexTranslationValidator[T]) validate(_ context.Context, obj T) error {
	var spec *kdexv1alpha1.KDexTranslationSpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexTranslation:
		spec = &t.Spec
	case *kdexv1alpha1.KDexClusterTranslation:
		spec = &t.Spec
	default:
		return fmt.Errorf("unsupported type: %T", t)
	}

	if len(spec.Translations) == 0 {
		return fmt.Errorf("no translations")
	}

	// ensure that every language has the same keys as the first language
	firstKeys := maps.Keys(spec.Translations[0].KeysAndValues)
	firstLanguage := spec.Translations[0].Lang
	for _, t := range spec.Translations {
		count := 0
		for key := range firstKeys {
			if _, ok := t.KeysAndValues[key]; !ok {
				return fmt.Errorf("language %s is missing key %s", t.Lang, key)
			}
			count++
		}
		if count != len(spec.Translations[0].KeysAndValues) {
			return fmt.Errorf("language %s has different number of keys than %s", t.Lang, firstLanguage)
		}
	}

	return nil
}
