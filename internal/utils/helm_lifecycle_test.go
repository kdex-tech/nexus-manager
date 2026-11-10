package utils

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/action"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/cli"
	kubefake "helm.sh/helm/v4/pkg/kube/fake"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
)

// memoryHelmClient is a HelmClient over in-memory release storage and a kube
// client that applies nothing.
func memoryHelmClient(t *testing.T) *HelmClient {
	t.Helper()
	cfg := action.NewConfiguration()
	cfg.KubeClient = &kubefake.PrintingKubeClient{Out: io.Discard, LogOutput: io.Discard}
	cfg.Releases = storage.Init(driver.NewMemory())
	cfg.Capabilities = chartcommon.DefaultCapabilities
	return &HelmClient{actionConfig: cfg, log: logr.Discard(), namespace: "ns", settings: cli.New()}
}

// localChart writes a minimal chart to a directory, which LocateChart resolves
// without any network.
func localChart(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "c")
	if err := os.MkdirAll(filepath.Join(dir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"Chart.yaml":        "apiVersion: v2\nname: c\nversion: 0.1.0\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Uninstall purges a release, so a teardown that retries after one release of
// several failed finds the others already gone. Were that an error, the retry
// would fail on the release it had already removed, and the host's finalizer
// would be held forever. A release that does not exist is uninstalled.
func TestHelmClient_UninstallOfAMissingReleaseSucceeds(t *testing.T) {
	h := memoryHelmClient(t)
	if err := h.Uninstall("never-installed"); err != nil {
		t.Errorf("Uninstall of a release that does not exist = %v, want nil", err)
	}
}

// A chart fetch can outlast the teardown's wait for it. Whatever the fetch
// brought back, nothing is installed or upgraded unless the operation is still
// wanted once the chart is in hand.
func TestHelmClient_InstallsNothingOnceProceedRefuses(t *testing.T) {
	h := memoryHelmClient(t)
	refused := errors.New("host is terminating")
	chartDir := localChart(t)

	if err := h.install(&ChartSpec{ReleaseName: "r", ChartName: chartDir, Namespace: "ns",
		Proceed: func() error { return refused }}); !errors.Is(err, refused) {
		t.Errorf("install = %v, want the refusal", err)
	}
	if _, err := h.actionConfig.Releases.Last("r"); err == nil {
		t.Fatal("install created a release after proceed refused")
	}

	if err := h.install(&ChartSpec{ReleaseName: "r", ChartName: chartDir, Namespace: "ns"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := h.upgrade(&ChartSpec{ReleaseName: "r", ChartName: chartDir, Namespace: "ns",
		Proceed: func() error { return refused }}); !errors.Is(err, refused) {
		t.Errorf("upgrade = %v, want the refusal", err)
	}
	if rel, err := h.actionConfig.Releases.Last("r"); err != nil {
		t.Fatal(err)
	} else if v := releaseVersion(t, rel); v != 1 {
		t.Errorf("release is at revision %d after a refused upgrade, want 1", v)
	}
}

func releaseVersion(t *testing.T, rel release.Releaser) int {
	t.Helper()
	a, err := release.NewAccessor(rel)
	if err != nil {
		t.Fatal(err)
	}
	return a.Version()
}
