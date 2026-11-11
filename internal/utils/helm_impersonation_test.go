package utils

import (
	"testing"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/kube"
	"k8s.io/client-go/rest"
)

// isolateFromKubeconfig leaves nothing a kubeconfig loader could find: no
// KUBECONFIG, no HELM_KUBE* override, and a HOME with no ~/.kube/config. A
// pod's environment, where the in-cluster fallback was lost.
func isolateFromKubeconfig(t *testing.T) {
	t.Helper()
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HELM_KUBEAPISERVER", "")
	t.Setenv("HELM_KUBETOKEN", "")
}

func baseRESTConfig() *rest.Config {
	return &rest.Config{Host: "https://10.0.0.1:443", BearerToken: "x"}
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

// The installer client reaches the API server the operator does: the base
// config it is given, not whatever a kubeconfig loader finds -- in a pod,
// nothing, and an impersonating loader then fell back to localhost:8080.
func TestNewHelmClient_InstallerUsesTheBaseConfigWithImpersonation(t *testing.T) {
	isolateFromKubeconfig(t)

	want := "system:serviceaccount:ns:kdex-installer"
	if got := InstallerUser("ns"); got != want {
		t.Fatalf("InstallerUser = %q, want %q", got, want)
	}

	h, err := NewHelmClient("ns", baseRESTConfig(), nil, logr.Discard(), WithImpersonation(InstallerUser("ns")))
	if err != nil {
		t.Fatal(err)
	}
	action, kc := helmGetterConfigs(t, h)
	for what, cfg := range map[string]*rest.Config{"action": action, "kube client": kc} {
		if cfg.Host != "https://10.0.0.1:443" {
			t.Errorf("%s Host = %q, want https://10.0.0.1:443", what, cfg.Host)
		}
		if cfg.BearerToken != "x" {
			t.Errorf("%s BearerToken = %q, want the base config's", what, cfg.BearerToken)
		}
		if cfg.Impersonate.UserName != want {
			t.Errorf("%s Impersonate.UserName = %q, want %q", what, cfg.Impersonate.UserName, want)
		}
	}
}

// The operator client is built the same way, without impersonation -- and
// HELM_KUBEASUSER cannot add one.
func TestNewHelmClient_OperatorUsesTheBaseConfigUnimpersonated(t *testing.T) {
	isolateFromKubeconfig(t)
	t.Setenv("HELM_KUBEASUSER", "system:serviceaccount:kube-system:leaked")

	h, err := NewHelmClient("ns", baseRESTConfig(), nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	action, kc := helmGetterConfigs(t, h)
	for what, cfg := range map[string]*rest.Config{"action": action, "kube client": kc} {
		if cfg.Host != "https://10.0.0.1:443" {
			t.Errorf("%s Host = %q, want https://10.0.0.1:443", what, cfg.Host)
		}
		if cfg.Impersonate.UserName != "" || len(cfg.Impersonate.Groups) != 0 {
			t.Errorf("%s Impersonate = %+v, want none", what, cfg.Impersonate)
		}
	}
}

// Helm resolves a manifest's unset namespace from the getter's kubeconfig
// loader: it must name the client's namespace.
func TestNewHelmClient_LoaderNamesTheClientsNamespace(t *testing.T) {
	isolateFromKubeconfig(t)

	h, err := NewHelmClient("ns", baseRESTConfig(), nil, logr.Discard(), WithImpersonation(InstallerUser("ns")))
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
	if cfg.Host != "https://10.0.0.1:443" || cfg.Impersonate.UserName != InstallerUser("ns") {
		t.Errorf("loader config = %q as %q, want the base config impersonating the installer", cfg.Host, cfg.Impersonate.UserName)
	}
}

// The base config is the operator's own and is shared: building a client
// must not change it.
func TestNewHelmClient_LeavesTheBaseConfigAlone(t *testing.T) {
	isolateFromKubeconfig(t)
	base := baseRESTConfig()

	if _, err := NewHelmClient("ns", base, nil, logr.Discard(), WithImpersonation(InstallerUser("ns"))); err != nil {
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
