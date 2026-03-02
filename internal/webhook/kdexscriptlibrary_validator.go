package webhook

import (
	"context"
	"fmt"

	"github.com/kdex-tech/nexus-manager/internal/validation"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexscriptlibrary,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexscriptlibraries,verbs=create;update,versions=v1alpha1,name=validate.kdexscriptlibrary.kdex.dev,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-kdex-dev-v1alpha1-kdexclusterscriptlibrary,mutating=false,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexclusterscriptlibraries,verbs=create;update,versions=v1alpha1,name=validate.kdexclusterscriptlibrary.kdex.dev,admissionReviewVersions=v1

type KDexScriptLibraryValidator[T runtime.Object] struct {
}

var _ admission.Validator[*kdexv1alpha1.KDexScriptLibrary] = &KDexScriptLibraryValidator[*kdexv1alpha1.KDexScriptLibrary]{}

func (v *KDexScriptLibraryValidator[T]) ValidateCreate(ctx context.Context, obj T) (admission.Warnings, error) {
	return v.validate(ctx, obj)
}

func (v *KDexScriptLibraryValidator[T]) ValidateUpdate(ctx context.Context, oldObj, newObj T) (admission.Warnings, error) {
	return v.validate(ctx, newObj)
}

func (v *KDexScriptLibraryValidator[T]) ValidateDelete(ctx context.Context, obj T) (admission.Warnings, error) {
	return nil, nil
}

func (v *KDexScriptLibraryValidator[T]) validate(_ context.Context, obj T) (admission.Warnings, error) {
	clusterScoped := false
	var spec *kdexv1alpha1.KDexScriptLibrarySpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexScriptLibrary:
		spec = &t.Spec
	case *kdexv1alpha1.KDexClusterScriptLibrary:
		clusterScoped = true
		spec = &t.Spec
	default:
		return nil, fmt.Errorf("unsupported type: %T", t)
	}

	if spec.PackageReference != nil && spec.PackageReference.SecretRef != nil && spec.PackageReference.SecretRef.Name == "" {
		return nil, fmt.Errorf("spec.packageReference.secretRef.name is required")
	}

	if clusterScoped && spec.PackageReference != nil && spec.PackageReference.SecretRef != nil && spec.PackageReference.SecretRef.Namespace == "" {
		return nil, fmt.Errorf("spec.packageReference.secretRef.namespace is required for cluster scoped script libraries")
	}

	if err := validation.ValidateScriptLibrary(spec); err != nil {
		return nil, err
	}

	// Validate ResourceProvider
	if err := validation.ValidateResourceProvider(spec); err != nil {
		return nil, err
	}

	return nil, nil
}
