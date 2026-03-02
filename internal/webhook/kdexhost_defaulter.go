package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/mutate-kdex-dev-v1alpha1-kdexhost,mutating=true,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexhosts,verbs=create;update,versions=v1alpha1,name=mutate.kdexhost.kdex.dev,admissionReviewVersions=v1

type KDexHostDefaulter[T runtime.Object] struct {
	Configuration configuration.NexusConfiguration
}

var _ admission.Defaulter[*kdexv1alpha1.KDexHost] = &KDexHostDefaulter[*kdexv1alpha1.KDexHost]{}

func (a *KDexHostDefaulter[T]) Default(ctx context.Context, obj T) error {
	var spec *kdexv1alpha1.KDexHostSpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexHost:
		spec = &t.Spec
	default:
		return fmt.Errorf("unsupported type: %T", t)
	}

	if spec.DefaultLang == "" {
		spec.DefaultLang = "en"
	}

	if spec.ThemeRef != nil && spec.ThemeRef.Kind == "" {
		spec.ThemeRef.Kind = "KDexTheme"
	}

	if spec.ModulePolicy == "" {
		spec.ModulePolicy = kdexv1alpha1.StrictModulePolicy
	}

	if spec.Routing.Strategy == "" {
		spec.Routing.Strategy = kdexv1alpha1.IngressRoutingStrategy
	}

	if spec.ScriptLibraryRef != nil && spec.ScriptLibraryRef.Kind == "" {
		spec.ScriptLibraryRef.Kind = KDexScriptLibrary
	}

	if spec.Registries.ImageRegistry == "" {
		spec.Registries.ImageRegistry = a.Configuration.DefaultImageRegistry
	}

	if spec.Registries.NpmRegistry == "" {
		spec.Registries.NpmRegistry = a.Configuration.DefaultNpmRegistry
	}

	if spec.UtilityPages == nil {
		spec.UtilityPages = &kdexv1alpha1.UtilityPages{}
	}
	if spec.UtilityPages.AnnouncementRef == nil {
		spec.UtilityPages.AnnouncementRef = &kdexv1alpha1.KDexObjectReference{}
	}
	if spec.UtilityPages.AnnouncementRef.Kind == "" {
		spec.UtilityPages.AnnouncementRef.Kind = KDexClusterUtilityPage
	}
	if spec.UtilityPages.AnnouncementRef.Name == "" {
		spec.UtilityPages.AnnouncementRef.Name = "kdex-default-utility-page-announcement"
	}

	if spec.UtilityPages.ErrorRef == nil {
		spec.UtilityPages.ErrorRef = &kdexv1alpha1.KDexObjectReference{}
	}
	if spec.UtilityPages.ErrorRef.Kind == "" {
		spec.UtilityPages.ErrorRef.Kind = KDexClusterUtilityPage
	}
	if spec.UtilityPages.ErrorRef.Name == "" {
		spec.UtilityPages.ErrorRef.Name = "kdex-default-utility-page-error"
	}

	if spec.UtilityPages.LoginRef == nil {
		spec.UtilityPages.LoginRef = &kdexv1alpha1.KDexObjectReference{}
	}
	if spec.UtilityPages.LoginRef.Kind == "" {
		spec.UtilityPages.LoginRef.Kind = KDexClusterUtilityPage
	}
	if spec.UtilityPages.LoginRef.Name == "" {
		spec.UtilityPages.LoginRef.Name = "kdex-default-utility-page-login"
	}

	spec.IngressPath = "/-/host"

	BackendDefaults(&spec.Backend)

	return nil
}
