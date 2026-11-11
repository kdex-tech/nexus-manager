package utils

import (
	"errors"
	"slices"
	"testing"

	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"
)

func storeRevision(t *testing.T, h *HelmClient, version int, status common.Status, manifest string) {
	t.Helper()
	if err := h.actionConfig.Releases.Create(&rspb.Release{
		Name:      "my-host",
		Namespace: "ns",
		Version:   version,
		Info:      &rspb.Info{Status: status},
		Manifest:  manifest,
	}); err != nil {
		t.Fatal(err)
	}
}

const legacyManifest = `---
# Source: host-manager/templates/serviceaccount.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: my-host
---
# Source: host-manager/templates/clusterrole.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: my-host
---
# Source: host-manager/templates/clusterrolebinding.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: my-host-ns
---
# Source: host-manager/templates/NOTES-only.yaml
# nothing rendered
`

const redesignedManifest = `---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: my-host
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: my-host
  namespace: ns
`

// The record listed is the latest revision's, whatever its status: an
// uninstall that failed part-way leaves it "uninstalling".
func TestHelmClient_ReleaseObjectsReadsTheLatestRevision(t *testing.T) {
	h := memoryHelmClient(t)
	storeRevision(t, h, 1, common.StatusSuperseded, redesignedManifest)
	storeRevision(t, h, 2, common.StatusUninstalling, legacyManifest)

	got, err := h.ReleaseObjects("my-host")
	if err != nil {
		t.Fatal(err)
	}
	want := []ReleaseObject{
		{Kind: "ServiceAccount", Name: "my-host"},
		{Kind: "ClusterRole", Name: "my-host"},
		{Kind: "ClusterRoleBinding", Name: "my-host-ns"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReleaseObjects = %v, want %v", got, want)
	}
}

func TestHelmClient_ReleaseObjectsKeepsTheStatedNamespace(t *testing.T) {
	h := memoryHelmClient(t)
	storeRevision(t, h, 1, common.StatusDeployed, redesignedManifest)

	got, err := h.ReleaseObjects("my-host")
	if err != nil {
		t.Fatal(err)
	}
	want := []ReleaseObject{
		{Kind: "ServiceAccount", Name: "my-host"},
		{Kind: "RoleBinding", Name: "my-host", Namespace: "ns"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ReleaseObjects = %v, want %v", got, want)
	}
}

func TestHelmClient_ReleaseObjectsOfAMissingReleaseIsEmpty(t *testing.T) {
	h := memoryHelmClient(t)
	got, err := h.ReleaseObjects("never-installed")
	if err != nil || len(got) != 0 {
		t.Errorf("ReleaseObjects = %v, %v; want nothing, nil", got, err)
	}
}

// A failed lookup says nothing about the record: it is an error, not an empty
// record that would pick the installer for a release it cannot uninstall.
func TestHelmClient_ReleaseObjectsReportsAFailedLookup(t *testing.T) {
	h := memoryHelmClient(t)
	unavailable := errors.New("the server is currently unable to handle the request")
	h.actionConfig.Releases = storage.Init(&failingQueries{Memory: driver.NewMemory(), err: unavailable})
	if _, err := h.ReleaseObjects("my-host"); !errors.Is(err, unavailable) {
		t.Errorf("ReleaseObjects with a failing lookup = %v, want that failure", err)
	}
}
