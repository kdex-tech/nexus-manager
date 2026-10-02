/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/kdex-tech/nexus-manager/internal/webhook"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func (r *KDexHostReconciler) createOrUpdateInternalTranslation(
	ctx context.Context,
	translationSpec kdexv1alpha1.KDexTranslationSpec,
	translationName string,
	generation int64,
	host *kdexv1alpha1.KDexHost,
) (*kdexv1alpha1.KDexInternalTranslation, error) {
	name := fmt.Sprintf("%s-%s", host.Name, translationName)
	internalTranslation := &kdexv1alpha1.KDexInternalTranslation{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: host.Namespace,
		},
	}

	// Use Patch instead of CreateOrUpdate to be more resilient to conflicts
	op, err := controllerutil.CreateOrPatch(ctx, r.Client, internalTranslation, func() error {
		if internalTranslation.Annotations == nil {
			internalTranslation.Annotations = make(map[string]string)
		}
		if internalTranslation.Labels == nil {
			internalTranslation.Labels = make(map[string]string)
		}

		if internalTranslation.CreationTimestamp.IsZero() {
			maps.Copy(internalTranslation.Annotations, host.Annotations)
			maps.Copy(internalTranslation.Labels, host.Labels)

			internalTranslation.Labels["app.kubernetes.io/name"] = kdexWeb
			internalTranslation.Labels["kdex.dev/instance"] = host.Name
		}

		internalTranslation.Labels["kdex.dev/generation"] = fmt.Sprintf("%d", generation)
		internalTranslation.Spec.KDexTranslationSpec = translationSpec
		internalTranslation.Spec.HostRef = corev1.LocalObjectReference{Name: host.Name}

		return ctrl.SetControllerReference(host, internalTranslation, r.Scheme)
	})

	if err == nil {
		// Update status separately
		latest := &kdexv1alpha1.KDexInternalTranslation{}
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(internalTranslation), latest); getErr == nil {
			patch := client.MergeFrom(latest.DeepCopy())
			if latest.Status.Attributes == nil {
				latest.Status.Attributes = make(map[string]string)
			}
			latest.Status.Attributes["translation.generation"] = strconv.FormatInt(generation, 10)
			_ = r.Status().Patch(ctx, latest, patch)
		}
	}

	log := logf.FromContext(ctx).WithName("translation")

	log.V(2).Info(
		"createOrUpdateInternalTranslation",
		"name", internalTranslation.Name,
		"op", op,
		"err", err,
	)

	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileError,
			err.Error(),
		)
		return nil, err
	}

	return internalTranslation, nil
}

func (r *KDexHostReconciler) createOrUpdateInternalUtilityPage(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
	utilityPageSpec kdexv1alpha1.KDexUtilityPageSpec,
	pageType kdexv1alpha1.KDexUtilityPageType,
	utilityPageGeneration int64,
) (*corev1.LocalObjectReference, error) {
	name := fmt.Sprintf("%s-%s", host.Name, strings.ToLower(string(pageType)))
	internalUtilityPage := &kdexv1alpha1.KDexInternalUtilityPage{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: host.Namespace,
		},
	}

	op, err := controllerutil.CreateOrPatch(ctx, r.Client, internalUtilityPage, func() error {
		if internalUtilityPage.Annotations == nil {
			internalUtilityPage.Annotations = make(map[string]string)
		}
		if internalUtilityPage.Labels == nil {
			internalUtilityPage.Labels = make(map[string]string)
		}

		if internalUtilityPage.CreationTimestamp.IsZero() {
			maps.Copy(internalUtilityPage.Annotations, host.Annotations)
			maps.Copy(internalUtilityPage.Labels, host.Labels)

			internalUtilityPage.Labels["app.kubernetes.io/name"] = kdexWeb
			internalUtilityPage.Labels["kdex.dev/instance"] = host.Name
			internalUtilityPage.Labels["kdex.dev/utility-page-type"] = string(pageType)
		}

		internalUtilityPage.Labels["kdex.dev/generation"] = fmt.Sprintf("%d", utilityPageGeneration)
		internalUtilityPage.Spec.KDexUtilityPageSpec = utilityPageSpec
		internalUtilityPage.Spec.HostRef = corev1.LocalObjectReference{Name: host.Name}

		return ctrl.SetControllerReference(host, internalUtilityPage, r.Scheme)
	})

	if err == nil {
		// Update status separately
		latest := &kdexv1alpha1.KDexInternalUtilityPage{}
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(internalUtilityPage), latest); getErr == nil {
			patch := client.MergeFrom(latest.DeepCopy())
			if latest.Status.Attributes == nil {
				latest.Status.Attributes = make(map[string]string)
			}
			latest.Status.Attributes["utilitypage.generation"] = strconv.FormatInt(utilityPageGeneration, 10)
			_ = r.Status().Patch(ctx, latest, patch)
		}
	}

	log := logf.FromContext(ctx).WithName("utilitypage")

	log.V(2).Info(
		"createOrUpdateInternalUtilityPage",
		"name", internalUtilityPage.Name,
		"type", pageType,
		"op", op,
		"err", err,
	)

	if err != nil {
		return nil, err
	}

	return &corev1.LocalObjectReference{Name: name}, nil
}

// resolveTranslations resolves every translation attached to host: the default,
// KDexTranslations that name the host in spec.hostRef, and the host's own
// translationRefs. It writes one KDexInternalTranslation per source, prunes the
// copies that are no longer attached, and returns their names in precedence
// order (lowest first), which host-manager uses as its catalog write order.
// collision is non-empty when two distinct sources map to one internal name.
func (r *KDexHostReconciler) resolveTranslations(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
) ([]corev1.LocalObjectReference, string, bool, ctrl.Result, error) {
	toSource := func(obj client.Object) translationSource {
		s := translationSource{Namespace: obj.GetNamespace(), Name: obj.GetName(), Generation: obj.GetGeneration()}
		switch v := obj.(type) {
		case *kdexv1alpha1.KDexTranslation:
			s.Kind = "KDexTranslation"
			s.Spec = v.Spec.KDexTranslationSpec
		case *kdexv1alpha1.KDexClusterTranslation:
			s.Kind = "KDexClusterTranslation"
			s.Spec = v.Spec
		}
		return s
	}
	resolve := func(ref *kdexv1alpha1.KDexObjectReference) (*translationSource, bool, ctrl.Result, error) {
		obj, shouldReturn, res, err := ResolveKDexObjectReference(ctx, r.Client, host, &host.Status.Conditions, ref, r.RequeueDelay)
		if shouldReturn || obj == nil {
			return nil, shouldReturn, res, err
		}
		s := toSource(obj)
		return &s, false, ctrl.Result{}, nil
	}

	defaultSrc, shouldReturn, res, err := resolve(&kdexv1alpha1.KDexObjectReference{
		Name: "kdex-default-translation",
		Kind: "KDexClusterTranslation",
	})
	if shouldReturn {
		return nil, "", true, res, err
	}

	attached := &kdexv1alpha1.KDexTranslationList{}
	if err := r.List(ctx, attached, client.InNamespace(host.Namespace), client.MatchingFields{hostIndexKey: host.Name}); err != nil {
		return nil, "", true, ctrl.Result{}, err
	}
	selfAttached := []translationSource{}
	for i := range attached.Items {
		t := &attached.Items[i]
		if !t.DeletionTimestamp.IsZero() {
			continue
		}
		s, shouldReturn, res, err := resolve(&kdexv1alpha1.KDexObjectReference{Kind: "KDexTranslation", Name: t.Name})
		if shouldReturn {
			return nil, "", true, res, err
		}
		if s != nil {
			selfAttached = append(selfAttached, *s)
		}
	}

	declared := []translationSource{}
	for i := range host.Spec.TranslationRefs {
		s, shouldReturn, res, err := resolve(&host.Spec.TranslationRefs[i])
		if shouldReturn {
			return nil, "", true, res, err
		}
		if s != nil {
			declared = append(declared, *s)
		}
	}

	ordered, collision := orderTranslationSources(defaultSrc, selfAttached, declared)

	refs := make([]corev1.LocalObjectReference, 0, len(ordered))
	keep := make(map[string]bool, len(ordered))
	for _, s := range ordered {
		internalTranslation, err := r.createOrUpdateInternalTranslation(ctx, s.Spec, s.Name, s.Generation, host)
		if err != nil {
			return nil, "", true, ctrl.Result{}, err
		}
		refs = append(refs, corev1.LocalObjectReference{Name: internalTranslation.Name})
		keep[internalTranslation.Name] = true

		if host.Status.Attributes == nil {
			host.Status.Attributes = make(map[string]string)
		}
		host.Status.Attributes[s.Name+translationGenerationSuffix] = fmt.Sprintf("%d", s.Generation)
	}

	r.pruneInternalTranslations(ctx, host, keep)

	return refs, collision, false, ctrl.Result{}, nil
}

//nolint:gocyclo
func (r *KDexHostReconciler) resolveUtilityPages(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
) (*corev1.LocalObjectReference, *corev1.LocalObjectReference, *corev1.LocalObjectReference, bool, ctrl.Result, error) {
	refs := map[kdexv1alpha1.KDexUtilityPageType]*corev1.LocalObjectReference{}

	types := []kdexv1alpha1.KDexUtilityPageType{
		kdexv1alpha1.AnnouncementUtilityPageType,
		kdexv1alpha1.ErrorUtilityPageType,
		kdexv1alpha1.LoginUtilityPageType,
	}

	for _, pageType := range types {
		var ref *kdexv1alpha1.KDexObjectReference
		if host.Spec.UtilityPages != nil {
			switch pageType {
			case kdexv1alpha1.AnnouncementUtilityPageType:
				ref = host.Spec.UtilityPages.AnnouncementRef
			case kdexv1alpha1.ErrorUtilityPageType:
				ref = host.Spec.UtilityPages.ErrorRef
			case kdexv1alpha1.LoginUtilityPageType:
				ref = host.Spec.UtilityPages.LoginRef
			}
		}

		resolvedObj, shouldReturn, r1, err := ResolveKDexObjectReference(ctx, r.Client, host, &host.Status.Conditions, ref, r.RequeueDelay)
		if shouldReturn && !isDefaultUtilityPage(ref) {
			return nil, nil, nil, true, r1, err
		}

		if resolvedObj != nil {
			var spec kdexv1alpha1.KDexUtilityPageSpec
			switch v := resolvedObj.(type) {
			case *kdexv1alpha1.KDexUtilityPage:
				spec = v.Spec
			case *kdexv1alpha1.KDexClusterUtilityPage:
				spec = v.Spec
			}

			// Validate Type matches
			if spec.Type != pageType {
				return nil, nil, nil, true, ctrl.Result{}, fmt.Errorf("utility page type %s does not match requested type %s", spec.Type, pageType)
			}

			internalRef, err := r.createOrUpdateInternalUtilityPage(ctx, host, spec, pageType, resolvedObj.GetGeneration())
			if err != nil {
				return nil, nil, nil, true, ctrl.Result{}, err
			}
			refs[pageType] = internalRef

			if host.Status.Attributes == nil {
				host.Status.Attributes = make(map[string]string)
			}
			host.Status.Attributes[strings.ToLower(string(pageType))+".utilitypage.generation"] = fmt.Sprintf("%d", resolvedObj.GetGeneration())
		}
	}

	return refs[kdexv1alpha1.AnnouncementUtilityPageType], refs[kdexv1alpha1.ErrorUtilityPageType], refs[kdexv1alpha1.LoginUtilityPageType], false, ctrl.Result{}, nil
}

func isDefaultUtilityPage(ref *kdexv1alpha1.KDexObjectReference) bool {
	return ref.Name == webhook.KDexDefaultUtilityPageAnnouncement ||
		ref.Name == webhook.KDexDefaultUtilityPageError ||
		ref.Name == webhook.KDexDefaultUtilityPageLogin
}
