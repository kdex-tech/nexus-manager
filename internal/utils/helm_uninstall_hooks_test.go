package utils

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/action"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/kube"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
)

// buildRecorder is a fake kube client that remembers every manifest Helm asks
// it to build. Helm builds a hook's manifest only to run the hook, so a hook
// manifest in this list means the hook ran.
type buildRecorder struct {
	*kubefake.PrintingKubeClient
	mu    sync.Mutex
	built []string
}

func (b *buildRecorder) Build(r io.Reader, validate bool) (kube.ResourceList, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.built = append(b.built, string(data))
	b.mu.Unlock()
	return b.PrintingKubeClient.Build(strings.NewReader(string(data)), validate)
}

// Helm keeps a release's record in a Secret in the release's namespace, so
// anyone who can write Secrets there can forge one whose pre-delete hook is,
// say, a ClusterRoleBinding. Uninstall runs as the operator, which may create
// ClusterRoleBindings: running a release's hooks hands that power to whoever
// last wrote its Secret. None of the releases this operator manages relies on
// a delete hook, so it never runs one (kdex-tech/nexus-manager#46).
func TestHelmClient_UninstallRunsNoReleaseHooks(t *testing.T) {
	const hook = "kind: ClusterRoleBinding\nmetadata:\n  name: forged-hook\n"
	kc := &buildRecorder{PrintingKubeClient: &kubefake.PrintingKubeClient{Out: io.Discard, LogOutput: io.Discard}}
	cfg := action.NewConfiguration()
	cfg.KubeClient = kc
	cfg.Releases = storage.Init(driver.NewMemory())
	if err := cfg.Releases.Create(&rspb.Release{
		Name:      "companion",
		Namespace: "ns",
		Version:   1,
		Chart:     &chart.Chart{Metadata: &chart.Metadata{Name: "c", Version: "0.1.0"}},
		Info:      &rspb.Info{Status: common.StatusDeployed, LastDeployed: time.Now()},
		Manifest:  "kind: ConfigMap\nmetadata:\n  name: cm\n",
		Hooks: []*rspb.Hook{{
			Name: "forged-hook", Kind: "ClusterRoleBinding", Path: "templates/hook.yaml",
			Manifest: hook, Events: []rspb.HookEvent{rspb.HookPreDelete, rspb.HookPostDelete},
		}},
	}); err != nil {
		t.Fatalf("seed release: %v", err)
	}

	h := &HelmClient{actionConfig: cfg, log: logr.Discard(), namespace: "ns"}
	if err := h.Uninstall("companion"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	for _, m := range kc.built {
		if strings.Contains(m, "forged-hook") {
			t.Fatalf("Uninstall ran a delete hook of the release: built %q", m)
		}
	}
	if len(kc.built) == 0 {
		t.Fatal("Uninstall built nothing: the test is not exercising the release's manifest")
	}
}
