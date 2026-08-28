package webhook

import (
	"context"
	"fmt"

	"github.com/kdex-tech/nexus-manager/internal/validation"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdextheme,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexthemes,verbs=create;update,versions=v1alpha1,name=validate.kdextheme.kdex.dev,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexclustertheme,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexclusterthemes,verbs=create;update,versions=v1alpha1,name=validate.kdexclustertheme.kdex.dev,admissionReviewVersions=v1

type KDexThemeValidator[T runtime.Object] struct {
}

var _ admission.Validator[*kdexv1alpha1.KDexTheme] = &KDexThemeValidator[*kdexv1alpha1.KDexTheme]{}

func (v *KDexThemeValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, v.validate(ctx, obj)
}

func (v *KDexThemeValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return nil, v.validate(ctx, newObj)
}

func (v *KDexThemeValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

func (v *KDexThemeValidator[T]) validate(_ context.Context, obj T) error {
	var spec *kdexv1alpha1.KDexThemeSpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexTheme:
		spec = &t.Spec
	case *kdexv1alpha1.KDexClusterTheme:
		spec = &t.Spec
	default:
		return fmt.Errorf("unsupported type: %T", t)
	}

	if err := validation.ValidateAssets(spec.Assets); err != nil {
		return err
	}

	// Validate ResourceProvider
	if err := validation.ValidateResourceProvider(spec); err != nil {
		return err
	}

	return nil
}
