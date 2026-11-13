package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// clusterReaderBindingName is the fixed ClusterRoleBinding shipped by the
// nexus chart and kustomize WITHOUT subjects: this operator owns the subject
// list at runtime, so a Helm upgrade must not reset it.
const clusterReaderBindingName = "kcnas-operator-host-manager-cluster-reader"

// ensureClusterReaderSubject adds the ServiceAccount to the fixed
// cluster-reader ClusterRoleBinding. Idempotent.
func (r *KDexHostReconciler) ensureClusterReaderSubject(ctx context.Context, namespace, sa string) error {
	subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: namespace}
	return r.updateClusterReaderSubjects(ctx, func(subjects []rbacv1.Subject) ([]rbacv1.Subject, bool) {
		if slices.Contains(subjects, subject) {
			return subjects, false
		}
		return append(subjects, subject), true
	})
}

// removeClusterReaderSubject removes the ServiceAccount from the fixed
// cluster-reader ClusterRoleBinding. Idempotent. A missing binding holds no
// subject, so there is nothing to remove: a teardown must not wedge on it.
func (r *KDexHostReconciler) removeClusterReaderSubject(ctx context.Context, namespace, sa string) error {
	subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: namespace}
	err := r.updateClusterReaderSubjects(ctx, func(subjects []rbacv1.Subject) ([]rbacv1.Subject, bool) {
		if !slices.Contains(subjects, subject) {
			return subjects, false
		}
		return slices.DeleteFunc(slices.Clone(subjects), func(s rbacv1.Subject) bool { return s == subject }), true
	})
	if errors.Is(err, errClusterReaderBindingMissing) {
		return nil
	}
	return err
}

// errClusterReaderBindingMissing is the cluster-reader binding not found.
var errClusterReaderBindingMissing = errors.New("ClusterRoleBinding " + clusterReaderBindingName + " not found: the nexus-manager chart that ships it is too old")

// updateClusterReaderSubjects is the optimistic-lock read-modify-update of
// the binding's subjects. The binding is owned by the nexus chart: when it is
// missing the chart is too old, and creating it here would split ownership.
// Read live through the API reader: the conflict-checked update needs the
// binding as it is now, which the name-filtered informer may lag.
func (r *KDexHostReconciler) updateClusterReaderSubjects(ctx context.Context, modify func([]rbacv1.Subject) ([]rbacv1.Subject, bool)) error {
	key := types.NamespacedName{Name: clusterReaderBindingName}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		binding := &rbacv1.ClusterRoleBinding{}
		if err := r.reader().Get(ctx, key, binding); err != nil {
			if apierrors.IsNotFound(err) {
				return errClusterReaderBindingMissing
			}
			return err
		}
		subjects, changed := modify(binding.Subjects)
		if !changed {
			return nil
		}
		binding.Subjects = subjects
		return r.Update(ctx, binding, client.FieldOwner(r.ControllerID))
	})
}

// recordedHostManagerServiceAccount is the ServiceAccount last granted
// cluster-reader access for the host ("" when none was). Teardown removes
// exactly this subject, never one recomputed from the spec.
func recordedHostManagerServiceAccount(host *kdexv1alpha1.KDexHost) string {
	return host.Status.Attributes[AttributeHostManagerServiceAccount]
}

// deploymentServiceAccount is the ServiceAccount d's pods run as: the
// namespace's "default" when the spec names none.
func deploymentServiceAccount(d *appsv1.Deployment) string {
	if sa := d.Spec.Template.Spec.ServiceAccountName; sa != "" {
		return sa
	}
	return "default"
}

// hostManagerServiceAccount is the ServiceAccount host-manager runs as, read
// from the live Deployment/<host> its chart renders (nexus forces the chart's
// fullname to the host name): whatever the values and the chart version made
// of it, that is the one that needs cluster reads. An unreadable Deployment
// is an error, never a guess.
func (r *KDexHostReconciler) hostManagerServiceAccount(ctx context.Context, host *kdexv1alpha1.KDexHost) (string, error) {
	key := client.ObjectKey{Namespace: host.Namespace, Name: host.Name}
	d := &appsv1.Deployment{}
	if err := r.reader().Get(ctx, key, d); err != nil {
		return "", fmt.Errorf("failed to read Deployment %s to grant its ServiceAccount cluster reads: %w", key, err)
	}
	return deploymentServiceAccount(d), nil
}

// grantHostManagerServiceAccount runs after a successful host-manager install:
// it adds the ServiceAccount the live Deployment runs as to the
// cluster-reader binding, removes the previously recorded ServiceAccount if
// the name changed, and only then records the new one. The record moves last
// so a failure part-way is redone by the next run.
func (r *KDexHostReconciler) grantHostManagerServiceAccount(ctx context.Context, host *kdexv1alpha1.KDexHost) error {
	sa, err := r.hostManagerServiceAccount(ctx, host)
	if err != nil {
		return err
	}
	if err := r.ensureClusterReaderSubject(ctx, host.Namespace, sa); err != nil {
		return err
	}
	key := client.ObjectKeyFromObject(host)
	var previous string
	_, err = r.patchHostStatus(ctx, key, func(h *kdexv1alpha1.KDexHost) bool {
		previous = recordedHostManagerServiceAccount(h)
		return false
	})
	if err != nil {
		return err
	}
	if previous == sa {
		return nil
	}
	if previous != "" {
		if err := r.releaseClusterReaderSubject(ctx, host, previous); err != nil {
			return err
		}
	}
	_, err = r.patchHostStatus(ctx, key, func(h *kdexv1alpha1.KDexHost) bool {
		h.Status.Attributes[AttributeHostManagerServiceAccount] = sa
		return true
	})
	return err
}

// releaseClusterReaderSubject removes ServiceAccount host.Namespace/sa --
// recorded for host, which no longer needs it (teardown, or a rename) -- from
// the cluster-reader binding, unless another KDexHost in the namespace that is
// not itself terminating has the same ServiceAccount recorded: one subject
// serves every host that runs as it, and removing it would strip that host's
// reads. The hosts are listed live, so one recorded since the cache last saw
// it keeps the subject.
func (r *KDexHostReconciler) releaseClusterReaderSubject(ctx context.Context, host *kdexv1alpha1.KDexHost, sa string) error {
	var hosts kdexv1alpha1.KDexHostList
	if err := r.reader().List(ctx, &hosts, client.InNamespace(host.Namespace)); err != nil {
		return fmt.Errorf("failed to list the namespace's KDexHosts: %w", err)
	}
	for i := range hosts.Items {
		h := &hosts.Items[i]
		if h.Name != host.Name && h.DeletionTimestamp.IsZero() && recordedHostManagerServiceAccount(h) == sa {
			return nil
		}
	}
	if err := r.removeClusterReaderSubject(ctx, host.Namespace, sa); err != nil {
		return fmt.Errorf("failed to remove the cluster-reader subject %s/%s: %w", host.Namespace, sa, err)
	}
	return nil
}

// healClusterReaderSubject re-adds the recorded host-manager ServiceAccount to
// the cluster-reader binding on every reconcile, outside the release-hash gate
// and regardless of the run's status: an installed host-manager keeps running
// while its run is Failed or InProgress, and must not lose its reads if the
// binding is reset. It reads the binding from the cache (field-selected to
// this one binding) and goes live -- the optimistic-lock read-modify-update --
// only when the subject is missing, so a settled fleet costs no API call per
// reconcile. A failure is logged by the caller, not fatal: the next reconcile
// retries it.
func (r *KDexHostReconciler) healClusterReaderSubject(ctx context.Context, host *kdexv1alpha1.KDexHost) error {
	sa := recordedHostManagerServiceAccount(host)
	if sa == "" {
		return nil
	}
	cached := &rbacv1.ClusterRoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterReaderBindingName}, cached); err == nil &&
		slices.Contains(cached.Subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: host.Namespace}) {
		return nil
	}
	return r.ensureClusterReaderSubject(ctx, host.Namespace, sa)
}

// clusterReaderBindingRequests maps an event on the cluster-reader binding to
// every KDexHost whose recorded ServiceAccount it no longer holds, so a
// subjects list reset out of band heals at once rather than at each host's
// next unrelated reconcile. A host whose subject is there is not woken: each
// grant writes the binding, and waking every host on each would make a fleet
// rollout quadratic.
func (r *KDexHostReconciler) clusterReaderBindingRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	binding, ok := obj.(*rbacv1.ClusterRoleBinding)
	if !ok || binding.Name != clusterReaderBindingName {
		return nil
	}
	var hosts kdexv1alpha1.KDexHostList
	if err := r.List(ctx, &hosts); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list KDexHosts for the cluster-reader binding")
		return nil
	}
	var requests []reconcile.Request
	for i := range hosts.Items {
		h := &hosts.Items[i]
		sa := recordedHostManagerServiceAccount(h)
		if sa == "" || slices.Contains(binding.Subjects, rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: h.Namespace}) {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(h)})
	}
	return requests
}
