package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
)

const impersonationTestKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: ctx
  context:
    cluster: c
    user: u
current-context: ctx
users:
- name: u
  user:
    token: t
`

// Impersonation has to reach the REST config Helm builds, so it must be on the
// settings before actionConfig.Init captures the RESTClientGetter.
func TestNewHelmClient_ImpersonationSetBeforeInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(impersonationTestKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)

	want := "system:serviceaccount:ns:kdex-installer"
	if got := InstallerUser("ns"); got != want {
		t.Fatalf("InstallerUser = %q, want %q", got, want)
	}

	h, err := NewHelmClient("ns", nil, logr.Discard(), WithImpersonation(InstallerUser("ns")))
	if err != nil {
		t.Fatal(err)
	}
	if h.settings.KubeAsUser != want {
		t.Fatalf("KubeAsUser = %q, want %q", h.settings.KubeAsUser, want)
	}
	cfg, err := h.settings.RESTClientGetter().ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Impersonate.UserName != want {
		t.Fatalf("Impersonate.UserName = %q, want %q", cfg.Impersonate.UserName, want)
	}
}
