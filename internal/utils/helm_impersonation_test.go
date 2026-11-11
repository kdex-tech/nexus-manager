package utils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/kube"
	"k8s.io/client-go/rest"
)

// isolateFromKubeconfig leaves nothing a kubeconfig loader could find: a
// KUBECONFIG naming an empty file, and no HELM_KUBE* override. An empty or
// unset KUBECONFIG would not do: the loader then falls back to
// clientcmd.RecommendedHomeFile, the developer's ~/.kube/config, fixed when
// the package initialised -- setting HOME here changes nothing. So a
// regression to kubeconfig loading finds no cluster, rather than a real one.
func isolateFromKubeconfig(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", empty)
	t.Setenv("HELM_KUBEAPISERVER", "")
	t.Setenv("HELM_KUBETOKEN", "")
}

// baseRESTConfig names a TEST-NET-1 address (RFC 5737): nothing answers it.
func baseRESTConfig() *rest.Config {
	return &rest.Config{Host: "https://192.0.2.1:443", BearerToken: "x"}
}

// helmGetterConfigs returns the REST config the client's Helm configuration
// acts through, and the one its kube client (installs, upgrades, deletes)
// does.
func helmGetterConfigs(t *testing.T, h *HelmClient) (*rest.Config, *rest.Config) {
	t.Helper()
	action, err := h.actionConfig.RESTClientGetter.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	kc, ok := h.actionConfig.KubeClient.(*kube.Client)
	if !ok {
		t.Fatalf("KubeClient is %T, want *kube.Client", h.actionConfig.KubeClient)
	}
	kcfg, err := kc.Factory.ToRESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	return action, kcfg
}

// The client reaches the API server the operator does: the base
// config it is given, not whatever a kubeconfig loader finds -- in a pod,
// nothing, and an impersonating loader then fell back to localhost:8080.
func TestNewHelmClient_InstallerUsesTheBaseConfigWithImpersonation(t *testing.T) {
	isolateFromKubeconfig(t)

	want := "system:serviceaccount:ns:kdex-installer"
	if got := InstallerUser("ns"); got != want {
		t.Fatalf("InstallerUser = %q, want %q", got, want)
	}

	h, err := NewHelmClient("ns", baseRESTConfig(), nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	action, kc := helmGetterConfigs(t, h)
	for what, cfg := range map[string]*rest.Config{"action": action, "kube client": kc} {
		if cfg.Host != "https://192.0.2.1:443" {
			t.Errorf("%s Host = %q, want https://192.0.2.1:443", what, cfg.Host)
		}
		if cfg.BearerToken != "x" {
			t.Errorf("%s BearerToken = %q, want the base config's", what, cfg.BearerToken)
		}
		if cfg.Impersonate.UserName != want {
			t.Errorf("%s Impersonate.UserName = %q, want %q", what, cfg.Impersonate.UserName, want)
		}
	}
}

// There is no other identity: a client built without asking for one still
// acts as its namespace's installer -- never as the operator's own identity --
// and HELM_KUBEASUSER cannot change that.
func TestNewHelmClient_AlwaysImpersonatesTheInstaller(t *testing.T) {
	isolateFromKubeconfig(t)
	t.Setenv("HELM_KUBEASUSER", "system:serviceaccount:kube-system:leaked")

	h, err := NewHelmClient("other", baseRESTConfig(), nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	action, kc := helmGetterConfigs(t, h)
	for what, cfg := range map[string]*rest.Config{"action": action, "kube client": kc} {
		if cfg.Impersonate.UserName != InstallerUser("other") || len(cfg.Impersonate.Groups) != 0 {
			t.Errorf("%s Impersonate = %+v, want only %s", what, cfg.Impersonate, InstallerUser("other"))
		}
	}
}

// Helm resolves a manifest's unset namespace from the getter's kubeconfig
// loader: it must name the client's namespace.
func TestNewHelmClient_LoaderNamesTheClientsNamespace(t *testing.T) {
	isolateFromKubeconfig(t)

	h, err := NewHelmClient("ns", baseRESTConfig(), nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	kc := h.actionConfig.KubeClient.(*kube.Client)
	ns, _, err := kc.Factory.ToRawKubeConfigLoader().Namespace()
	if err != nil || ns != "ns" {
		t.Errorf("loader namespace = %q, %v; want ns", ns, err)
	}
	cfg, err := kc.Factory.ToRawKubeConfigLoader().ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://192.0.2.1:443" || cfg.Impersonate.UserName != InstallerUser("ns") {
		t.Errorf("loader config = %q as %q, want the base config impersonating the installer", cfg.Host, cfg.Impersonate.UserName)
	}
}

// The base config is the operator's own and is shared: building a client
// must not change it.
func TestNewHelmClient_LeavesTheBaseConfigAlone(t *testing.T) {
	isolateFromKubeconfig(t)
	base := baseRESTConfig()

	if _, err := NewHelmClient("ns", base, nil, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	if base.Impersonate.UserName != "" || base.WrapTransport != nil || base.Burst != 0 {
		t.Errorf("base config changed: %+v", base)
	}
}

func TestNewHelmClient_RequiresABaseConfig(t *testing.T) {
	if _, err := NewHelmClient("ns", nil, nil, logr.Discard()); err == nil {
		t.Error("want an error for a nil base config")
	}
}
