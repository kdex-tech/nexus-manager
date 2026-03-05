package webhook

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"

	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-kdex-dev-v1alpha1-kdexpage,mutating=true,failurePolicy=Ignore,sideEffects=None,groups=kdex.dev,resources=kdexpages,verbs=create;update,versions=v1alpha1,name=mutate.kdexpage.kdex.dev,admissionReviewVersions=v1

type KDexPageDefaulter[T runtime.Object] struct {
}

func (a *KDexPageDefaulter[T]) Default(ctx context.Context, obj T) error {
	var spec *kdexv1alpha1.KDexPageSpec

	switch t := any(obj).(type) {
	case *kdexv1alpha1.KDexPage:
		spec = &t.Spec
	default:
		return fmt.Errorf("unsupported type: %T", t)
	}

	for _, entry := range spec.ContentEntries {
		if entry.AppRef != nil {
			if entry.AppRef.Kind == "" {
				entry.AppRef.Kind = "KDexApp"
			}
		}
	}

	if spec.OverrideFooterRef != nil && spec.OverrideFooterRef.Kind == "" {
		spec.OverrideFooterRef.Kind = "KDexPageFooter"
	}

	if spec.OverrideHeaderRef != nil && spec.OverrideHeaderRef.Kind == "" {
		spec.OverrideHeaderRef.Kind = "KDexPageHeader"
	}

	for _, v := range spec.OverrideNavigationRefs {
		if v.Kind == "" {
			v.Kind = KDexPageNavigation
		}
	}

	if spec.PageArchetypeRef.Kind == "" {
		spec.PageArchetypeRef.Kind = "KDexPageArchetype"
	}

	if spec.ScriptLibraryRef != nil && spec.ScriptLibraryRef.Kind == "" {
		spec.ScriptLibraryRef.Kind = KDexScriptLibrary
	}

	return nil
}
