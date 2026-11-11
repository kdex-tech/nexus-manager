package controller

import (
	"context"
	"fmt"
	"slices"

	"github.com/Masterminds/semver/v3"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MinHostManagerChartVersion is the oldest host-manager chart this operator
// installs. Older charts still ship a ClusterRole/roleRef of their own, which
// the installer identity can no longer create.
const MinHostManagerChartVersion = "0.33.0"

// ConditionReasonHostManagerChartUnsupported is the reason on a host whose
// host-manager chart version is below MinHostManagerChartVersion.
const ConditionReasonHostManagerChartUnsupported kdexv1alpha1.ConditionReason = "HostManagerChartUnsupported"

// clusterReaderBindingName is the fixed ClusterRoleBinding shipped by the
// nexus chart and kustomize WITHOUT subjects: this operator owns the subject
// list at runtime, so a Helm upgrade must not reset it.
const clusterReaderBindingName = "kcnas-operator-host-manager-cluster-reader"

// hostManagerChartUnsupportedError is a host-manager chart version the
// operator will not install.
type hostManagerChartUnsupportedError struct{ msg string }

func (e *hostManagerChartUnsupportedError) Error() string { return e.msg }

// checkHostManagerChartVersion refuses any version that is not strict semver
// at or above MinHostManagerChartVersion (ranges and tags such as "^0.33" or
// "latest" included: the floor cannot be verified for them).
func checkHostManagerChartVersion(version string) error {
	v, err := semver.StrictNewVersion(version)
	if err != nil {
		return &hostManagerChartUnsupportedError{fmt.Sprintf("host-manager chart version %q is not an exact semantic version; at least %s is required", version, MinHostManagerChartVersion)}
	}
	if v.LessThan(semver.MustParse(MinHostManagerChartVersion)) {
		return &hostManagerChartUnsupportedError{fmt.Sprintf("host-manager chart version %q is older than the minimum supported %s", version, MinHostManagerChartVersion)}
	}
	return nil
}

// hostManagerServiceAccountName mirrors the host-manager chart's
// "host-manager.serviceAccountName" helper (chart/templates/_helpers.tpl) over
// the final merged values: the cluster-reader subject must name the SA the
// chart actually renders. nexus forces the chart's fullname to the host name
// via fullnameOverride.
func hostManagerServiceAccountName(vals map[string]any, hostName string) string {
	sa, _ := vals["serviceAccount"].(map[string]any)
	name, _ := sa["name"].(string)
	create := true
	if c, ok := sa["create"].(bool); ok {
		create = c
	}
	if name != "" {
		return name
	}
	if create {
		return hostName
	}
	return "default"
}

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
// cluster-reader ClusterRoleBinding. Idempotent.
func (r *KDexHostReconciler) removeClusterReaderSubject(ctx context.Context, namespace, sa string) error {
	subject := rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: sa, Namespace: namespace}
	return r.updateClusterReaderSubjects(ctx, func(subjects []rbacv1.Subject) ([]rbacv1.Subject, bool) {
		if !slices.Contains(subjects, subject) {
			return subjects, false
		}
		return slices.DeleteFunc(slices.Clone(subjects), func(s rbacv1.Subject) bool { return s == subject }), true
	})
}

// updateClusterReaderSubjects is the optimistic-lock read-modify-update of
// the binding's subjects. The binding is owned by the nexus chart: when it is
// missing the chart is too old, and creating it here would split ownership.
// Read through the API reader so no cluster-wide ClusterRoleBinding informer
// starts.
func (r *KDexHostReconciler) updateClusterReaderSubjects(ctx context.Context, modify func([]rbacv1.Subject) ([]rbacv1.Subject, bool)) error {
	key := types.NamespacedName{Name: clusterReaderBindingName}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		binding := &rbacv1.ClusterRoleBinding{}
		if err := r.reader().Get(ctx, key, binding); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("ClusterRoleBinding %q not found: the nexus-manager chart that ships it is too old", clusterReaderBindingName)
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

// healClusterReaderSubject re-adds a host's host-manager ServiceAccount to the
// cluster-reader binding on every reconcile, outside the release-hash gate, so
// the subject self-heals if anything resets the binding. Only a host whose
// host-manager release has been installed has such an ServiceAccount. A
// failure is logged, not fatal: the next reconcile retries it.
func (r *KDexHostReconciler) healClusterReaderSubject(ctx context.Context, host *kdexv1alpha1.KDexHost) error {
	if host.Status.Attributes[AttributeHelmReleaseStatus] != HelmStatusCompleted {
		return nil
	}
	spec, err := r.buildHostManagerChartSpec(host)
	if err != nil {
		return err
	}
	return r.ensureClusterReaderSubject(ctx, host.Namespace, hostManagerServiceAccountName(spec.Values, host.Name))
}
