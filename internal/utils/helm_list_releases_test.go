package utils

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func releaseSecret(ns, secretName string, lbls map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns, Labels: lbls},
		Data:       map[string][]byte{"release": []byte("payload")},
	}
}

func TestListReleases_MetadataOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const owner = "kdex.dev/companion-of"
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		releaseSecret("ns", "sh.helm.release.v1.a.v1", map[string]string{"owner": "helm", "name": "a", owner: "h"}),
		releaseSecret("ns", "sh.helm.release.v1.a.v2", map[string]string{"owner": "helm", "name": "a", owner: "h"}),
		releaseSecret("ns", "sh.helm.release.v1.b.v1", map[string]string{"owner": "helm", "name": "b"}),
		releaseSecret("ns", "plain", map[string]string{"name": "c", owner: "h"}),
		releaseSecret("other", "sh.helm.release.v1.d.v1", map[string]string{"owner": "helm", "name": "d", owner: "h"}),
	).Build()

	c := &HelmClient{namespace: "ns", metaReader: fc}
	got, err := c.ListReleases(map[string]string{owner: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"a"}) {
		t.Errorf("ListReleases = %v, want [a]", got)
	}
}

func TestListReleases_NoReaderIsAnError(t *testing.T) {
	c := &HelmClient{namespace: "ns"}
	if _, err := c.ListReleases(nil); err == nil {
		t.Fatal("want an error without a metadata reader")
	}
}
