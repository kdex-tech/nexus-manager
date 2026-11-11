package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// LabelManagedBy and ManagedByOperator mark every object this operator
	// creates outside Helm. CacheOptions selects the installer's
	// ServiceAccounts and RoleBindings by this label.
	LabelManagedBy    = "app.kubernetes.io/managed-by"
	ManagedByOperator = "kcnas-operator"

	// installerClusterRoleName is the fixed, aggregated ClusterRole the nexus
	// chart ships for the installer; each namespace's installer RoleBinding
	// grants it in that namespace only.
	installerClusterRoleName = "kcnas-operator-installer"

	// installerRoleBindingName names the installer RoleBinding after the
	// ServiceAccount it binds.
	installerRoleBindingName = utils.InstallerServiceAccountName

	// installerFinalizer holds the installer RoleBinding until the last host
	// in its namespace has finished teardown: a namespace deletion removes
	// the binding alongside the hosts, and a teardown still uninstalling as
	// the installer must not lose its grant part-way.
	installerFinalizer = "kdex.dev/installer"

	// kindClusterRole is the kind a roleRef or a release record names a
	// ClusterRole by.
	kindClusterRole = "ClusterRole"
)

// errNamespaceTerminating stops installer bootstrap in a namespace being
// deleted, which rejects creates. It wraps errHostTerminating: every host in
// the namespace is about to be torn down, so a helm operation that meets it
// stops quietly and leaves the host's status to the teardown.
var errNamespaceTerminating = fmt.Errorf("namespace is terminating: %w", errHostTerminating)

func installerLabels() map[string]string {
	return map[string]string{LabelManagedBy: ManagedByOperator}
}

// namespaceTerminating reports whether the namespace is being deleted, read
// live so no Namespace informer starts. A namespace that is gone counts as
// terminating: nothing can be created in it and nothing is left to uninstall.
func (r *KDexHostReconciler) namespaceTerminating(ctx context.Context, namespace string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("failed to read namespace %q: %w", namespace, err)
	}
	return !ns.DeletionTimestamp.IsZero(), nil
}

// ensureInstaller bootstraps the namespace's installer identity: the
// ServiceAccount utils.InstallerServiceAccountName and a RoleBinding granting
// it installerClusterRoleName, both labelled as this operator's and the
// RoleBinding holding installerFinalizer. Neither has an owner: several hosts
// may share the namespace, so no one host owns them (releaseInstallerIfUnused
// removes them after the last). Idempotent; an existing object is adopted and
// healed (label, finalizer, subjects). In a terminating namespace it creates
// nothing and returns errNamespaceTerminating.
func (r *KDexHostReconciler) ensureInstaller(ctx context.Context, namespace string) error {
	terminating, err := r.namespaceTerminating(ctx, namespace)
	if err != nil {
		return err
	}
	if terminating {
		return fmt.Errorf("cannot bootstrap the installer in %q: %w", namespace, errNamespaceTerminating)
	}
	return r.ensureInstallerObjects(ctx, namespace)
}

// ensureInstallerObjects is ensureInstaller for a caller that has already
// found the namespace not terminating: one pass makes one decision, so a
// namespace that starts terminating after it fails the creates, and the next
// pass sees it terminating.
func (r *KDexHostReconciler) ensureInstallerObjects(ctx context.Context, namespace string) error {
	saKey := client.ObjectKey{Namespace: namespace, Name: utils.InstallerServiceAccountName}
	if err := ensureOwned(ctx, r, saKey, func() *corev1.ServiceAccount { return &corev1.ServiceAccount{} },
		func(sa *corev1.ServiceAccount) (bool, error) {
			return ensureLabels(sa), nil
		}); err != nil {
		return fmt.Errorf("failed to ensure the installer ServiceAccount in %q: %w", namespace, err)
	}

	roleRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: installerClusterRoleName}
	subjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: utils.InstallerServiceAccountName, Namespace: namespace}}
	rbKey := client.ObjectKey{Namespace: namespace, Name: installerRoleBindingName}
	if err := ensureOwned(ctx, r, rbKey, func() *rbacv1.RoleBinding { return &rbacv1.RoleBinding{} },
		func(rb *rbacv1.RoleBinding) (bool, error) {
			if rb.ResourceVersion == "" {
				rb.RoleRef = roleRef
			} else if rb.RoleRef != roleRef {
				// roleRef is immutable: a binding of this name to anything
				// else is not this operator's to repurpose.
				return false, fmt.Errorf("it binds %s %q, not ClusterRole %q", rb.RoleRef.Kind, rb.RoleRef.Name, installerClusterRoleName)
			}
			changed := ensureLabels(rb)
			if controllerutil.AddFinalizer(rb, installerFinalizer) {
				changed = true
			}
			// Exactly the installer: an added subject would borrow its grant.
			if !slices.Equal(rb.Subjects, subjects) {
				rb.Subjects = subjects
				changed = true
			}
			return changed, nil
		}); err != nil {
		return fmt.Errorf("failed to ensure the installer RoleBinding in %q: %w", namespace, err)
	}
	return nil
}

// ensureLabels adds the operator's labels to obj, reporting whether it changed.
func ensureLabels(obj client.Object) bool {
	labels := obj.GetLabels()
	want := installerLabels()
	changed := false
	for k, v := range want {
		if labels[k] != v {
			changed = true
		}
	}
	if !changed {
		return false
	}
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, want)
	obj.SetLabels(labels)
	return true
}

// ensureOwned creates the object at key, or brings an existing one in line,
// as mutate requires. mutate runs on a fresh read (or, to create, on an empty
// object with only the key set) and reports whether an existing object needs
// writing; a new one is always created.
//
// The cached read is label-selected (CacheOptions), so an object that exists
// without the label is invisible to it: a create that meets one, or an update
// that meets a newer version, re-reads it live -- into a fresh object, so
// nothing from the failed attempt survives the decode -- and adopts it. An
// update that finds the cached object deleted since creates it anew.
func ensureOwned[T client.Object](ctx context.Context, r *KDexHostReconciler, key client.ObjectKey, newObj func() T, mutate func(T) (bool, error)) error {
	obj := newObj()
	err := r.Get(ctx, key, obj)
	if err == nil {
		changed, err := mutate(obj)
		if err != nil || !changed {
			return err
		}
		err = r.Update(ctx, obj, client.FieldOwner(r.ControllerID))
		switch {
		case apierrors.IsConflict(err):
			return ensureOwnedLive(ctx, r, key, newObj, mutate)
		case !apierrors.IsNotFound(err):
			return err
		}
		// The cache still held an object deleted since: create it anew.
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	obj = newObj()
	obj.SetName(key.Name)
	obj.SetNamespace(key.Namespace)
	if _, err := mutate(obj); err != nil {
		return err
	}
	err = r.Create(ctx, obj, client.FieldOwner(r.ControllerID))
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	return ensureOwnedLive(ctx, r, key, newObj, mutate)
}

// ensureOwnedLive is ensureOwned's fallback when the cache missed the object
// or was stale: it reads live and updates, retrying on conflict.
func ensureOwnedLive[T client.Object](ctx context.Context, r *KDexHostReconciler, key client.ObjectKey, newObj func() T, mutate func(T) (bool, error)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		live := newObj()
		if err := r.reader().Get(ctx, key, live); err != nil {
			return err
		}
		changed, err := mutate(live)
		if err != nil || !changed {
			return err
		}
		return r.Update(ctx, live, client.FieldOwner(r.ControllerID))
	})
}

// releaseInstallerIfUnused removes the namespace's installer identity once no
// KDexHost other than departingHost is left in it that is not itself
// terminating: it removes the RoleBinding's finalizer, then deletes the
// RoleBinding and the ServiceAccount. A host whose teardown is still to run
// re-creates them (ensureInstaller) before it uninstalls. Idempotent; the
// hosts are listed live, so one created since the cache last saw the
// namespace keeps the installer.
func (r *KDexHostReconciler) releaseInstallerIfUnused(ctx context.Context, namespace, departingHost string) error {
	var hosts kdexv1alpha1.KDexHostList
	if err := r.reader().List(ctx, &hosts, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to list the namespace's KDexHosts: %w", err)
	}
	for _, h := range hosts.Items {
		if h.Name != departingHost && h.DeletionTimestamp.IsZero() {
			return nil
		}
	}

	rbKey := client.ObjectKey{Namespace: namespace, Name: installerRoleBindingName}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		rb := &rbacv1.RoleBinding{}
		if err := r.reader().Get(ctx, rbKey, rb); err != nil {
			return err
		}
		if !controllerutil.RemoveFinalizer(rb, installerFinalizer) {
			return nil
		}
		return r.Update(ctx, rb, client.FieldOwner(r.ControllerID))
	})
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("failed to release the installer RoleBinding's finalizer in %q: %w", namespace, err)
	}

	var errs []error
	for _, obj := range []client.Object{
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: installerRoleBindingName, Namespace: namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: utils.InstallerServiceAccountName, Namespace: namespace}},
	} {
		if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("failed to delete the installer %T %s/%s: %w", obj, namespace, obj.GetName(), err))
		}
	}
	return errors.Join(errs...)
}

// teardownHost removes everything a terminating host owns, in order: any
// legacy cluster-scoped RBAC, which no namespace deletion removes; its Helm
// releases, unless the namespace is terminating -- its deletion removes them,
// and refuses the installer bootstrap an uninstall needs; then the
// cluster-reader subject recorded for it; then the namespace's installer, if
// no other live host needs it. Every step is idempotent, so a pass that fails
// part-way is redone whole by the next.
func (r *KDexHostReconciler) teardownHost(ctx context.Context, host *kdexv1alpha1.KDexHost, secrets kdexv1alpha1.Secrets, log logr.Logger) error {
	terminating, err := r.namespaceTerminating(ctx, host.Namespace)
	if err != nil {
		return err
	}
	// Migration code: delete in release B (privilege-redesign follow-up
	// issue). nexus deletes the host's legacy RBAC itself -- matched by UID
	// and Helm ownership, never by the release record -- before the
	// uninstall, which then no longer needs to.
	role, binding, err := r.legacyRBAC(ctx, host)
	if err != nil {
		return err
	}
	if err := r.deleteLegacyRBAC(ctx, role, binding); err != nil {
		return err
	}

	if terminating {
		log.V(1).Info("namespace is terminating; its deletion removes the host's releases", "host", host.Name, "namespace", host.Namespace)
	} else if err := r.uninstallHostReleases(ctx, host, secrets, log); err != nil {
		return err
	}

	if sa := recordedHostManagerServiceAccount(host); sa != "" {
		if err := r.removeClusterReaderSubject(ctx, host.Namespace, sa); err != nil {
			return fmt.Errorf("failed to remove the cluster-reader subject %s/%s: %w", host.Namespace, sa, err)
		}
	}

	return r.releaseInstallerIfUnused(ctx, host.Namespace, host.Name)
}

// uninstallHostReleases uninstalls the host-manager release and every
// companion the host owns, impersonating the namespace's installer -- the only
// identity a Helm client has, so a forged release record can act with no more
// than the installer's namespace-bounded rights; one listing anything else
// fails the uninstall closed, and the host keeps its finalizer. Its caller has
// found the namespace not terminating. It bootstraps the installer first: a
// host deleted before any helm run created it, or after a sibling's teardown
// released it, would otherwise be refused every uninstall and hold its
// finalizer forever.
//
// Both uninstalls are attempted before any failure is returned, so one
// blocked release does not strand the others; retrying is safe, since
// uninstalling a release that is already gone succeeds.
func (r *KDexHostReconciler) uninstallHostReleases(ctx context.Context, host *kdexv1alpha1.KDexHost, secrets kdexv1alpha1.Secrets, log logr.Logger) error {
	if err := r.ensureInstallerObjects(ctx, host.Namespace); err != nil {
		return err
	}
	c, err := r.getOrCreateHelmClient(host.Name, host.Namespace, secrets, log.WithName("helm"))
	if err != nil {
		return err
	}

	var uninstallErrs []error
	// Migration code: delete in release B (privilege-redesign follow-up
	// issue). A pre-redesign record still lists the legacy RBAC teardownHost
	// has just deleted, which the installer may not delete: drop it from the
	// record so the uninstall does not fail on it.
	dropped, err := c.DropClusterRBACFromRecord(host.Name)
	if len(dropped) != 0 {
		log.Info("dropped cluster-scoped RBAC from the host-manager release record before uninstalling it",
			"host", host.Name, "namespace", host.Namespace, "dropped", dropped)
	}
	if err == nil {
		err = c.Uninstall(host.Name)
	}
	if err != nil {
		log.Error(err, "failed to uninstall host manager release", "name", host.Name)
		uninstallErrs = append(uninstallErrs, err)
	}

	// Everything this host owns, not only what it still declares, so a
	// companion dropped in the same edit that deleted the host cannot outlive
	// the CR.
	hostReleases, err := r.kdexHostNames(ctx, host.Namespace)
	if err == nil {
		err = r.uninstallCompanionCharts(c, host, hostReleases, log)
	}
	if err != nil {
		log.Error(err, "failed to uninstall companion releases", "host", host.Name)
		uninstallErrs = append(uninstallErrs, err)
	}
	return errors.Join(uninstallErrs...)
}
