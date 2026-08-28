package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/render"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexutilitypage,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexutilitypages,verbs=create;update,versions=v1alpha1,name=validate.kdexutilitypage.kdex.dev,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexclusterutilitypage,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexclusterutilitypages,verbs=create;update,versions=v1alpha1,name=validate.kdexclusterutilitypage.kdex.dev,admissionReviewVersions=v1

type KDexUtilityPageValidator[T runtime.Object] struct {
}

var _ admission.Validator[*kdexv1alpha1.KDexUtilityPage] = &KDexUtilityPageValidator[*kdexv1alpha1.KDexUtilityPage]{}

func (v *KDexUtilityPageValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, v.validate(ctx, obj)
}

func (v *KDexUtilityPageValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return nil, v.validate(ctx, newObj)
}

func (v *KDexUtilityPageValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

func (v *KDexUtilityPageValidator[T]) validate(_ context.Context, obj T) error {
	var spec *kdexv1alpha1.KDexUtilityPageSpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexUtilityPage:
		spec = &t.Spec
	case *kdexv1alpha1.KDexClusterUtilityPage:
		spec = &t.Spec
	default:
		return fmt.Errorf("unsupported type: %T", t)
	}

	for idx, entry := range spec.ContentEntries {
		if entry.RawHTML != "" {
			if err := render.ValidateContent(entry.Slot, entry.RawHTML); err != nil {
				return fmt.Errorf("invalid go template in spec.contentEntries[%d].rawHTML: %w", idx, err)
			}
		}
		if entry.AppRef != nil && entry.AppRef.Name == "" {
			return fmt.Errorf("spec.contentEntries[%d].appRef.name is required", idx)
		}
	}

	return nil
}
