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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
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
	name := internalUtilityPageName(host, pageType)
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

	// consent.* is accepted only from the default and the host's own
	// translationRefs. A translation that is also host-declared keeps its
	// declared position (and its keys) in orderTranslationSources.
	declaredKeys := make(map[string]bool, len(declared))
	for _, s := range declared {
		declaredKeys[s.key()] = true
	}
	for i := range selfAttached {
		if declaredKeys[selfAttached[i].key()] {
			continue
		}
		spec, dropped := withoutConsentKeys(selfAttached[i].Spec)
		if len(dropped) > 0 {
			selfAttached[i].Spec = spec
			logf.FromContext(ctx).WithName("translation").V(1).Info(
				"ignoring consent.* keys in a self-attached translation; set them through the host's translationRefs",
				"translation", selfAttached[i].key(), "keys", dropped)
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

// utilityPageTypes is the single list of utility page types a host resolves.
var utilityPageTypes = []kdexv1alpha1.KDexUtilityPageType{
	kdexv1alpha1.AnnouncementUtilityPageType,
	kdexv1alpha1.ConsentUtilityPageType,
	kdexv1alpha1.ErrorUtilityPageType,
	kdexv1alpha1.LoginUtilityPageType,
}

// defaultConsentRef is the Consent page a host uses when its stored spec has
// no consentRef: hosts created before the Consent type existed, or while the
// fail-open defaulter was unavailable, still get the default branded page.
func defaultConsentRef() *kdexv1alpha1.KDexObjectReference {
	return &kdexv1alpha1.KDexObjectReference{
		Kind: webhook.KDexClusterUtilityPage,
		Name: webhook.KDexDefaultUtilityPageConsent,
	}
}

// storedUtilityPageRef returns the host's reference for the given utility page
// type exactly as stored (nil when unset).
func storedUtilityPageRef(host *kdexv1alpha1.KDexHost, pageType kdexv1alpha1.KDexUtilityPageType) *kdexv1alpha1.KDexObjectReference {
	if host.Spec.UtilityPages == nil {
		return nil
	}
	switch pageType {
	case kdexv1alpha1.AnnouncementUtilityPageType:
		return host.Spec.UtilityPages.AnnouncementRef
	case kdexv1alpha1.ConsentUtilityPageType:
		return host.Spec.UtilityPages.ConsentRef
	case kdexv1alpha1.ErrorUtilityPageType:
		return host.Spec.UtilityPages.ErrorRef
	case kdexv1alpha1.LoginUtilityPageType:
		return host.Spec.UtilityPages.LoginRef
	}
	return nil
}

// utilityPageRef returns the reference the host resolves for the given utility
// page type: the stored one, or for Consent the default when none is stored.
func utilityPageRef(host *kdexv1alpha1.KDexHost, pageType kdexv1alpha1.KDexUtilityPageType) *kdexv1alpha1.KDexObjectReference {
	ref := storedUtilityPageRef(host, pageType)
	if ref == nil && pageType == kdexv1alpha1.ConsentUtilityPageType {
		return defaultConsentRef()
	}
	return ref
}

// unresolvedUtilityPage describes a utility page reference that did not
// resolve but must not stop the reconcile.
type unresolvedUtilityPage struct {
	message string
	requeue ctrl.Result
}

// resolveUtilityPages writes one KDexInternalUtilityPage per resolved utility
// page and returns their references by type.
//
// A Consent reference that does not resolve (missing, not Ready, wrong type)
// never stops the reconcile: host-manager serves its built-in consent page
// when the internal host has no consentRef, so the host proceeds, any stale
// <host>-consent is deleted, and consent describes the problem (nil when there
// is none, or when only the default page is absent). The other types keep
// their behaviour: a non-default reference that does not resolve returns.
func (r *KDexHostReconciler) resolveUtilityPages(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
) (refs map[kdexv1alpha1.KDexUtilityPageType]*corev1.LocalObjectReference, consent *unresolvedUtilityPage, shouldReturn bool, res ctrl.Result, err error) {
	refs = map[kdexv1alpha1.KDexUtilityPageType]*corev1.LocalObjectReference{}

	for _, pageType := range utilityPageTypes {
		ref := utilityPageRef(host, pageType)

		internalRef, unresolved, r1, resolveErr, writeErr := r.resolveUtilityPage(ctx, host, pageType, ref)
		if writeErr != nil {
			return nil, nil, true, ctrl.Result{}, writeErr
		}
		if unresolved {
			if pageType != kdexv1alpha1.ConsentUtilityPageType {
				return nil, nil, true, r1, resolveErr
			}
			message := fmt.Sprintf("consentRef %s/%s did not resolve; serving the built-in consent page", ref.Kind, ref.Name)
			if resolveErr != nil {
				message += ": " + resolveErr.Error()
			}
			consent = &unresolvedUtilityPage{message: message, requeue: r1}
		}

		if internalRef != nil {
			refs[pageType] = internalRef
			continue
		}
		if pageType == kdexv1alpha1.ConsentUtilityPageType {
			if err := r.deleteInternalUtilityPage(ctx, host, pageType); err != nil {
				return nil, nil, true, ctrl.Result{}, err
			}
		}
	}

	return refs, consent, false, ctrl.Result{}, nil
}

// resolveUtilityPage resolves ref and writes its KDexInternalUtilityPage.
// unresolved is true when a non-default ref names a page that cannot be used
// (missing, not Ready, or of another type); r1 and resolveErr then say why. An
// absent default page is not unresolved: it yields no internal page. writeErr
// is a failure to write the internal page.
func (r *KDexHostReconciler) resolveUtilityPage(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
	pageType kdexv1alpha1.KDexUtilityPageType,
	ref *kdexv1alpha1.KDexObjectReference,
) (internalRef *corev1.LocalObjectReference, unresolved bool, r1 ctrl.Result, resolveErr, writeErr error) {
	resolvedObj, shouldReturn, r1, resolveErr := ResolveKDexObjectReference(ctx, r.Client, host, &host.Status.Conditions, ref, r.RequeueDelay)
	if shouldReturn && !isDefaultUtilityPage(ref) {
		return nil, true, r1, resolveErr, nil
	}
	if resolvedObj == nil {
		return nil, false, ctrl.Result{}, nil, nil
	}

	var spec kdexv1alpha1.KDexUtilityPageSpec
	switch v := resolvedObj.(type) {
	case *kdexv1alpha1.KDexUtilityPage:
		spec = v.Spec
	case *kdexv1alpha1.KDexClusterUtilityPage:
		spec = v.Spec
	}

	if spec.Type != pageType {
		return nil, true, ctrl.Result{}, fmt.Errorf("utility page type %s does not match requested type %s", spec.Type, pageType), nil
	}

	internalRef, writeErr = r.createOrUpdateInternalUtilityPage(ctx, host, spec, pageType, resolvedObj.GetGeneration())
	if writeErr != nil {
		return nil, false, ctrl.Result{}, nil, writeErr
	}

	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}
	host.Status.Attributes[utilityPageGenerationAttribute(pageType)] = fmt.Sprintf("%d", resolvedObj.GetGeneration())

	return internalRef, false, ctrl.Result{}, nil, nil
}

func utilityPageGenerationAttribute(pageType kdexv1alpha1.KDexUtilityPageType) string {
	return strings.ToLower(string(pageType)) + ".utilitypage.generation"
}

func internalUtilityPageName(host *kdexv1alpha1.KDexHost, pageType kdexv1alpha1.KDexUtilityPageType) string {
	return fmt.Sprintf("%s-%s", host.Name, strings.ToLower(string(pageType)))
}

// deleteInternalUtilityPage deletes the host's KDexInternalUtilityPage of the
// given type, if the host controls one, and drops its generation attribute.
// host-manager indexes utility pages by type, so a page left behind would keep
// being served after its reference stopped resolving.
func (r *KDexHostReconciler) deleteInternalUtilityPage(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
	pageType kdexv1alpha1.KDexUtilityPageType,
) error {
	delete(host.Status.Attributes, utilityPageGenerationAttribute(pageType))

	existing := &kdexv1alpha1.KDexInternalUtilityPage{}
	err := r.Get(ctx, client.ObjectKey{Namespace: host.Namespace, Name: internalUtilityPageName(host, pageType)}, existing)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if !existing.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(existing, host) {
		return nil
	}
	if err := r.Delete(ctx, existing); client.IgnoreNotFound(err) != nil {
		return err
	}
	logf.FromContext(ctx).WithName("utilitypage").V(1).Info("deleted internal utility page whose reference no longer resolves",
		"name", existing.Name, "type", pageType)
	return nil
}

// defaultConsentPageRequests enqueues the hosts that fall back to the default
// Consent page (no stored consentRef) when that page changes. The reference
// path handler cannot find them: they name no page.
func (r *KDexHostReconciler) defaultConsentPageRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetName() != webhook.KDexDefaultUtilityPageConsent {
		return nil
	}
	hosts := &kdexv1alpha1.KDexHostList{}
	if err := r.List(ctx, hosts); err != nil {
		logf.FromContext(ctx).Error(err, "listing hosts for the default consent page")
		return nil
	}
	requests := []reconcile.Request{}
	for i := range hosts.Items {
		h := &hosts.Items[i]
		if storedUtilityPageRef(h, kdexv1alpha1.ConsentUtilityPageType) == nil {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(h)})
		}
	}
	return requests
}

func isDefaultUtilityPage(ref *kdexv1alpha1.KDexObjectReference) bool {
	return ref.Name == webhook.KDexDefaultUtilityPageAnnouncement ||
		ref.Name == webhook.KDexDefaultUtilityPageConsent ||
		ref.Name == webhook.KDexDefaultUtilityPageError ||
		ref.Name == webhook.KDexDefaultUtilityPageLogin
}
