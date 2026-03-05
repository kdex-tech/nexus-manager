package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/render"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexpage,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexpages,verbs=create;update,versions=v1alpha1,name=validate.kdexpage.kdex.dev,admissionReviewVersions=v1

type KDexPageValidator[T runtime.Object] struct {
}

var _ admission.Validator[*kdexv1alpha1.KDexPage] = &KDexPageValidator[*kdexv1alpha1.KDexPage]{}

func (v *KDexPageValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *KDexPageValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *KDexPageValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

func (v *KDexPageValidator[T]) validate(_ context.Context, obj T) (admission.Warnings, error) {
	var spec *kdexv1alpha1.KDexPageSpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexPage:
		spec = &t.Spec
	default:
		return nil, fmt.Errorf("unsupported type: %T", t)
	}

	if spec.BasePath == "/" && spec.ParentPageRef != nil {
		return nil, fmt.Errorf("page with basePath '/' must not specify a parent page")
	}

	for idx, entry := range spec.ContentEntries {
		if entry.RawHTML != "" {
			if err := render.ValidateContent(entry.Slot, entry.RawHTML); err != nil {
				return nil, fmt.Errorf("invalid go template in spec.contentEntries[%d].rawHTML: %w", idx, err)
			}
		}
		if entry.AppRef != nil && entry.AppRef.Name == "" {
			return nil, fmt.Errorf("spec.contentEntries[%d].appRef.name is required", idx)
		}
	}

	return nil, nil
}
