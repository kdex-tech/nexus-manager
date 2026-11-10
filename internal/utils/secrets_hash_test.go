package utils

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// ownedSecret is a Secret as an operator such as External Secrets leaves it:
// owned, by a controller, whose deletion it blocks. Every one of those is a
// pointer field.
func ownedSecret(name, resourceVersion string) corev1.Secret {
	yes := true
	return corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns", ResourceVersion: resourceVersion,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "external-secrets.io/v1", Kind: "ExternalSecret", Name: name, UID: "u",
				Controller: &yes, BlockOwnerDeletion: &yes,
			}},
		},
		Data: map[string][]byte{"password": []byte("hunter2")},
	}
}

// The Secrets a host resolved are compared across reconciles -- to reuse a
// Helm client, a located chart, a run's backoff -- and every reconcile reads a
// fresh copy of them. Two copies of the same Secrets are the same Secrets.
func TestHashSecrets_IsStableAcrossCopies(t *testing.T) {
	secrets := kdexv1alpha1.Secrets{ownedSecret("a", "1"), ownedSecret("b", "7")}
	if HashSecrets(secrets.DeepCopy()) != HashSecrets(secrets.DeepCopy()) {
		t.Fatal("two copies of the same Secrets hash differently")
	}
	reordered := kdexv1alpha1.Secrets{ownedSecret("b", "7"), ownedSecret("a", "1")}
	if HashSecrets(secrets) != HashSecrets(reordered) {
		t.Error("the same Secrets in another order hash differently")
	}
}

// A Secret updated is a Secret changed: its resourceVersion says so, without
// its contents -- which would end up in status -- ever being hashed.
func TestHashSecrets_ChangesWithAnUpdate(t *testing.T) {
	before := HashSecrets(kdexv1alpha1.Secrets{ownedSecret("a", "1")})
	if HashSecrets(kdexv1alpha1.Secrets{ownedSecret("a", "2")}) == before {
		t.Error("an updated Secret hashes as before")
	}
	if HashSecrets(kdexv1alpha1.Secrets{ownedSecret("other", "1")}) == before {
		t.Error("another Secret hashes as this one")
	}
	changed := ownedSecret("a", "1")
	changed.Data["password"] = []byte("something else")
	if HashSecrets(kdexv1alpha1.Secrets{changed}) != before {
		t.Error("the hash depends on a Secret's contents")
	}
}
