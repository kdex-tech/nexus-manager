// Migration code: delete in release B (privilege-redesign follow-up issue).
// Every test in this file exercises the legacy RBAC migration.

package utils

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/release/common"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
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

// revision reads one stored revision of my-host back.
func revision(t *testing.T, h *HelmClient, version int) *rspb.Release {
	t.Helper()
	r, err := h.actionConfig.Releases.Get("my-host", version)
	if err != nil {
		t.Fatal(err)
	}
	rel, ok := r.(*rspb.Release)
	if !ok {
		t.Fatalf("revision %d is a %T", version, r)
	}
	return rel
}

// kinds lists the kind/name of every document in manifest, in Helm's order.
func kinds(t *testing.T, manifest string) []string {
	t.Helper()
	docs := releaseutil.SplitManifests(manifest)
	keys := make([]string, 0, len(docs))
	for k := range docs {
		keys = append(keys, k)
	}
	sort.Sort(releaseutil.BySplitManifestsOrder(keys))
	var out []string
	for _, k := range keys {
		var kind, name string
		for line := range strings.SplitSeq(docs[k], "\n") {
			if v, ok := strings.CutPrefix(line, "kind: "); ok {
				kind = v
			}
			if v, ok := strings.CutPrefix(line, "  name: "); ok && name == "" {
				name = v
			}
		}
		if kind != "" {
			out = append(out, kind+"/"+name)
		}
	}
	return out
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
rules: []
---
# Source: host-manager/templates/clusterrolebinding.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: my-host-ns
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: my-host
subjects:
- kind: ServiceAccount
  name: my-host
  namespace: ns
---
# Source: host-manager/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-host
---
# Source: host-manager/templates/NOTES-only.yaml
# nothing rendered
`

// forgedManifest is a record rewritten by someone who writes Secrets in the
// namespace: a document whose top-level kind and name are the host's legacy
// ClusterRole, carrying an items list that Helm's uninstall would flatten and
// delete -- a Secret in another namespace and a system ClusterRoleBinding --
// and a plain List doing the same.
const forgedManifest = `---
apiVersion: v1
kind: ServiceAccount
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
    namespace: other-tenant
- apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRoleBinding
  metadata:
    name: system:basic-user
---
apiVersion: v1
kind: List
metadata:
  name: also-forged
items:
- apiVersion: v1
  kind: Secret
  metadata:
    name: victim
    namespace: other-tenant
---
apiVersion: v1
kind: ConfigMapList
items:
- apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cm
    namespace: other-tenant
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

// The legacy RBAC documents leave the latest revision's record -- nexus deletes
// the objects itself -- and everything else in it stays, in order, as does
// the revision's status.
func TestHelmClient_DropClusterRBACFromRecordRewritesTheLatestRevision(t *testing.T) {
	h := memoryHelmClient(t)
	storeRevision(t, h, 1, common.StatusSuperseded, legacyManifest)
	storeRevision(t, h, 2, common.StatusDeployed, legacyManifest)

	dropped, err := h.DropClusterRBACFromRecord("my-host")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ClusterRole/my-host", "ClusterRoleBinding/my-host-ns"}; !slices.Equal(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
	latest := revision(t, h, 2)
	if got, want := kinds(t, latest.Manifest), []string{"ServiceAccount/my-host", "Deployment/my-host"}; !slices.Equal(got, want) {
		t.Errorf("latest record lists %v, want %v", got, want)
	}
	if latest.Info.Status != common.StatusDeployed {
		t.Errorf("latest status = %s, want it kept", latest.Info.Status)
	}
	// Only the revision an uninstall acts on is rewritten.
	if got := kinds(t, revision(t, h, 1).Manifest); !slices.Contains(got, "ClusterRole/my-host") {
		t.Errorf("revision 1 lists %v, want it untouched", got)
	}
}

// A forged record cannot smuggle objects past the rewrite inside a document
// that looks like legacy RBAC, or inside any list: every list-typed document
// goes, so no uninstall ever flattens one.
func TestHelmClient_DropClusterRBACFromRecordDropsListDocuments(t *testing.T) {
	h := memoryHelmClient(t)
	storeRevision(t, h, 1, common.StatusDeployed, forgedManifest)

	if _, err := h.DropClusterRBACFromRecord("my-host"); err != nil {
		t.Fatal(err)
	}
	manifest := revision(t, h, 1).Manifest
	if got, want := kinds(t, manifest), []string{"ServiceAccount/my-host"}; !slices.Equal(got, want) {
		t.Errorf("record lists %v, want %v", got, want)
	}
	for _, s := range []string{"victim", "system:basic-user", "items"} {
		if strings.Contains(manifest, s) {
			t.Errorf("rewritten record still holds %q:\n%s", s, manifest)
		}
	}
}

// What the installer cannot delete and is not legacy RBAC stays in the
// record: its uninstall then fails closed on Forbidden, rather than the
// rewrite quietly forgetting it.
func TestHelmClient_DropClusterRBACFromRecordKeepsOtherKinds(t *testing.T) {
	const manifest = `---
apiVersion: kdex.dev/v1alpha1
kind: KDexClusterTheme
metadata:
  name: shared
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: coredns
  namespace: kube-system
`
	h := memoryHelmClient(t)
	storeRevision(t, h, 1, common.StatusDeployed, manifest)

	dropped, err := h.DropClusterRBACFromRecord("my-host")
	if err != nil || len(dropped) != 0 {
		t.Fatalf("DropClusterRBACFromRecord = %v, %v; want nothing dropped", dropped, err)
	}
	if got, want := kinds(t, revision(t, h, 1).Manifest), []string{"KDexClusterTheme/shared", "Deployment/coredns"}; !slices.Equal(got, want) {
		t.Errorf("record lists %v, want %v", got, want)
	}
}

// updateCounting counts the record writes a storage driver is asked for.
type updateCounting struct {
	*driver.Memory
	updates int
}

func (u *updateCounting) Update(key string, rls release.Releaser) error {
	u.updates++
	return u.Memory.Update(key, rls)
}

// A record with nothing to drop is not written: a teardown's rewrite of an
// already-redesigned release costs one read.
func TestHelmClient_DropClusterRBACFromRecordLeavesACleanRecordAlone(t *testing.T) {
	h := memoryHelmClient(t)
	counting := &updateCounting{Memory: driver.NewMemory()}
	h.actionConfig.Releases = storage.Init(counting)
	storeRevision(t, h, 1, common.StatusDeployed, redesignedManifest)

	dropped, err := h.DropClusterRBACFromRecord("my-host")
	if err != nil || len(dropped) != 0 {
		t.Fatalf("DropClusterRBACFromRecord = %v, %v; want nothing dropped", dropped, err)
	}
	if counting.updates != 0 {
		t.Errorf("%d record writes, want none", counting.updates)
	}
	if revision(t, h, 1).Manifest != redesignedManifest {
		t.Error("a clean record was changed")
	}
}

func TestHelmClient_DropClusterRBACFromRecordOfAMissingRelease(t *testing.T) {
	h := memoryHelmClient(t)
	if dropped, err := h.DropClusterRBACFromRecord("never-installed"); err != nil || len(dropped) != 0 {
		t.Errorf("DropClusterRBACFromRecord = %v, %v; want nothing, nil", dropped, err)
	}
}

// A failed lookup says nothing about the record: it is an error, and the
// teardown keeps its finalizer.
func TestHelmClient_DropClusterRBACFromRecordReportsAFailedLookup(t *testing.T) {
	h := memoryHelmClient(t)
	unavailable := errors.New("the server is currently unable to handle the request")
	h.actionConfig.Releases = storage.Init(&failingQueries{Memory: driver.NewMemory(), err: unavailable})
	if _, err := h.DropClusterRBACFromRecord("my-host"); !errors.Is(err, unavailable) {
		t.Errorf("DropClusterRBACFromRecord with a failing lookup = %v, want that failure", err)
	}
}

// The upgrade proof reads the status of the release's latest revision.
func TestHelmClient_ReleaseStatusIsTheLatestRevisions(t *testing.T) {
	h := memoryHelmClient(t)
	storeRevision(t, h, 1, common.StatusDeployed, redesignedManifest)
	storeRevision(t, h, 2, common.StatusPendingUpgrade, redesignedManifest)
	if got, err := h.ReleaseStatus("my-host"); err != nil || got != string(common.StatusPendingUpgrade) {
		t.Errorf("ReleaseStatus = %q, %v; want pending-upgrade", got, err)
	}
	if got, err := h.ReleaseStatus("never-installed"); err != nil || got != "" {
		t.Errorf("ReleaseStatus of a missing release = %q, %v; want empty, nil", got, err)
	}

	unavailable := errors.New("the server is currently unable to handle the request")
	h.actionConfig.Releases = storage.Init(&failingQueries{Memory: driver.NewMemory(), err: unavailable})
	if _, err := h.ReleaseStatus("my-host"); !errors.Is(err, unavailable) {
		t.Errorf("ReleaseStatus with a failing lookup = %v, want that failure", err)
	}
}
