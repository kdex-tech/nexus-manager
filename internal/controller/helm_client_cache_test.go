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

func (s *stubHelmClient) InstallOrUpgrade(*utils.ChartSpec) error { return nil }
func (s *stubHelmClient) Uninstall(string) error                  { return nil }

func (s *stubHelmClient) ListReleases(map[string]string) ([]string, error) { return nil, nil }

// secretsWithPassword is the registry Secret at resourceVersion, as the cache
// returns it: owned by the controller that keeps it, whose deletion it blocks
// (pointer fields, as External Secrets sets them), and freshly copied.
func secretsWithPassword(resourceVersion, pw string) kdexv1alpha1.Secrets {
	yes := true
	return kdexv1alpha1.Secrets{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "creds", Namespace: "ns", ResourceVersion: resourceVersion,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "external-secrets.io/v1", Kind: "ExternalSecret", Name: "creds", UID: "u",
					Controller: &yes, BlockOwnerDeletion: &yes,
				}},
			},
			Data: map[string][]byte{"password": []byte(pw)},
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

	old := secretsWithPassword("1", "old-password")

	c1, err := r.getOrCreateHelmClient("host", "ns", old, logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected factory to be called once, got %d", calls)
	}

	// Same secrets, read again -> cached client is reused, factory not called
	// again. Every reconcile reads a fresh copy of the host's Secrets.
	c1b, err := r.getOrCreateHelmClient("host", "ns", secretsWithPassword("1", "old-password"), logr.Discard())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected cached client to be reused (calls=1), got %d", calls)
	}
	if c1 != c1b {
		t.Fatalf("expected the same cached client instance for unchanged secrets")
	}

	// Rotated credentials -> a fresh client must be built. Updating the
	// Secret bumped its resourceVersion.
	rotated := secretsWithPassword("2", "new-password")
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

// A Helm client is bound to its host's namespace and release storage. Host
// "a-b" in namespace "c" and host "a" in namespace "b-c" are different hosts,
// whatever their names look like joined up.
func TestGetOrCreateHelmClientKeepsHostsInDifferentNamespacesApart(t *testing.T) {
	var namespaces []string
	r := &KDexHostReconciler{
		HelmClientFactory: func(namespace string, _ kdexv1alpha1.Secrets, _ logr.Logger) (utils.HelmClientInterface, error) {
			namespaces = append(namespaces, namespace)
			return &stubHelmClient{id: len(namespaces)}, nil
		},
	}

	first, err := r.getOrCreateHelmClient("a-b", "c", nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.getOrCreateHelmClient("a", "b-c", nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("host a in b-c got host a-b's client, bound to namespace %v", namespaces)
	}
}

// deleteHelmClient forgets a host's cached client.
func TestHelmClientCache_DeleteForgetsTheHost(t *testing.T) {
	r := &KDexHostReconciler{
		HelmClientFactory: func(string, kdexv1alpha1.Secrets, logr.Logger) (utils.HelmClientInterface, error) {
			return &stubHelmClient{}, nil
		},
	}
	if _, err := r.getOrCreateHelmClient("host", "ns", nil, logr.Discard()); err != nil {
		t.Fatal(err)
	}
	if err := r.deleteHelmClient("host", "ns"); err != nil {
		t.Fatal(err)
	}
	if len(r.helmClients) != 0 {
		t.Fatalf("deleteHelmClient left %d clients", len(r.helmClients))
	}
}

func (s *stubHelmClient) ReleaseStatus(string) (string, error) { return "", nil }

func (s *stubHelmClient) DropClusterRBACFromRecord(string) ([]string, error) { return nil, nil }
