package controller

import (
	"slices"
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

// A companion dropped from the spec in the same edit that deletes the host is
// never seen by a spec-driven finalizer, so it outlives the CR with nothing
// left to reference it. Discovery by owner label is what makes it reachable.
func TestUninstallCompanionCharts_RemovesOwnedButUndeclaredRelease(t *testing.T) {
	mock := &MockHelmClient{
		ReleaseLabels: map[string]map[string]string{
			"declared-companion": {LabelCompanionOf: "my-host"},
			"ghost-companion":    {LabelCompanionOf: "my-host"},
		},
	}

	r := &KDexHostReconciler{}
	if err := r.uninstallCompanionCharts(mock, hostWithCompanions("my-host", "declared-companion"), logr.Discard()); err != nil {
		t.Fatalf("uninstallCompanionCharts returned error: %v", err)
	}

	got := slices.Clone(mock.UninstalledCharts)
	slices.Sort(got)
	want := []string{"declared-companion", "ghost-companion"}
	if !slices.Equal(got, want) {
		t.Errorf("uninstalled releases = %v, want %v", got, want)
	}
}

// The owner label carries the host name, so one host's teardown must not reach
// into another host's releases that happen to share the namespace.
func TestUninstallCompanionCharts_LeavesAnotherHostsReleases(t *testing.T) {
	mock := &MockHelmClient{
		ReleaseLabels: map[string]map[string]string{
			"mine":     {LabelCompanionOf: "my-host"},
			"theirs":   {LabelCompanionOf: "other-host"},
			"unlabels": {},
		},
	}

	r := &KDexHostReconciler{}
	if err := r.uninstallCompanionCharts(mock, hostWithCompanions("my-host"), logr.Discard()); err != nil {
		t.Fatalf("uninstallCompanionCharts returned error: %v", err)
	}

	if !slices.Equal(mock.UninstalledCharts, []string{"mine"}) {
		t.Errorf("uninstalled releases = %v, want [mine]", mock.UninstalledCharts)
	}
}

// A listing failure must not be swallowed: reporting success while releases
// survive is what turns a transient API error into a permanent orphan.
func TestUninstallCompanionCharts_PropagatesListFailure(t *testing.T) {
	mock := &MockHelmClient{FailList: true}

	r := &KDexHostReconciler{}
	err := r.uninstallCompanionCharts(mock, hostWithCompanions("my-host", "declared-companion"), logr.Discard())
	if err == nil {
		t.Fatal("expected an error when listing releases fails, got nil")
	}
}
