package controller

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const installerNamespacePolicy = "installer_namespace_policy"

// The chart ships the policy config/ declares, and both name the label and
// the installer role the code uses.
func TestInstallerNamespacePolicy_ChartMatchesConfig(t *testing.T) {
	for _, f := range []string{installerNamespacePolicy, installerNamespacePolicy + "_binding"} {
		var cfg, chart map[string]any
		if err := loadManifest(filepath.Join(repoRoot, "config/rbac/fixed", f+".yaml"), &cfg); err != nil {
			t.Fatal(err)
		}
		if err := loadManifest(filepath.Join(repoRoot, "chart/templates/rbac", f+".yaml"), &chart); err != nil {
			t.Fatal(err)
		}
		c, _ := yaml.Marshal(map[string]any{"kind": cfg["kind"], "name": cfg["metadata"].(map[string]any)["name"], "spec": cfg["spec"]})
		h, _ := yaml.Marshal(map[string]any{"kind": chart["kind"], "name": chart["metadata"].(map[string]any)["name"], "spec": chart["spec"]})
		if string(c) != string(h) {
			t.Errorf("%s: chart differs from config:\nchart:\n%s\nconfig:\n%s", f, h, c)
		}
	}
	raw, err := os.ReadFile(filepath.Join(repoRoot, "config/rbac/fixed", installerNamespacePolicy+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// The CEL string literals, quoted, so a name merely contained in another
	// (the policy's own name contains the role's) does not count.
	for _, want := range []string{LabelHostNamespace, installerClusterRoleName, HostNamespaceEnabled, utils.InstallerServiceAccountName, kindClusterRole} {
		if !strings.Contains(string(raw), "'"+want+"'") {
			t.Errorf("policy has no CEL literal '%s'", want)
		}
	}
}

// Against a real API server: the policy refuses an installer binding in a
// namespace no administrator enabled, and one binding anyone but the
// namespace's installer, whoever asks (the suite runs as an administrator).
var _ = Describe("The installer namespace policy", func() {
	It("admits installer bindings only in enabled namespaces, for the installer only", func(ctx SpecContext) {
		policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
		Expect(loadManifest(filepath.Join(repoRoot, "config/rbac/fixed", installerNamespacePolicy+".yaml"), policy)).To(Succeed())
		binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
		Expect(loadManifest(filepath.Join(repoRoot, "config/rbac/fixed", installerNamespacePolicy+"_binding.yaml"), binding)).To(Succeed())
		Expect(k8sClient.Create(ctx, policy)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { Expect(k8sClient.Delete(ctx, policy)).To(Succeed()) })
		Expect(k8sClient.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { Expect(k8sClient.Delete(ctx, binding)).To(Succeed()) })

		suffix := time.Now().UnixNano()
		plain := fmt.Sprintf("vap-plain-%d", suffix)
		enabled := fmt.Sprintf("vap-enabled-%d", suffix)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: plain}})).To(Succeed())
		Expect(k8sClient.Create(ctx, hostNamespace(enabled))).To(Succeed())

		n := 0
		installerBinding := func(ns string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
			n++
			return &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("probe-%d", n), Namespace: ns},
				RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: kindClusterRole, Name: installerClusterRoleName},
				Subjects:   subjects,
			}
		}
		installerOf := func(ns string) rbacv1.Subject {
			return rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: utils.InstallerServiceAccountName, Namespace: ns}
		}

		// The policy takes effect asynchronously: wait until it refuses.
		Eventually(func() error {
			rb := installerBinding(plain, installerOf(plain))
			err := k8sClient.Create(ctx, rb)
			if err == nil {
				_ = k8sClient.Delete(ctx, rb)
				return fmt.Errorf("admitted before the policy took effect")
			}
			if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
				return StopTrying(err.Error())
			}
			return nil
		}, "20s", "200ms").Should(Succeed())

		err := k8sClient.Create(ctx, installerBinding(plain, installerOf(plain)))
		Expect(err).To(MatchError(ContainSubstring("kdex.dev/host-namespace=true")))

		Expect(k8sClient.Create(ctx, installerBinding(enabled, installerOf(enabled)))).To(Succeed())
		Expect(k8sClient.Create(ctx, installerBinding(enabled, installerOf(enabled),
			rbacv1.Subject{Kind: rbacv1.ServiceAccountKind, Name: "borrower", Namespace: enabled}))).
			To(MatchError(ContainSubstring("exactly its namespace's kdex-installer")))
		Expect(k8sClient.Create(ctx, installerBinding(enabled, installerOf(plain)))).
			To(MatchError(ContainSubstring("exactly its namespace's kdex-installer")))

		// Bindings to any other role are not the policy's.
		other := installerBinding(plain, installerOf(plain))
		other.RoleRef.Name = "view"
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
	})
})
