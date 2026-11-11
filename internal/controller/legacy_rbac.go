package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Migration from the cluster-scoped host-manager RBAC (make-before-break).
//
// Before the privilege redesign every host-manager release rendered a
// ClusterRoleBinding/<host>-<namespace>, and -- unless the operator pointed it
// at a shared role (HOST_MANAGER_CLUSTER_ROLE) -- a ClusterRole/<host>. The
// redesigned chart renders only a namespaced Role and RoleBinding/<host>.
// Upgrading as the namespace's installer cannot remove the legacy objects:
// Helm skips a dropped resource it is Forbidden to Get. So nexus deletes them
// itself, once the upgrade has landed and rolled out, and uninstalls a release
// whose record still lists them with its own identity.
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

// clusterScopedKinds are the cluster-scoped kinds a release record may list
// that a namespace's installer cannot uninstall. A kind missing here is
// treated as namespaced, so its uninstall is attempted as the installer and
// fails Forbidden: a blocked teardown, never a wider identity.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
var clusterScopedKinds = map[string]struct{}{
	"APIService":                       {},
	kindClusterRole:                    {},
	"ClusterRoleBinding":               {},
	"CustomResourceDefinition":         {},
	"IngressClass":                     {},
	"MutatingWebhookConfiguration":     {},
	"Namespace":                        {},
	"PersistentVolume":                 {},
	"PriorityClass":                    {},
	"RuntimeClass":                     {},
	"StorageClass":                     {},
	"ValidatingAdmissionPolicy":        {},
	"ValidatingAdmissionPolicyBinding": {},
	"ValidatingWebhookConfiguration":   {},
}

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

// hostReleaseUninstallIdentity is who uninstalls the host-manager release,
// decided by its record, read through operator (an installer may not be able
// to read a record in every state). A record listing nothing outside the
// host's namespace is the installer's. One listing legacy RBAC is the
// operator's: the installer, Forbidden to Get a ClusterRole, would fail the
// uninstall -- whether or not the objects still exist, since an uninstall that
// failed part-way keeps the record whole.
//
// The record can be forged by whoever writes Secrets in the namespace, and an
// uninstall as the operator deletes what it lists. So the operator is used
// only when everything the record lists outside the namespace is the host's
// legacy RBAC -- by name, and, where the object exists, by its Helm ownership
// -- and anything else is refused, for an administrator to uninstall.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) hostReleaseUninstallIdentity(ctx context.Context, operator utils.HelmClientInterface, host *kdexv1alpha1.KDexHost) (utils.HelmIdentity, error) {
	objs, err := operator.ReleaseObjects(host.Name)
	if err != nil {
		return 0, err
	}
	identity := utils.HelmAsInstaller
	for _, o := range objs {
		_, clusterScoped := clusterScopedKinds[o.Kind]
		if !clusterScoped && (o.Namespace == "" || o.Namespace == host.Namespace) && !strings.HasSuffix(o.Kind, "List") {
			continue
		}
		if err := r.verifyLegacyRecordObject(ctx, o, host); err != nil {
			return 0, err
		}
		identity = utils.HelmAsOperator
	}
	return identity, nil
}

// verifyLegacyRecordObject accepts o, an object the host's release record lists
// outside the namespace, only if it is the host's legacy ClusterRole or
// ClusterRoleBinding by name, and the object of that name, if one exists, is
// the release's.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) verifyLegacyRecordObject(ctx context.Context, o utils.ReleaseObject, host *kdexv1alpha1.KDexHost) error {
	var live client.Object
	var ours func() bool
	switch {
	case o.Kind == kindClusterRole && o.Name == host.Name:
		role := &rbacv1.ClusterRole{}
		live, ours = role, func() bool { return isLegacyRole(role, host) }
	case o.Kind == "ClusterRoleBinding" && o.Name == legacyBindingName(host):
		binding := &rbacv1.ClusterRoleBinding{}
		live, ours = binding, func() bool { return isLegacyBinding(binding, host) }
	default:
		return fmt.Errorf("the record of release %s/%s lists %s %q outside its namespace, which is not its legacy RBAC: refusing to uninstall it as the operator; uninstall it by hand",
			host.Namespace, host.Name, o.Kind, o.Name)
	}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: o.Name}, live); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to read %s %q listed by the record of release %s/%s: %w", o.Kind, o.Name, host.Namespace, host.Name, err)
	}
	if !ours() {
		return fmt.Errorf("the record of release %s/%s lists %s %q, which exists but is not the release's: refusing to uninstall it as the operator; uninstall it by hand",
			host.Namespace, host.Name, o.Kind, o.Name)
	}
	return nil
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
func (r *KDexHostReconciler) cleanupLegacyRBAC(ctx context.Context, host *kdexv1alpha1.KDexHost) (pending bool, err error) {
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

	upgraded, err := r.hostManagerUpgraded(ctx, host)
	if err != nil {
		setLegacyRBACCondition(host, metav1.ConditionTrue, reasonLegacyRBACProofReadFailed,
			fmt.Sprintf("legacy %s remain: %v", names, err))
		return true, err
	}
	if !upgraded {
		setLegacyRBACCondition(host, metav1.ConditionTrue, reasonLegacyRBACAwaitingUpgrade,
			fmt.Sprintf("legacy %s remain; they are deleted once the host-manager chart's RoleBinding/%s exists in %s and Deployment/%s has rolled out",
				names, host.Name, host.Namespace, host.Name))
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
// the legacy grant: the chart's RoleBinding/<host>, Helm-owned by the host's
// release, exists in the host's namespace -- read live, the RoleBinding cache
// holds only the operator's own -- and Deployment/<host> has fully rolled out,
// so no pod still running on the legacy grant is left.
//
// Migration code: delete in release B (see the follow-up issue filed in Task N9).
func (r *KDexHostReconciler) hostManagerUpgraded(ctx context.Context, host *kdexv1alpha1.KDexHost) (bool, error) {
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
	if err := r.Get(ctx, key, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read Deployment %s: %w", key, err)
	}
	return deploymentRolledOut(deployment), nil
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
