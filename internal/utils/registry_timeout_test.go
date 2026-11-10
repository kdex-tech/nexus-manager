package utils

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/registry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// A chart reference is tenant input: a companion may name any registry. One
// that accepts the connection and never answers would hold the helm operation
// -- its render slot, and a teardown waiting on it -- for as long as it likes.
// Every registry request has a deadline, whatever the secret says.
func TestRegistryClient_TimesOutAgainstATarpit(t *testing.T) {
	release := make(chan struct{})
	stall := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release })
	plain := httptest.NewServer(stall)
	defer plain.Close()
	tlsTarpit := httptest.NewTLSServer(stall)
	defer tlsTarpit.Close()
	// Deferred last so it runs first: Close waits for the stalled handlers.
	defer close(release)

	secret := func(repo string, data map[string]string) kdexv1alpha1.Secrets {
		s := corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"kdex.dev/secret-type": "helm"}},
			Data:       map[string][]byte{"repository": []byte(repo)},
		}
		for k, v := range data {
			s.Data[k] = []byte(v)
		}
		return kdexv1alpha1.Secrets{s}
	}
	plainHost := strings.TrimPrefix(plain.URL, "http://")
	tlsHost := strings.TrimPrefix(tlsTarpit.URL, "https://")

	cases := []struct {
		name    string
		host    string
		secrets kdexv1alpha1.Secrets
	}{
		{"plain HTTP", plainHost, secret(plainHost, map[string]string{"plainHTTP": "true"})},
		{"plain HTTP with credentials", plainHost, secret(plainHost, map[string]string{
			"plainHTTP": "true", "username": "u", "password": "p",
		})},
		{"insecure TLS", tlsHost, secret(tlsHost, map[string]string{"insecure": "true"})},
		{"insecure TLS with credentials", tlsHost, secret(tlsHost, map[string]string{
			"insecure": "true", "username": "u", "password": "p",
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := tc.host + "/charts/tarpit"
			options, err := registryClientOptions("oci://"+ref, tc.secrets, false, 200*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			c, err := registry.NewClient(options...)
			if err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() {
				_, err := c.Tags(ref)
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "Client.Timeout") {
					t.Fatalf("Tags against a tarpit = %v, want the client's timeout", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Tags against a tarpit did not return: the registry client has no deadline")
			}
		})
	}
}
