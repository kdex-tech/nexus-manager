package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
)

func hashReconciler(t *testing.T) *KDexHostReconciler {
	t.Helper()
	// The Ginkgo suite registers these in BeforeSuite, which does not run for a
	// plain `go test -run`. AddToScheme is idempotent.
	if err := configuration.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return &KDexHostReconciler{
		Configuration: configuration.LoadConfiguration("/config.yaml", scheme.Scheme),
		Scheme:        scheme.Scheme,
	}
}

func mustHash(t *testing.T, r *KDexHostReconciler, host *kdexv1alpha1.KDexHost) string {
	t.Helper()
	h, err := r.computeHelmReleaseHash(host)
	if err != nil {
		t.Fatalf("computeHelmReleaseHash: %v", err)
	}
	return h
}

func baseHost() *kdexv1alpha1.KDexHost {
	return &kdexv1alpha1.KDexHost{
		ObjectMeta: metav1.ObjectMeta{Name: "a-host", Namespace: "ns"},
		Spec: kdexv1alpha1.KDexHostSpec{
			BrandName:    "Before",
			Organization: "Org",
			Routing:      kdexv1alpha1.Routing{Domains: []string{"kdex.dev"}},
		},
	}
}

// The release hash gates an expensive chart pull + render. Host spec fields
// that never reach the Helm values must not invalidate it, or routine edits
// (rebrand, add a domain, point at a different theme) each pay for a full
// re-render that cannot change a single rendered byte.
func TestHelmReleaseHash_IgnoresFieldsThatDoNotReachTheRelease(t *testing.T) {
	r := hashReconciler(t)

	before := baseHost()
	after := baseHost()
	after.Spec.BrandName = "After"
	after.Spec.Organization = "Another Org"
	after.Spec.Routing.Domains = []string{"kdex.dev", "extra.kdex.dev"}
	after.Spec.ThemeRef = &kdexv1alpha1.KDexObjectReference{Name: "some-theme"}

	if got, want := mustHash(t, r, after), mustHash(t, r, before); got != want {
		t.Errorf("hash changed for fields that never reach the Helm release:\n got  %s\n want %s", got, want)
	}
}

// The guard against over-narrowing: per-host chart values obviously feed the
// render, so they must still invalidate.
func TestHelmReleaseHash_ChangesWithHostManagerValues(t *testing.T) {
	r := hashReconciler(t)

	before := baseHost()
	after := baseHost()
	after.Spec.Helm = &kdexv1alpha1.HelmConfig{
		HostManager: &kdexv1alpha1.HostManagerHelmConfig{Values: "valkey:\n  enabled: false\n"},
	}

	if mustHash(t, r, after) == mustHash(t, r, before) {
		t.Error("hash did not change when spec.helm.hostManager.values changed")
	}
}

// Companion charts are reconciled and pruned inside the same hash-gated pass,
// so a companion added or removed must invalidate. Without this the prune
// added for #36 would never run on the edit that removes one.
func TestHelmReleaseHash_ChangesWithCompanionCharts(t *testing.T) {
	r := hashReconciler(t)

	before := baseHost()
	before.Spec.Helm = &kdexv1alpha1.HelmConfig{
		CompanionCharts: []kdexv1alpha1.CompanionChart{
			{Name: "a", Chart: "chart-a"},
			{Name: "b", Chart: "chart-b"},
		},
	}
	after := baseHost()
	after.Spec.Helm = &kdexv1alpha1.HelmConfig{
		CompanionCharts: []kdexv1alpha1.CompanionChart{
			{Name: "a", Chart: "chart-a"},
		},
	}

	if mustHash(t, r, after) == mustHash(t, r, before) {
		t.Error("hash did not change when a companion chart was removed")
	}
}

// The whole NexusConfiguration is handed to every host as .Values.config and
// the host-manager chart renders it into a ConfigMap, so a cluster-default
// edit genuinely changes every host's manifest. This is a characterization
// test: the fleet-wide invalidation is real, not a hashing artifact.
func TestHelmReleaseHash_ChangesWithNexusConfiguration(t *testing.T) {
	r := hashReconciler(t)
	host := baseHost()

	before := mustHash(t, r, host)

	r.Configuration.HostDefault.Chart.Version = "9.9.9-changed"

	if mustHash(t, r, host) == before {
		t.Error("hash did not change when the cluster-wide NexusConfiguration changed")
	}
}
