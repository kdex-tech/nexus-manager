package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// translationSource is one translation resolved for a host, before it is copied
// into a KDexInternalTranslation named "<host>-<Name>".
type translationSource struct {
	Kind       string
	Namespace  string // empty for KDexClusterTranslation
	Name       string
	Generation int64
	Spec       kdexv1alpha1.KDexTranslationSpec
}

func (s translationSource) key() string {
	return s.Kind + "/" + s.Namespace + "/" + s.Name
}

// orderTranslationSources returns a host's translations from lowest to highest
// precedence: the default, then self-attached translations (by name), then the
// host's translationRefs (in list order). host-manager writes its catalog in
// this order and the catalog is last-write-wins, so the host-declared value
// beats anything a self-attached translation ships.
//
// A translation that is both self-attached and host-declared keeps only its
// host-declared position. Two distinct sources with the same Name would share
// one KDexInternalTranslation; the higher-precedence one is kept and collision
// describes each conflict (empty when there is none).
func orderTranslationSources(
	defaultSrc *translationSource,
	selfAttached, declared []translationSource,
) ([]translationSource, string) {
	declaredKeys := make(map[string]bool, len(declared))
	for _, s := range declared {
		declaredKeys[s.key()] = true
	}

	self := slices.Clone(selfAttached)
	slices.SortFunc(self, func(a, b translationSource) int { return strings.Compare(a.Name, b.Name) })

	candidates := []translationSource{}
	if defaultSrc != nil {
		candidates = append(candidates, *defaultSrc)
	}
	for _, s := range self {
		if !declaredKeys[s.key()] {
			candidates = append(candidates, s)
		}
	}
	candidates = append(candidates, declared...)

	// Walk from highest precedence down so the first claimant of a name wins.
	winner := map[string]translationSource{}
	keep := make([]bool, len(candidates))
	conflicts := []string{}
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		prev, taken := winner[c.Name]
		if !taken {
			winner[c.Name] = c
			keep[i] = true
			continue
		}
		if prev.key() != c.key() {
			conflicts = append(conflicts, fmt.Sprintf("%s and %s both map to internal translation suffix %q; using %s",
				c.key(), prev.key(), c.Name, prev.key()))
		}
	}

	ordered := make([]translationSource, 0, len(candidates))
	for i, c := range candidates {
		if keep[i] {
			ordered = append(ordered, c)
		}
	}
	slices.Sort(conflicts)
	return ordered, strings.Join(conflicts, "; ")
}

// indexTranslationByHostRef indexes a KDexTranslation by the host it attaches
// itself to through spec.hostRef.
func indexTranslationByHostRef(obj client.Object) []string {
	t, ok := obj.(*kdexv1alpha1.KDexTranslation)
	if !ok || t.Spec.HostRef == nil || t.Spec.HostRef.Name == "" {
		return nil
	}
	return []string{t.Spec.HostRef.Name}
}

// indexInternalTranslationByHost indexes a KDexInternalTranslation by the host
// it was produced for.
func indexInternalTranslationByHost(obj client.Object) []string {
	t, ok := obj.(*kdexv1alpha1.KDexInternalTranslation)
	if !ok || t.Spec.HostRef.Name == "" {
		return nil
	}
	return []string{t.Spec.HostRef.Name}
}

const translationGenerationSuffix = ".translation.generation"

// pruneInternalTranslations deletes the KDexInternalTranslations this host
// controls whose names are not in keep, and drops their generation attributes
// from the host status. host-manager serves every internal translation that
// names its host, so a copy left behind keeps serving strings whose source is
// gone. Failures are logged and retried on the next reconcile; they never fail
// this one.
func (r *KDexHostReconciler) pruneInternalTranslations(ctx context.Context, host *kdexv1alpha1.KDexHost, keep map[string]bool) {
	log := logf.FromContext(ctx).WithName("translation")

	existing := &kdexv1alpha1.KDexInternalTranslationList{}
	if err := r.List(ctx, existing, client.InNamespace(host.Namespace), client.MatchingFields{hostIndexKey: host.Name}); err != nil {
		log.Error(err, "listing internal translations to prune")
		return
	}
	for i := range existing.Items {
		it := &existing.Items[i]
		if keep[it.Name] || !it.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(it, host) {
			continue
		}
		if err := r.Delete(ctx, it); client.IgnoreNotFound(err) != nil {
			log.Error(err, "pruning internal translation", "name", it.Name)
			continue
		}
		log.V(1).Info("pruned internal translation", "name", it.Name)
	}

	for attr := range host.Status.Attributes {
		if source, ok := strings.CutSuffix(attr, translationGenerationSuffix); ok && !keep[host.Name+"-"+source] {
			delete(host.Status.Attributes, attr)
		}
	}
}

// setTranslationCollisionCondition marks host Degraded because two distinct
// translations map to one KDexInternalTranslation name.
func setTranslationCollisionCondition(host *kdexv1alpha1.KDexHost, collision string) {
	kdexv1alpha1.SetConditions(
		&host.Status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Degraded:    metav1.ConditionTrue,
			Progressing: metav1.ConditionFalse,
			Ready:       metav1.ConditionFalse,
		},
		kdexv1alpha1.ConditionReasonReconcileError,
		"translation name collision: "+collision,
	)
}

// translationHostRefRequests enqueues the KDexHost a KDexTranslation attaches
// itself to through spec.hostRef. EnqueueRequestsFromMapFunc maps both the old
// and the new object on update, so a moved or removed hostRef also
// re-reconciles the previous host, which then prunes its copy.
func translationHostRefRequests(_ context.Context, obj client.Object) []reconcile.Request {
	t, ok := obj.(*kdexv1alpha1.KDexTranslation)
	if !ok || t.Spec.HostRef == nil || t.Spec.HostRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: t.Namespace, Name: t.Spec.HostRef.Name}}}
}
