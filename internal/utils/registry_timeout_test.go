package utils

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/registry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"oras.land/oras-go/v2/registry/remote/retry"
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
				if !isTimeout(err) {
					t.Fatalf("Tags against a tarpit = %v, want a timeout", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Tags against a tarpit did not return: the registry client has no deadline")
			}
		})
	}
}

func testLimits(request, budget time.Duration, maxRequests int) registryFetchLimits {
	return registryFetchLimits{
		requestTimeout: request,
		budget:         budget,
		maxRequests:    maxRequests,
		maxBytes:       RegistryFetchMaxBytes,
	}
}

// isTimeout reports whether err is a deadline that ran out, whichever layer
// noticed first: the http.Client's Timeout, or the retry transport's own
// context check, which returns a bare context.DeadlineExceeded.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
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

// A blob is pulled into memory whatever size it declares, so any one response
// is bounded too, and the error says what lets a bigger chart through.
func TestFetchBudget_BoundsTheBytesOfAnyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 64<<10)
		for range 1 << 10 { // 64MiB
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	limits := testLimits(time.Minute, time.Minute, 100)
	limits.maxBytes = 1 << 20
	c := &http.Client{Transport: newFetchBudget(http.DefaultTransport, limits)}

	resp, err := c.Get(srv.URL + "/v2/charts/c/blobs/sha256:abc")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	n, err := io.Copy(io.Discard, resp.Body)
	// A chart past the limit is one the operator must be told how to let
	// through.
	if err == nil || !strings.Contains(err.Error(), "--registry-fetch-max-bytes") ||
		!strings.Contains(err.Error(), "registryFetchMaxBytes") {
		t.Errorf("the budget's error = %v, want it to name the flag and the chart value that raise it", err)
	}
	if !errors.Is(err, errFetchBudget) {
		t.Errorf("reading a 64MiB response within a 1MiB budget = %v, want the fetch budget exhausted", err)
	}
	if n > 2<<20 {
		t.Errorf("read %d bytes within a 1MiB budget", n)
	}
}

// An insecure or plain-HTTP registry is reached directly, as it always was: a
// proxy taken from the environment would carry its credentials in the clear,
// and an in-cluster registry missing from NO_PROXY would become unreachable.
func TestRegistryTransport_InsecureGoesDirect(t *testing.T) {
	rt, ok := registryTransport(false, true).(*retry.Transport)
	if !ok {
		t.Fatalf("registryTransport = %T, want it wrapped for retries", registryTransport(false, true))
	}
	base, ok := rt.Base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport = %T", rt.Base)
	}
	if base.Proxy != nil {
		t.Error("the insecure transport takes a proxy from the environment")
	}
	if base.TLSClientConfig == nil || !base.TLSClientConfig.InsecureSkipVerify {
		t.Error("the insecure transport verifies TLS")
	}
	if base.DialContext == nil || base.TLSHandshakeTimeout == 0 || base.IdleConnTimeout == 0 {
		t.Error("the insecure transport lost the default transport's dial and idle timeouts")
	}
}

// tagPage writes one page of tags linking to next: one tag for each 16 bytes
// of pageBytes, each tag shorter than that, so the page comes to somewhat
// under pageBytes.
func tagPage(w http.ResponseWriter, n int64, pageBytes int, next string) {
	var tags []string
	for size := 0; size < pageBytes; size += 16 {
		tags = append(tags, fmt.Sprintf(`"%d.%d.0"`, n, size))
	}
	w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"name":"charts/tarpit","tags":[%s]}`, strings.Join(tags, ","))
}

// endlessLinks serves 256KiB tag pages without end, each linking to next(n).
func endlessLinks(next func(n int64) string) func(served *atomic.Int64) http.Handler {
	return func(served *atomic.Int64) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := served.Add(1)
			tagPage(w, n, 256<<10, next(n))
		})
	}
}

// listTarpit lists a tag listing served by h against a 1MiB byte budget,
// returning the error and the pages h served.
func listTarpit(t *testing.T, h func(served *atomic.Int64) http.Handler) (error, int64) {
	t.Helper()
	var served atomic.Int64
	srv := httptest.NewServer(h(&served))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	limits := testLimits(time.Minute, time.Minute, 1_000_000)
	limits.maxBytes = 1 << 20
	err := tagsWithin(t, host+"/charts/tarpit", helmSecret(host, map[string]string{"plainHTTP": "true"}), limits, 20*time.Second)
	return err, served.Load()
}

// Time and request count do not bound memory. To resolve a version range
// Helm keeps every semver tag of every page in the heap, several times its
// size on the wire, so a registry handing out page after page could grow the
// operator past its memory limit well within its time and request budgets.
// The fetch's bytes are bounded too, by one budget across every response:
// the registry writes every URL the client follows -- each page's Link, each
// redirect -- so whatever the next page's URL looks like, even a blob's, a
// page of tags is still a page of tags.
func TestRegistryClient_BoundsTheBytesOfATagListing(t *testing.T) {
	for _, tc := range []struct {
		next    string
		handler func(served *atomic.Int64) http.Handler
	}{
		{"the tag list's own path", endlessLinks(func(n int64) string {
			return fmt.Sprintf("/v2/charts/tarpit/tags/list?n=1&last=%d", n)
		})},
		{"another path", endlessLinks(func(n int64) string {
			return fmt.Sprintf("/elsewhere/page?n=%d", n)
		})},
		{"a blob's path", endlessLinks(func(n int64) string {
			return fmt.Sprintf("/v2/charts/tarpit/blobs/sha256:%064d?p=%d", n, n)
		})},
		{"a redirect off the tag list's path", func(served *atomic.Int64) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/tags/list") {
					http.Redirect(w, r, "/storage/page?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
					return
				}
				n := served.Add(1)
				tagPage(w, n, 256<<10, fmt.Sprintf("/v2/charts/tarpit/tags/list?n=1&last=%d", n))
			})
		}},
	} {
		t.Run(tc.next, func(t *testing.T) {
			err, pages := listTarpit(t, tc.handler)
			if !errors.Is(err, errFetchBudget) {
				t.Fatalf("an endless listing whose next page is at %s = %v, want the fetch budget exhausted", tc.next, err)
			}
			if pages > 6 {
				t.Errorf("read %d pages of 256KiB within a 1MiB budget, want it stopped by the sixth", pages)
			}
		})
	}
}

// The byte budget is the manager's to set.
func TestHelmClient_TakesTheRegistryFetchByteBudget(t *testing.T) {
	c, err := NewHelmClient("ns", baseRESTConfig(), nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	if got := c.registryLimits().maxBytes; got != RegistryFetchMaxBytes {
		t.Errorf("default byte budget = %d, want %d", got, RegistryFetchMaxBytes)
	}
	c, err = NewHelmClient("ns", baseRESTConfig(), nil, logr.Discard(), WithRegistryFetchMaxBytes(123))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.registryLimits().maxBytes; got != 123 {
		t.Errorf("configured byte budget = %d, want 123", got)
	}
}
