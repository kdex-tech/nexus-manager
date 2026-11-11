package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"helm.sh/helm/v4/pkg/release/common"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// Migration from the cluster-scoped host-manager RBAC (make-before-break).
//
// Before the privilege redesign every host-manager release rendered a
// ClusterRoleBinding/<host>-<namespace>, and -- unless the operator pointed it
// at a shared role (HOST_MANAGER_CLUSTER_ROLE) -- a ClusterRole/<host>. The
// redesigned chart renders only a namespaced Role and RoleBinding/<host>.
// Upgrading as the namespace's installer cannot remove the legacy objects:
// Helm skips a dropped resource it is Forbidden to Get. So nexus deletes them
// itself, once the upgrade has landed and rolled out -- or at teardown, before
// the uninstall, which then drops them from the release record
// (HelmClientInterface.DropClusterRBACFromRecord) and runs as the installer
// like every other Helm action.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
const (
	// ConditionLegacyRBACPresent is True on a host while its legacy
	// cluster-scoped RBAC remains, and absent once it is gone.
	ConditionLegacyRBACPresent = "LegacyRBACPresent"

	reasonLegacyRBACAwaitingUpgrade = "AwaitingHostManagerUpgrade"
	reasonLegacyRBACCleanupFailed   = "CleanupFailed"
	reasonLegacyRBACReadFailed      = "LegacyRBACReadFailed"
	reasonLegacyRBACProofReadFailed = "UpgradeProofReadFailed"

	helmReleaseNameAnnotation      = "meta.helm.sh/release-name"
	helmReleaseNamespaceAnnotation = "meta.helm.sh/release-namespace"
)

// helmReleasedBy reports whether Helm's ownership annotations on obj name the
// host's release: the release name alone is not enough, since same-named hosts
// in two namespaces render same-named cluster-scoped objects.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func helmReleasedBy(obj client.Object, host *kdexv1alpha1.KDexHost) bool {
	a := obj.GetAnnotations()
	return a[helmReleaseNameAnnotation] == host.Name && a[helmReleaseNamespaceAnnotation] == host.Namespace
}

// legacyBindingName is the name the legacy chart gave the host's binding.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func legacyBindingName(host *kdexv1alpha1.KDexHost) string {
	return host.Name + "-" + host.Namespace
}

// isLegacyRole reports whether role is the host's legacy ClusterRole: named
// after the host and Helm-owned by its release. A shared role -- one the
// legacy binding was pointed at -- is not annotated for the release, and is
// never this release's to delete.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func isLegacyRole(role *rbacv1.ClusterRole, host *kdexv1alpha1.KDexHost) bool {
	return role.Name == host.Name && helmReleasedBy(role, host)
}

// isLegacyBinding reports whether binding is the host's legacy
// ClusterRoleBinding: its name alone proves nothing -- host rsi in a-b and
// host rsi-a in b both yield rsi-a-b -- so it must be Helm-owned by the
// release, and bind a ClusterRole. Any ClusterRole: the legacy chart bound a
// shared one when the operator set HOST_MANAGER_CLUSTER_ROLE.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func isLegacyBinding(binding *rbacv1.ClusterRoleBinding, host *kdexv1alpha1.KDexHost) bool {
	return binding.Name == legacyBindingName(host) && helmReleasedBy(binding, host) && binding.RoleRef.Kind == kindClusterRole
}

// legacyRBAC returns the host's legacy ClusterRole and ClusterRoleBinding, nil
// for each not found or not the host's (isLegacyRole, isLegacyBinding). Read
// live: ClusterRoles are not cached, and the ClusterRoleBinding cache holds
// only the cluster-reader binding.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) legacyRBAC(ctx context.Context, host *kdexv1alpha1.KDexHost) (role *rbacv1.ClusterRole, binding *rbacv1.ClusterRoleBinding, err error) {
	role = &rbacv1.ClusterRole{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: host.Name}, role); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to read legacy ClusterRole %q: %w", host.Name, err)
		}
		role = nil
	} else if !isLegacyRole(role, host) {
		role = nil
	}

	binding = &rbacv1.ClusterRoleBinding{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: legacyBindingName(host)}, binding); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to read legacy ClusterRoleBinding %q: %w", legacyBindingName(host), err)
		}
		binding = nil
	} else if !isLegacyBinding(binding, host) {
		binding = nil
	}
	return role, binding, nil
}

// legacyRBACNames names what legacyRBAC found, for the condition's message.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func legacyRBACNames(role *rbacv1.ClusterRole, binding *rbacv1.ClusterRoleBinding) string {
	var names []string
	if role != nil {
		names = append(names, "ClusterRole/"+role.Name)
	}
	if binding != nil {
		names = append(names, "ClusterRoleBinding/"+binding.Name)
	}
	return strings.Join(names, ", ")
}

// deleteLegacyRBAC deletes what legacyRBAC found, each pinned to the UID it was
// matched by: one deleted and re-created since is another object, which the
// API server refuses to delete (a Conflict, so the next pass matches afresh).
// One already gone is no error.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) deleteLegacyRBAC(ctx context.Context, role *rbacv1.ClusterRole, binding *rbacv1.ClusterRoleBinding) error {
	var objs []client.Object
	if role != nil {
		objs = append(objs, role)
	}
	if binding != nil {
		objs = append(objs, binding)
	}
	var errs []error
	for _, obj := range objs {
		uid := obj.GetUID()
		if err := r.Delete(ctx, obj, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("failed to delete legacy %T %q: %w", obj, obj.GetName(), err))
		}
	}
	return errors.Join(errs...)
}

// setLegacyRBACCondition sets ConditionLegacyRBACPresent on the in-memory
// host; Reconcile's status patch writes it.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func setLegacyRBACCondition(host *kdexv1alpha1.KDexHost, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&host.Status.Conditions, metav1.Condition{
		Type:               ConditionLegacyRBACPresent,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: host.Generation,
	})
}

// cleanupLegacyRBAC deletes the host's legacy cluster-scoped RBAC once the
// redesigned chart has replaced it (hostManagerUpgraded): until then deleting
// would leave host-manager without a grant. ConditionLegacyRBACPresent says
// what remains, and is removed once nothing does. It runs on every
// non-deleting pass, outside the release-hash gate: a release already
// upgraded starts no helm run.
//
// pending reports that legacy RBAC may remain after this pass -- found and
// not yet deleted, or not readable -- so the caller looks again
// (requeueForLegacyRBAC): nothing watches host-manager's Deployment, and
// Available stays True through its rollout, so no event follows the moment
// the proof is met.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) cleanupLegacyRBAC(ctx context.Context, host *kdexv1alpha1.KDexHost, secrets kdexv1alpha1.Secrets) (pending bool, err error) {
	role, binding, err := r.legacyRBAC(ctx, host)
	if err != nil {
		setLegacyRBACCondition(host, metav1.ConditionUnknown, reasonLegacyRBACReadFailed, err.Error())
		return true, err
	}
	if role == nil && binding == nil {
		meta.RemoveStatusCondition(&host.Status.Conditions, ConditionLegacyRBACPresent)
		return false, nil
	}
	names := legacyRBACNames(role, binding)

	upgraded, err := r.hostManagerUpgraded(ctx, host, secrets)
	if err != nil {
		setLegacyRBACCondition(host, metav1.ConditionTrue, reasonLegacyRBACProofReadFailed,
			fmt.Sprintf("legacy %s remain: %v", names, err))
		return true, err
	}
	if !upgraded {
		setLegacyRBACCondition(host, metav1.ConditionTrue, reasonLegacyRBACAwaitingUpgrade,
			fmt.Sprintf("legacy %s remain; they are deleted once the host-manager chart's RoleBinding/%s exists in %s, Deployment/%s has rolled out, release %s is deployed, and the ServiceAccount the Deployment runs as is a subject of ClusterRoleBinding/%s",
				names, host.Name, host.Namespace, host.Name, host.Name, clusterReaderBindingName))
		return true, nil
	}

	if err := r.deleteLegacyRBAC(ctx, role, binding); err != nil {
		setLegacyRBACCondition(host, metav1.ConditionTrue, reasonLegacyRBACCleanupFailed,
			fmt.Sprintf("legacy %s remain: %v", names, err))
		return true, err
	}
	meta.RemoveStatusCondition(&host.Status.Conditions, ConditionLegacyRBACPresent)
	return false, nil
}

// requeueForLegacyRBAC joins the legacy cleanup's requeue after into res, the
// result the rest of Reconcile returns with err: the sooner non-zero requeue
// wins. A result returned with an error is ignored -- the error's backoff
// requeues -- so it is left as it is.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func requeueForLegacyRBAC(res ctrl.Result, err error, after time.Duration) ctrl.Result {
	if err != nil || after <= 0 {
		return res
	}
	if res.RequeueAfter == 0 || after < res.RequeueAfter {
		res.RequeueAfter = after
	}
	return res
}

// hostManagerUpgraded reports whether the redesigned host-manager has replaced
// the legacy grant, everything read live:
//   - the chart's RoleBinding/<host>, Helm-owned by the host's release, exists
//     in the host's namespace (the RoleBinding cache holds only the
//     operator's own);
//   - Deployment/<host> has fully rolled out, so no pod still running on the
//     legacy grant is left (the cache can show a finished rollout while the
//     next one has begun);
//   - the release's latest revision is deployed -- read through the
//     installer's Helm storage -- not failed after applying the RoleBinding
//     and Deployment, nor left pending by a nexus killed mid-upgrade, which
//     fails every later upgrade and so the grant after it;
//   - the cluster-reader binding holds the ServiceAccount the Deployment runs
//     as: before the migration the legacy binding is host-manager's only read
//     of cluster-scoped objects, and the grant after an install can fail.
//
// Migration code: delete in release B (privilege-redesign follow-up issue).
func (r *KDexHostReconciler) hostManagerUpgraded(ctx context.Context, host *kdexv1alpha1.KDexHost, secrets kdexv1alpha1.Secrets) (bool, error) {
	key := client.ObjectKey{Namespace: host.Namespace, Name: host.Name}
	rb := &rbacv1.RoleBinding{}
	if err := r.reader().Get(ctx, key, rb); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read RoleBinding %s: %w", key, err)
	}
	if !helmReleasedBy(rb, host) {
		return false, nil
	}

	deployment := &appsv1.Deployment{}
	if err := r.reader().Get(ctx, key, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read Deployment %s: %w", key, err)
	}
	if !deploymentRolledOut(deployment) {
		return false, nil
	}

	if r.HelmClientFactory == nil {
		return false, errors.New("HelmClientFactory is nil")
	}
	helm, err := r.getOrCreateHelmClient(host.Name, host.Namespace, secrets, logf.FromContext(ctx).WithName("helm"))
	if err != nil {
		return false, err
	}
	status, err := helm.ReleaseStatus(host.Name)
	if err != nil {
		return false, err
	}
	if status != string(common.StatusDeployed) {
		return false, nil
	}

	return r.clusterReaderGranted(ctx, host.Namespace, deploymentServiceAccount(deployment))
}

// deploymentRolledOut reports whether d's controller has seen its latest spec
// and every replica runs it -- no old one left terminating -- with at least
// one available.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func deploymentRolledOut(d *appsv1.Deployment) bool {
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	return d.Status.ObservedGeneration >= d.Generation &&
		d.Status.UpdatedReplicas == replicas &&
		d.Status.Replicas == d.Status.UpdatedReplicas &&
		d.Status.AvailableReplicas >= 1
}
