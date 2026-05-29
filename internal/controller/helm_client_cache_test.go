package controller

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// stubHelmClient is a no-op HelmClientInterface used to identify cache entries.
type stubHelmClient struct{ id int }

func (s *stubHelmClient) AddRepository(string, string) error      { return nil }
func (s *stubHelmClient) InstallOrUpgrade(*utils.ChartSpec) error { return nil }
func (s *stubHelmClient) Uninstall(string) error                  { return nil }

func secretsWithPassword(pw string) kdexv1alpha1.Secrets {
	return kdexv1alpha1.Secrets{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns"},
			Data:       map[string][]byte{"password": []byte(pw)},
		},
	}
}

// TestGetOrCreateHelmClientRefreshesOnSecretChange is a regression test for
// https://github.com/kdex-tech/nexus-manager/issues/9 - the cached Helm client
// was keyed only by name+namespace and captured the secrets at creation time,
// so rotated registry credentials were never picked up.
func TestGetOrCreateHelmClientRefreshesOnSecretChange(t *testing.T) {
	var calls int
	r := &KDexHostReconciler{
		HelmClientFactory: func(_ string, _ kdexv1alpha1.Secrets, _ logr.Logger) (utils.HelmClientInterface, error) {
			calls++
			return &stubHelmClient{id: calls}, nil
		},
	}

	old := secretsWithPassword("old-password")

	c1, err := r.getOrCreateHelmClient("host", "ns", old, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected factory to be called once, got %d", calls)
	}

	// Same secrets -> cached client is reused, factory not called again.
	c1b, err := r.getOrCreateHelmClient("host", "ns", old, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected cached client to be reused (calls=1), got %d", calls)
	}
	if c1 != c1b {
		t.Fatalf("expected the same cached client instance for unchanged secrets")
	}

	// Rotated credentials -> a fresh client must be built.
	rotated := secretsWithPassword("new-password")
	c2, err := r.getOrCreateHelmClient("host", "ns", rotated, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected factory to be called again after credential rotation (calls=2), got %d", calls)
	}
	if c1 == c2 {
		t.Fatalf("expected a fresh client after credential rotation, got the stale cached one")
	}
}
