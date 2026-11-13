package controller

import (
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A Helm client is built from the API server's own REST config (here
// envtest's, in a pod the in-cluster one), so its calls reach that server, as
// the installer.
var _ = Describe("Helm client impersonating the installer", func() {
	It("reaches the API server as the namespace's installer", func(ctx SpecContext) {
		ns := fmt.Sprintf("helm-impersonation-%d", time.Now().UnixNano())
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		installer, err := utils.NewHelmClient(ns, cfg, nil, logr.Discard())
		Expect(err).NotTo(HaveOccurred())

		// Without a grant the server refuses the installer by name: the call
		// reached it, impersonating. Uninstall reads the release history first.
		err = installer.Uninstall("absent")
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "want Forbidden, got %v", err)
		Expect(err.Error()).To(ContainSubstring(utils.InstallerUser(ns)))

		// Granted reading Helm's release records, the installer finds none.
		Expect(k8sClient.Create(ctx, &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "release-reader", Namespace: ns},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list"}}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "release-reader", Namespace: ns},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: "release-reader"},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: utils.InstallerServiceAccountName, Namespace: ns}},
		})).To(Succeed())
		Eventually(func() error {
			return installer.Uninstall("absent")
		}, "10s", "100ms").Should(Succeed())
	})
})
