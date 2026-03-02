package webhook

import (
	"context"
	"fmt"

	"github.com/kdex-tech/nexus-manager/internal/validation"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexapp,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexapps,verbs=create;update,versions=v1alpha1,name=validate.kdexapp.kdex.dev,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexclusterapp,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexclusterapps,verbs=create;update,versions=v1alpha1,name=validate.kdexclusterapp.kdex.dev,admissionReviewVersions=v1

type KDexAppValidator[T runtime.Object] struct {
}

var _ admission.Validator[*kdexv1alpha1.KDexApp] = &KDexAppValidator[*kdexv1alpha1.KDexApp]{}

func (v *KDexAppValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *KDexAppValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *KDexAppValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

func (v *KDexAppValidator[T]) validate(_ context.Context, obj T) (admission.Warnings, error) {
	clusterScoped := false
	var spec *kdexv1alpha1.KDexAppSpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexApp:
		spec = &t.Spec
	case *kdexv1alpha1.KDexClusterApp:
		clusterScoped = true
		spec = &t.Spec
	default:
		return nil, fmt.Errorf("unsupported type: %T", t)
	}

	// apply the same logic as KDexScriptLibrary
	sl := &kdexv1alpha1.KDexScriptLibrarySpec{
		Backend:          spec.Backend,
		PackageReference: &spec.PackageReference,
		Scripts:          spec.Scripts,
	}

	if spec.PackageReference.SecretRef != nil && spec.PackageReference.SecretRef.Name == "" {
		return nil, fmt.Errorf("spec.packageReference.secretRef.name is required")
	}

	if clusterScoped && spec.PackageReference.SecretRef != nil && spec.PackageReference.SecretRef.Namespace == "" {
		return nil, fmt.Errorf("spec.packageReference.secretRef.namespace is required for cluster scoped apps")
	}

	if err := validation.ValidateScriptLibrary(sl); err != nil {
		return nil, err
	}

	// Validate ResourceProvider
	if err := validation.ValidateResourceProvider(spec); err != nil {
		return nil, err
	}

	return nil, nil
}
