package controller

import (
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

func hostWithCompanions(name string, companionNames ...string) *kdexv1alpha1.KDexHost {
	companions := make([]kdexv1alpha1.CompanionChart, 0, len(companionNames))
	for _, n := range companionNames {
		companions = append(companions, kdexv1alpha1.CompanionChart{Name: n, Chart: "some-chart"})
	}
	return &kdexv1alpha1.KDexHost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: kdexv1alpha1.KDexHostSpec{
			Helm: &kdexv1alpha1.HelmConfig{CompanionCharts: companions},
		},
	}
}

// recorded marks releases as installed by this operator for the host, the way
// a completed helm reconcile records them.
func recorded(host *kdexv1alpha1.KDexHost, releases ...string) *kdexv1alpha1.KDexHost {
	if host.Status.Attributes == nil {
		host.Status.Attributes = map[string]string{}
	}
	host.Status.Attributes[AttributeCompanionReleases] = strings.Join(releases, ",")
	return host
}

// hostReleases names every KDexHost in the namespace, whose own releases carry
// the host's name.
func hostReleases(names ...string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// A companion dropped from the spec in the same edit that deletes the host is
// never seen by a spec-driven finalizer, so it outlives the CR with nothing
// left to reference it. Discovery by owner label is what makes it reachable.
func TestUninstallCompanionCharts_RemovesOwnedButUndeclaredRelease(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"declared-companion": {LabelCompanionOf: "my-host"},
		"ghost-companion":    {LabelCompanionOf: "my-host"},
	})

	r := &KDexHostReconciler{}
	host := recorded(hostWithCompanions("my-host", "declared-companion"), "declared-companion", "ghost-companion")
	if err := r.uninstallCompanionCharts(mock, host, hostReleases("my-host"), logr.Discard()); err != nil {
		t.Fatalf("uninstallCompanionCharts returned error: %v", err)
	}

	got := mock.Uninstalled()
	slices.Sort(got)
	want := []string{"declared-companion", "ghost-companion"}
	if !slices.Equal(got, want) {
		t.Errorf("uninstalled releases = %v, want %v", got, want)
	}
}

// The owner label carries the host name, so one host's teardown must not reach
// into another host's releases that happen to share the namespace.
func TestUninstallCompanionCharts_LeavesAnotherHostsReleases(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"mine":     {LabelCompanionOf: "my-host"},
		"theirs":   {LabelCompanionOf: "other-host"},
		"unlabels": {},
	})

	r := &KDexHostReconciler{}
	if err := r.uninstallCompanionCharts(mock, recorded(hostWithCompanions("my-host"), "mine"), hostReleases("my-host"), logr.Discard()); err != nil {
		t.Fatalf("uninstallCompanionCharts returned error: %v", err)
	}

	if !slices.Equal(mock.Uninstalled(), []string{"mine"}) {
		t.Errorf("uninstalled releases = %v, want [mine]", mock.Uninstalled())
	}
}

// A listing failure must not be swallowed: reporting success while releases
// survive is what turns a transient API error into a permanent orphan.
func TestUninstallCompanionCharts_PropagatesListFailure(t *testing.T) {
	mock := NewMockHelmClient(nil)
	mock.SetFailList(true)

	r := &KDexHostReconciler{}
	err := r.uninstallCompanionCharts(mock, hostWithCompanions("my-host", "declared-companion"), hostReleases("my-host"), logr.Discard())
	if err == nil {
		t.Fatal("expected an error when listing releases fails, got nil")
	}
}

// Teardown must not report success while releases survive. Swallowing the
// error and removing the finalizer anyway leaves the releases running with no
// CR left to reference them -- the same permanent orphan #36 exists to prevent,
// arrived at from the deletion path instead.
func TestUninstallCompanionCharts_PropagatesUninstallFailure(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"healthy": {LabelCompanionOf: "my-host"},
		"stuck":   {LabelCompanionOf: "my-host"},
	})
	mock.SetFailUninstall("stuck")

	r := &KDexHostReconciler{}
	err := r.uninstallCompanionCharts(mock, recorded(hostWithCompanions("my-host"), "healthy", "stuck"), hostReleases("my-host"), logr.Discard())

	if err == nil {
		t.Fatal("expected an error when a companion uninstall fails, got nil")
	}
	if !strings.Contains(err.Error(), "stuck") {
		t.Errorf("error %q does not name the release that failed", err)
	}
	// The other release must still have been attempted: one stuck companion
	// cannot be allowed to strand the rest.
	if !slices.Contains(mock.Uninstalled(), "healthy") {
		t.Errorf("uninstalled = %v, want it to still include healthy", mock.Uninstalled())
	}
}

// kdex-tech/nexus-manager#46, re-parenting: anyone who can label a Secret in
// the namespace can put the companion-of label on a release's Helm record. The
// label alone must never make a release this host's to uninstall -- only the
// operator's own record of what it installed does.
func TestUninstallCompanionCharts_LabelAloneNamesNoTarget(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"mine":       {LabelCompanionOf: "my-host"},
		"relabelled": {LabelCompanionOf: "my-host"},
	})

	r := &KDexHostReconciler{}
	if err := r.uninstallCompanionCharts(mock, recorded(hostWithCompanions("my-host"), "mine"), hostReleases("my-host"), logr.Discard()); err != nil {
		t.Fatalf("uninstallCompanionCharts returned error: %v", err)
	}
	if !slices.Equal(mock.Uninstalled(), []string{"mine"}) {
		t.Errorf("uninstalled releases = %v, want [mine]", mock.Uninstalled())
	}
}

func TestPruneCompanionCharts_LabelAloneNamesNoTarget(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"dropped":    {LabelCompanionOf: "my-host"},
		"relabelled": {LabelCompanionOf: "my-host"},
	})

	r := &KDexHostReconciler{}
	pruned, _, err := r.pruneCompanionCharts(mock, recorded(hostWithCompanions("my-host"), "dropped"), hostReleases("my-host"), logr.Discard())
	if err != nil {
		t.Fatalf("pruneCompanionCharts returned error: %v", err)
	}
	if !slices.Equal(mock.Uninstalled(), []string{"dropped"}) {
		t.Errorf("uninstalled releases = %v, want [dropped]", mock.Uninstalled())
	}
	if !slices.Equal(pruned, []string{"dropped"}) {
		t.Errorf("pruned = %v, want [dropped]", pruned)
	}
}

// A recorded release whose owner label is gone is no longer one the label
// listing finds: someone else has taken it over, so it is left alone too.
func TestPruneCompanionCharts_RecordAloneNamesNoTarget(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"taken-over": {LabelCompanionOf: "other-host"},
	})

	r := &KDexHostReconciler{}
	pruned, _, err := r.pruneCompanionCharts(mock, recorded(hostWithCompanions("my-host"), "taken-over"), hostReleases("my-host", "other-host"), logr.Discard())
	if err != nil {
		t.Fatalf("pruneCompanionCharts returned error: %v", err)
	}
	if len(mock.Uninstalled()) != 0 || len(pruned) != 0 {
		t.Errorf("uninstalled = %v, pruned = %v, want neither", mock.Uninstalled(), pruned)
	}
}

// A KDexHost's own release is named after the host. A companion of the same
// name would be installed over that release, and uninstalled with it: none is
// ever acted on, whatever the spec, the record or the labels say.
func TestCompanionCharts_NeverTouchAKDexHostRelease(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"other-host": {LabelCompanionOf: "my-host"},
		"my-host":    {LabelCompanionOf: "my-host"},
	})
	host := recorded(hostWithCompanions("my-host", "other-host"), "other-host", "my-host")
	hosts := hostReleases("my-host", "other-host")
	r := &KDexHostReconciler{}

	if _, _, err := r.pruneCompanionCharts(mock, recorded(hostWithCompanions("my-host"), "other-host", "my-host"), hosts, logr.Discard()); err != nil {
		t.Fatalf("pruneCompanionCharts returned error: %v", err)
	}
	if err := r.uninstallCompanionCharts(mock, host, hosts, logr.Discard()); err != nil {
		t.Fatalf("uninstallCompanionCharts returned error: %v", err)
	}
	if len(mock.Uninstalled()) != 0 {
		t.Errorf("uninstalled = %v, want no KDexHost release touched", mock.Uninstalled())
	}

	err := checkCompanionNames(hostWithCompanions("my-host", "ok", "other-host"), hosts)
	if err == nil || !strings.Contains(err.Error(), "other-host") {
		t.Errorf("checkCompanionNames = %v, want an error naming other-host", err)
	}
	if err := checkCompanionNames(hostWithCompanions("my-host", "ok"), hosts); err != nil {
		t.Errorf("checkCompanionNames = %v, want nil", err)
	}
}

// The record a run writes is ((recorded + installed) - pruned), kept only
// where the spec declares a name or a release labelled as the host's carries
// it, applied to the record as the server holds it rather than as the run
// first read it.
func TestNextCompanionRecord(t *testing.T) {
	got := nextCompanionRecord(
		// "other" was recorded by an overlapping run this one never saw.
		map[string]struct{}{"a": {}, "dropped": {}, "failed": {}, "redeclared": {}, "other": {}},
		companionRecordChange{
			added:  []string{"b", "a"},
			pruned: []string{"dropped"},
			stale:  []string{"failed", "redeclared"},
		},
		// The spec as it is now declares "redeclared" again.
		map[string]struct{}{"a": {}, "b": {}, "redeclared": {}},
	)
	if want := "a,b,other,redeclared"; got != want {
		t.Errorf("nextCompanionRecord = %q, want %q", got, want)
	}
	stale := staleCompanions(
		map[string]struct{}{"a": {}, "labelled": {}, "failed": {}},
		map[string]struct{}{"a": {}},
		map[string]struct{}{"labelled": {}},
	)
	if !slices.Equal(stale, []string{"failed"}) {
		t.Errorf("staleCompanions = %v, want [failed]", stale)
	}
	if got := recordedCompanions(recorded(hostWithCompanions("h"), "a", "b")); len(got) != 2 {
		t.Errorf("recordedCompanions = %v, want a and b", got)
	}
	if got := recordedCompanions(hostWithCompanions("h")); len(got) != 0 {
		t.Errorf("recordedCompanions of an unrecorded host = %v, want none", got)
	}
}

// kdex-tech/nexus-manager#46: a declared name is not a release this operator
// installed. Its install may have failed, or the host may be deleted before
// the run that would install it -- and then anyone who can write a Secret in
// the namespace can forge a release under that name, with the host's label,
// for teardown to uninstall. Teardown removes only what the record names.
func TestUninstallCompanionCharts_DeclaredAloneNamesNoTarget(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"mine":   {LabelCompanionOf: "my-host"},
		"forged": {LabelCompanionOf: "my-host"},
	})

	r := &KDexHostReconciler{}
	host := recorded(hostWithCompanions("my-host", "mine", "forged", "never-installed"), "mine")
	if err := r.uninstallCompanionCharts(mock, host, hostReleases("my-host"), logr.Discard()); err != nil {
		t.Fatalf("uninstallCompanionCharts returned error: %v", err)
	}
	if !slices.Equal(mock.Uninstalled(), []string{"mine"}) {
		t.Errorf("uninstalled releases = %v, want [mine]", mock.Uninstalled())
	}
}

// A host whose companions were installed before the record existed, and that
// is deleted before any reconcile could seed its record, would otherwise leave
// every companion behind. With no record at all, the spec stands in for it.
func TestUninstallCompanionCharts_TreatsAnAbsentRecordAsTheSpec(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"legacy":     {LabelCompanionOf: "my-host"},
		"relabelled": {LabelCompanionOf: "my-host"},
	})

	r := &KDexHostReconciler{}
	if err := r.uninstallCompanionCharts(mock, hostWithCompanions("my-host", "legacy"), hostReleases("my-host"), logr.Discard()); err != nil {
		t.Fatalf("uninstallCompanionCharts returned error: %v", err)
	}
	if !slices.Equal(mock.Uninstalled(), []string{"legacy"}) {
		t.Errorf("uninstalled releases = %v, want [legacy]", mock.Uninstalled())
	}
}
