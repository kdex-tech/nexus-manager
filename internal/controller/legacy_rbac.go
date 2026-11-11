package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Migration from the cluster-scoped host-manager RBAC (make-before-break).
//
// Before the privilege redesign every host-manager release rendered a
// ClusterRole/<host> and a ClusterRoleBinding/<host>-<namespace>. The
// redesigned chart renders only a namespaced Role and RoleBinding/<host>.
// Upgrading as the namespace's installer cannot remove the legacy objects:
// Helm skips a dropped resource it is Forbidden to Get. So nexus deletes them
// itself, once the new RoleBinding proves the upgrade landed, and uninstalls
// a release whose record may still list them with its own identity.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
const (
	// ConditionLegacyRBACPresent is True on a host while its legacy
	// cluster-scoped RBAC remains, and absent once both objects are gone.
	ConditionLegacyRBACPresent = "LegacyRBACPresent"

	reasonLegacyRBACAwaitingUpgrade = "AwaitingHostManagerUpgrade"
	reasonLegacyRBACCleanupFailed   = "CleanupFailed"
	reasonLegacyRBACCheckFailed     = "CheckFailed"

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

// legacyRBAC returns the host's legacy ClusterRole and ClusterRoleBinding, nil
// for each not found or not the host's: the role must be Helm-owned by the
// host's release; the binding too, and bind that role. Its name alone proves
// nothing -- host rsi in a-b and host rsi-a in b both yield rsi-a-b. Read live:
// ClusterRoles are not cached, and the ClusterRoleBinding cache holds only the
// cluster-reader binding.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) legacyRBAC(ctx context.Context, host *kdexv1alpha1.KDexHost) (role *rbacv1.ClusterRole, binding *rbacv1.ClusterRoleBinding, err error) {
	role = &rbacv1.ClusterRole{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: host.Name}, role); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to read legacy ClusterRole %q: %w", host.Name, err)
		}
		role = nil
	} else if !helmReleasedBy(role, host) {
		role = nil
	}

	bindingName := host.Name + "-" + host.Namespace
	binding = &rbacv1.ClusterRoleBinding{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: bindingName}, binding); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("failed to read legacy ClusterRoleBinding %q: %w", bindingName, err)
		}
		binding = nil
	} else if !helmReleasedBy(binding, host) || binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != host.Name {
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

// legacyUninstallIdentity is who uninstalls the host-manager release: the
// operator while legacy RBAC remains, since the release record may still list
// it and the installer, Forbidden to Get a ClusterRole, would fail the
// uninstall; otherwise the installer.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func legacyUninstallIdentity(role *rbacv1.ClusterRole, binding *rbacv1.ClusterRoleBinding) utils.HelmIdentity {
	if role != nil || binding != nil {
		return utils.HelmAsOperator
	}
	return utils.HelmAsInstaller
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
// redesigned chart's RoleBinding/<host> exists in the host's namespace -- it
// is what replaces the legacy grant, so until then deleting would leave
// host-manager without one. ConditionLegacyRBACPresent says what remains, and
// is removed once nothing does. It runs on every non-deleting pass, outside
// the release-hash gate: a release already upgraded starts no helm run.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) cleanupLegacyRBAC(ctx context.Context, host *kdexv1alpha1.KDexHost) error {
	role, binding, err := r.legacyRBAC(ctx, host)
	if err != nil {
		setLegacyRBACCondition(host, metav1.ConditionUnknown, reasonLegacyRBACCheckFailed, err.Error())
		return err
	}
	if role == nil && binding == nil {
		meta.RemoveStatusCondition(&host.Status.Conditions, ConditionLegacyRBACPresent)
		return nil
	}
	names := legacyRBACNames(role, binding)

	upgraded, err := r.hostManagerRoleBindingExists(ctx, host)
	if err != nil {
		setLegacyRBACCondition(host, metav1.ConditionTrue, reasonLegacyRBACCheckFailed,
			fmt.Sprintf("legacy %s remain: %v", names, err))
		return err
	}
	if !upgraded {
		setLegacyRBACCondition(host, metav1.ConditionTrue, reasonLegacyRBACAwaitingUpgrade,
			fmt.Sprintf("legacy %s remain; they are deleted once the host-manager chart's RoleBinding/%s exists in %s", names, host.Name, host.Namespace))
		return nil
	}

	if err := r.deleteLegacyRBAC(ctx, role, binding); err != nil {
		setLegacyRBACCondition(host, metav1.ConditionTrue, reasonLegacyRBACCleanupFailed,
			fmt.Sprintf("legacy %s remain: %v", names, err))
		return err
	}
	meta.RemoveStatusCondition(&host.Status.Conditions, ConditionLegacyRBACPresent)
	return nil
}

// hostManagerRoleBindingExists reports whether the redesigned host-manager
// chart's RoleBinding/<host> exists in the host's namespace, Helm-owned by the
// host's release. Read live: the RoleBinding cache holds only the operator's
// own, label-selected.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) hostManagerRoleBindingExists(ctx context.Context, host *kdexv1alpha1.KDexHost) (bool, error) {
	rb := &rbacv1.RoleBinding{}
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: host.Namespace, Name: host.Name}, rb); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read RoleBinding %s/%s: %w", host.Namespace, host.Name, err)
	}
	return helmReleasedBy(rb, host), nil
}
