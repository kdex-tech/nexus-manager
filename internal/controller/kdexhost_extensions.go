package controller

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// maxHostExtensions mirrors KDexInternalHost.spec.extensions maxItems.
const maxHostExtensions = 32

const extensionGenerationSuffix = ".extension.generation"

// indexExtensionByHostRef indexes a KDexHostExtension by the host it names.
func indexExtensionByHostRef(obj client.Object) []string {
	e, ok := obj.(*kdexv1alpha1.KDexHostExtension)
	if !ok || e.Spec.HostRef.Name == "" {
		return nil
	}
	return []string{e.Spec.HostRef.Name}
}

// extensionHostRefRequests enqueues the host an extension names. On update the
// old and new objects are both mapped, so a moved hostRef or a relabel also
// re-reconciles the previous host, which then drops the extension.
func extensionHostRefRequests(_ context.Context, obj client.Object) []reconcile.Request {
	e, ok := obj.(*kdexv1alpha1.KDexHostExtension)
	if !ok || e.Spec.HostRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: e.Namespace, Name: e.Spec.HostRef.Name}}}
}

// selectExtensions returns, in application order (weight ascending, then
// name), the candidates host accepts: they name host, are not being deleted,
// and match host.spec.extensionSelector. At most maxHostExtensions are
// applied; the rest are returned as overflow. A nil selector accepts none
// (extensions grant authority, so consent is explicit); an unparseable one
// accepts none and returns the error.
func selectExtensions(host *kdexv1alpha1.KDexHost, candidates []kdexv1alpha1.KDexHostExtension) (applied, overflow []kdexv1alpha1.KDexHostExtension, err error) {
	if host.Spec.ExtensionSelector == nil {
		return nil, nil, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(host.Spec.ExtensionSelector)
	if err != nil {
		return nil, nil, err
	}
	matched := []kdexv1alpha1.KDexHostExtension{}
	for _, e := range candidates {
		if e.Spec.HostRef.Name != host.Name || !e.DeletionTimestamp.IsZero() || !sel.Matches(labels.Set(e.Labels)) {
			continue
		}
		matched = append(matched, e)
	}
	slices.SortFunc(matched, func(a, b kdexv1alpha1.KDexHostExtension) int {
		if c := cmp.Compare(a.Spec.Weight, b.Spec.Weight); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(matched) > maxHostExtensions {
		return matched[:maxHostExtensions], matched[maxHostExtensions:], nil
	}
	return matched, nil, nil
}

// resolveExtensions lists the extensions naming host, selects the applied set,
// records each applied extension's generation in host status, and returns them
// as the internal host's spec.extensions — nil when none, so an unchanged host
// does not churn its internal host (nexus issue #19).
func (r *KDexHostReconciler) resolveExtensions(ctx context.Context, host *kdexv1alpha1.KDexHost) ([]kdexv1alpha1.InternalHostExtension, error) {
	list := &kdexv1alpha1.KDexHostExtensionList{}
	if err := r.List(ctx, list, client.InNamespace(host.Namespace), client.MatchingFields{hostIndexKey: host.Name}); err != nil {
		return nil, err
	}
	applied, overflow, err := selectExtensions(host, list.Items)
	if err != nil {
		return nil, fmt.Errorf("spec.extensionSelector: %w", err)
	}
	if len(overflow) > 0 {
		logf.FromContext(ctx).Info("host selects more extensions than it can apply; ignoring the rest",
			"limit", maxHostExtensions, "ignored", len(overflow))
	}

	for attr := range host.Status.Attributes {
		if strings.HasSuffix(attr, extensionGenerationSuffix) {
			delete(host.Status.Attributes, attr)
		}
	}
	if len(applied) == 0 {
		return nil, nil
	}
	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}
	out := make([]kdexv1alpha1.InternalHostExtension, 0, len(applied))
	for _, e := range applied {
		host.Status.Attributes[e.Name+extensionGenerationSuffix] = fmt.Sprintf("%d", e.Generation)
		out = append(out, kdexv1alpha1.InternalHostExtension{
			Name:                  e.Name,
			Generation:            e.Generation,
			Weight:                e.Spec.Weight,
			ClaimMappings:         e.Spec.ClaimMappings,
			AnonymousEntitlements: e.Spec.AnonymousEntitlements,
		})
	}
	return out, nil
}
