package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-kdex-dev-v1alpha1-kdexapp,mutating=true,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexapps,verbs=create;update,versions=v1alpha1,name=mutate.kdexapp.kdex.dev,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/mutate-kdex-dev-v1alpha1-kdexclusterapp,mutating=true,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexclusterapps,verbs=create;update,versions=v1alpha1,name=mutate.kdexclusterapp.kdex.dev,admissionReviewVersions=v1

type KDexAppDefaulter[T runtime.Object] struct {
}

func (a *KDexAppDefaulter[T]) Default(ctx context.Context, obj T) error {
	var name string
	var spec *kdexv1alpha1.KDexAppSpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexApp:
		name = t.Name
		spec = &t.Spec
	case *kdexv1alpha1.KDexClusterApp:
		name = t.Name
		spec = &t.Spec
	default:
		return fmt.Errorf("unsupported type: %T", t)
	}

	spec.IngressPath = "/-/a/" + name

	if spec.PackageReference.SecretRef != nil && spec.PackageReference.SecretRef.Kind == "" {
		spec.PackageReference.SecretRef.Kind = "Secret"
	}

	BackendDefaults(&spec.Backend)

	return nil
}
