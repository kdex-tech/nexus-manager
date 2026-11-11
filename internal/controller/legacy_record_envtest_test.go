// Migration code: delete in release B (privilege-redesign follow-up issue).
// Every test in this file exercises the legacy RBAC migration.

package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	chartv2 "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// ensureInstallerClusterRole makes the installer's ClusterRole hold the rules
// the nexus chart aggregates into it (config/rbac/fixed): envtest runs no
// aggregation controller, so the base role's rules are copied in directly.
func ensureInstallerClusterRole(ctx context.Context) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "fixed", "installer_base_role.yaml"))
	Expect(err).NotTo(HaveOccurred())
	base := &rbacv1.ClusterRole{}
	Expect(yaml.Unmarshal(raw, base)).To(Succeed())
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: installerClusterRoleName}, Rules: base.Rules}
	if err := k8sClient.Create(ctx, role); err != nil {
		Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "create installer ClusterRole: %v", err)
	}
}

// releaseStorage is Helm's release storage in ns, read and written with the
// test's own (unrestricted) identity.
func releaseStorage(ns string) *storage.Storage {
	cs, err := kubernetes.NewForConfig(cfg)
	Expect(err).NotTo(HaveOccurred())
	return storage.Init(driver.NewSecrets(cs.CoreV1().Secrets(ns)))
}

// recordExists reports whether any revision of the release is stored.
func recordExists(ns, name string) bool {
	h, err := releaseStorage(ns).History(name)
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return false
	}
	Expect(err).NotTo(HaveOccurred())
	return len(h) > 0
}

// storeRecord writes a host-manager release record, as anyone who writes
// Secrets in the namespace can, through Helm's own storage.
func storeRecord(ns, name, manifest string) {
	Expect(releaseStorage(ns).Create(&rspb.Release{
		Name:      name,
		Namespace: ns,
		Version:   1,
		Info:      &rspb.Info{Status: common.StatusDeployed},
		Chart:     &chartv2.Chart{Metadata: &chartv2.Metadata{Name: "host-manager", Version: "0.32.0"}},
		Manifest:  manifest,
	})).To(Succeed())
}

// standInForGC removes the foregroundDeletion finalizer from any of objs
// being deleted, as the garbage collector -- which envtest does not run --
// does for an object with no dependents. Without it a foreground uninstall
// waits out its whole timeout.
func standInForGC(ctx context.Context, objs ...client.Object) {
	go func() {
		defer GinkgoRecover()
		for ctx.Err() == nil {
			for _, o := range objs {
				live := o.DeepCopyObject().(client.Object)
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(o), live); err != nil || live.GetDeletionTimestamp() == nil {
					continue
				}
				fins := slices.DeleteFunc(live.GetFinalizers(), func(f string) bool { return f == metav1.FinalizerDeleteDependents })
				live.SetFinalizers(fins)
				_ = k8sClient.Update(ctx, live)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
}

// alive reports whether obj still exists and is not being deleted.
func alive(ctx context.Context, obj client.Object) bool {
	live := obj.DeepCopyObject().(client.Object)
	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), live)
	return err == nil && live.GetDeletionTimestamp() == nil
}

// Against a real API server, real Helm and the installer's real rules: a
// teardown acts on a release record only as the namespace's installer, so
// what a forged record lists outside the namespace is never deleted.
var _ = Describe("Teardown of a forged host-manager release record", func() {
	var (
		ns, other string
		r         *KDexHostReconciler
		host      *kdexv1alpha1.KDexHost
	)

	BeforeEach(func(ctx SpecContext) {
		suffix := time.Now().UnixNano()
		ns, other = fmt.Sprintf("forged-%d", suffix), fmt.Sprintf("other-tenant-%d", suffix)
		for _, n := range []string{ns, other} {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: n}})).To(Succeed())
		}
		ensureInstallerClusterRole(ctx)
		host = &kdexv1alpha1.KDexHost{ObjectMeta: metav1.ObjectMeta{Name: "my-host", Namespace: ns}}
		r = &KDexHostReconciler{
			Client:       k8sClient,
			APIReader:    k8sClient,
			ControllerID: "forged-record",
			Ctx:          ctx,
			Scheme:       scheme.Scheme,
			HelmClientFactory: func(namespace string, secrets kdexv1alpha1.Secrets, logger logr.Logger) (utils.HelmClientInterface, error) {
				return utils.NewHelmClient(namespace, cfg, secrets, logger, utils.WithMetadataReader(k8sClient))
			},
		}
		// Bootstrap the installer and wait for the API server's authorizer to
		// see its grant, so each spec's teardown meets its rules, not their
		// propagation.
		Expect(r.ensureInstaller(ctx, ns)).To(Succeed())
		installer, err := utils.NewHelmClient(ns, cfg, nil, logr.Discard())
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() error {
			_, err := installer.ReleaseStatus("absent")
			return err
		}, "10s", "100ms").Should(Succeed())
	})

	It("never deletes a Secret in another namespace or a ClusterRoleBinding a list document names", func(ctx SpecContext) {
		victim := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "victim", Namespace: other}}
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "victim-" + other},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "view"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: "default", Namespace: other}},
		}
		owned := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "my-host", Namespace: ns}}
		for _, o := range []client.Object{victim, binding, owned} {
			Expect(k8sClient.Create(ctx, o)).To(Succeed())
		}
		DeferCleanup(func(ctx SpecContext) { _ = k8sClient.Delete(ctx, binding) })
		// The forgery: a document whose top-level kind and name are the
		// host's legacy ClusterRole, with an items list Helm would flatten.
		storeRecord(ns, host.Name, fmt.Sprintf(`---
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-host
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: my-host
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: victim
    namespace: %s
- apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRoleBinding
  metadata:
    name: %s
`, other, binding.Name))
		gcCtx, stop := context.WithCancel(ctx)
		defer stop()
		standInForGC(gcCtx, victim, binding, owned)

		Expect(r.teardownHost(ctx, host, nil, logr.Discard())).To(Succeed())

		Expect(alive(ctx, victim)).To(BeTrue(), "the other tenant's Secret was deleted")
		Expect(alive(ctx, binding)).To(BeTrue(), "the ClusterRoleBinding was deleted")
		// What the record rightly listed in the namespace is uninstalled, and
		// the record with it.
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(owned), &corev1.ConfigMap{}))
		}, "10s", "100ms").Should(BeTrue())
		Expect(recordExists(ns, host.Name)).To(BeFalse(), "the release record outlived the uninstall")
	})

	It("fails closed on a cluster-scoped kind the installer cannot delete", func(ctx SpecContext) {
		clusterKind := &kdexv1alpha1.KDexClusterTranslation{
			ObjectMeta: metav1.ObjectMeta{Name: "shared-" + ns},
			Spec: kdexv1alpha1.KDexTranslationSpec{Translations: []kdexv1alpha1.Translation{
				{Lang: "en", KeysAndValues: map[string]string{"k": "v"}},
			}},
		}
		Expect(k8sClient.Create(ctx, clusterKind)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { _ = k8sClient.Delete(ctx, clusterKind) })
		storeRecord(ns, host.Name, fmt.Sprintf(`---
apiVersion: kdex.dev/v1alpha1
kind: KDexClusterTranslation
metadata:
  name: %s
`, clusterKind.Name))

		err := r.teardownHost(ctx, host, nil, logr.Discard())
		Expect(err).To(MatchError(ContainSubstring("failed to uninstall release my-host")))
		Expect(alive(ctx, clusterKind)).To(BeTrue(), "the cluster-scoped object was deleted")
		// The record stays for an administrator to deal with.
		Expect(recordExists(ns, host.Name)).To(BeTrue(), "the release record is gone")
	})
})
