package webhook

import (
	"context"
	"fmt"

	"github.com/kdex-tech/nexus-manager/internal/validation"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexhost,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexhosts,verbs=create;update,versions=v1alpha1,name=validate.kdexhost.kdex.dev,admissionReviewVersions=v1

type KDexHostValidator[T runtime.Object] struct {
}

var _ admission.Validator[*kdexv1alpha1.KDexHost] = &KDexHostValidator[*kdexv1alpha1.KDexHost]{}

func (v *KDexHostValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, v.validate(ctx, obj)
}

func (v *KDexHostValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return nil, v.validate(ctx, newObj)
}

func (v *KDexHostValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

func (v *KDexHostValidator[T]) validate(_ context.Context, obj T) error {
	var host *kdexv1alpha1.KDexHost

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexHost:
		host = t
	default:
		return fmt.Errorf("unsupported type: %T", t)
	}

	spec := &host.Spec

	if spec.BrandName == "" {
		return fmt.Errorf(`spec.brandName: Invalid value: ""`)
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
