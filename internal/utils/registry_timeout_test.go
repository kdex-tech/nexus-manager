package utils

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/registry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// helmSecret is a Helm registry secret for repo, with data.
func helmSecret(repo string, data map[string]string) kdexv1alpha1.Secrets {
	s := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"kdex.dev/secret-type": "helm"}},
		Data:       map[string][]byte{"repository": []byte(repo)},
	}
	for k, v := range data {
		s.Data[k] = []byte(v)
	}
	return kdexv1alpha1.Secrets{s}
}

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

	plainHost := strings.TrimPrefix(plain.URL, "http://")
	tlsHost := strings.TrimPrefix(tlsTarpit.URL, "https://")

	cases := []struct {
		name    string
		host    string
		secrets kdexv1alpha1.Secrets
	}{
		{"plain HTTP", plainHost, helmSecret(plainHost, map[string]string{"plainHTTP": "true"})},
		{"plain HTTP with credentials", plainHost, helmSecret(plainHost, map[string]string{
			"plainHTTP": "true", "username": "u", "password": "p",
		})},
		{"insecure TLS", tlsHost, helmSecret(tlsHost, map[string]string{"insecure": "true"})},
		{"insecure TLS with credentials", tlsHost, helmSecret(tlsHost, map[string]string{
			"insecure": "true", "username": "u", "password": "p",
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref := tc.host + "/charts/tarpit"
			options, err := registryClientOptions("oci://"+ref, tc.secrets, false, testLimits(200*time.Millisecond, time.Minute, 1000))
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

func testLimits(request, budget time.Duration, maxRequests int) registryFetchLimits {
	return registryFetchLimits{requestTimeout: request, budget: budget, maxRequests: maxRequests}
}

// tagsWithin lists ref's tags through a registry client built with limits for
// a chart in that registry, failing the test if it has not returned by within.
func tagsWithin(t *testing.T, ref string, secrets kdexv1alpha1.Secrets, limits registryFetchLimits, within time.Duration) error {
	t.Helper()
	options, err := registryClientOptions("oci://"+ref, secrets, false, limits)
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
		return err
	case <-time.After(within):
		t.Fatalf("Tags(%s) did not return within %s", ref, within)
		return nil
	}
}

// endlessTags serves a tag list that always has one more page, each served
// promptly, after delay.
func endlessTags(delay time.Duration, served *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := served.Add(1)
		time.Sleep(delay)
		w.Header().Set("Link", fmt.Sprintf(`<%s?n=1&last=0.0.%d>; rel="next"`, r.URL.Path, n))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"name":"charts/tarpit","tags":["0.0.%d"]}`, n)
	})
}

// A per-request timeout does not bound a fetch. Resolving a chart whose
// version is unpinned or a range lists the repository's tags, following each
// page's Link to the next: a registry that always has another page, each
// served at once, would hold the operation and its render slot forever. The
// fetch as a whole is bounded, by request count and by time.
func TestRegistryClient_BoundsAnEndlessTagListing(t *testing.T) {
	t.Run("by request count", func(t *testing.T) {
		var served atomic.Int64
		srv := httptest.NewServer(endlessTags(0, &served))
		defer srv.Close()
		host := strings.TrimPrefix(srv.URL, "http://")
		secrets := helmSecret(host, map[string]string{"plainHTTP": "true"})

		err := tagsWithin(t, host+"/charts/tarpit", secrets, testLimits(time.Minute, time.Minute, 20), 20*time.Second)
		if err == nil {
			t.Fatal("an endless tag listing succeeded, want it cut off")
		}
		if n := served.Load(); n > 20 {
			t.Errorf("the registry served %d requests, want at most 20", n)
		}
	})
	t.Run("by time", func(t *testing.T) {
		var served atomic.Int64
		srv := httptest.NewServer(endlessTags(20*time.Millisecond, &served))
		defer srv.Close()
		host := strings.TrimPrefix(srv.URL, "http://")
		secrets := helmSecret(host, map[string]string{"plainHTTP": "true"})

		start := time.Now()
		err := tagsWithin(t, host+"/charts/tarpit", secrets, testLimits(time.Minute, 300*time.Millisecond, 1_000_000), 20*time.Second)
		if err == nil {
			t.Fatal("an endless tag listing succeeded, want it cut off")
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("the listing ran %s, want it cut off near its 300ms budget", took)
		}
	})
}

// The tenant case: a chart in a registry no Helm secret names, which accepts
// the connection and never completes the TLS handshake.
func TestRegistryClient_TimesOutAgainstAnUnknownRegistry(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	var conns []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	start := time.Now()
	err = tagsWithin(t, ln.Addr().String()+"/charts/tarpit", nil, testLimits(300*time.Millisecond, time.Minute, 1000), 20*time.Second)
	if err == nil {
		t.Fatal("Tags against a registry that never answers succeeded")
	}
	// The default transport's own TLS handshake timeout is 10s: well inside
	// it, the deadline cutting the request off is ours.
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Tags gave up after %s, want it cut off near its 300ms timeout", took)
	}
}

// A secret's credentials still reach the registry it names.
func TestRegistryClient_SendsTheSecretsCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "u" || p != "p" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"charts/c","tags":["1.0.0"]}`))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	secrets := helmSecret(host, map[string]string{"plainHTTP": "true", "username": "u", "password": "p"})

	if err := tagsWithin(t, host+"/charts/c", secrets, defaultRegistryFetchLimits, 20*time.Second); err != nil {
		t.Errorf("Tags with the secret's credentials = %v, want nil", err)
	}
}
