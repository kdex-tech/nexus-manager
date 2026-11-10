package utils

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/registry"
)

func sha256Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// forgedSizeRegistry serves one chart whose manifest declares a chart layer
// of layerSize bytes, while the layer itself is a few bytes, sent without a
// Content-Length that would give the forgery away.
func forgedSizeRegistry(layerSize int64) http.Handler {
	config := []byte(`{"name":"huge","version":"1.0.0","apiVersion":"v2"}`)
	layer := []byte("not a chart")
	manifest, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config": map[string]any{
			"mediaType": registry.ConfigMediaType, "digest": sha256Digest(config), "size": len(config),
		},
		"layers": []map[string]any{{
			"mediaType": registry.ChartLayerMediaType, "digest": sha256Digest(layer), "size": layerSize,
		}},
	})
	blobs := map[string][]byte{sha256Digest(config): config, sha256Digest(layer): layer}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/manifests/"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
			w.Header().Set("Docker-Content-Digest", sha256Digest(manifest))
			w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(manifest)
			}
		case strings.Contains(r.URL.Path, "/blobs/"):
			blob, ok := blobs[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			// Chunked, with no Content-Length to contradict the manifest:
			// the client has only the declared size to go on.
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			_, _ = w.Write(blob)
		default:
			http.NotFound(w, r)
		}
	})
}

// A manifest is the registry's to write, and the size it declares for a layer
// is only a claim. oras-go before v2.7.0 allocated that claim before reading a
// byte, so a manifest of a few hundred bytes declaring 2^62 panicked the
// operator -- in a goroutine of oras's own, which nothing can recover -- and
// one declaring a terabyte ran it out of memory. The pull fails instead.
func TestRegistryClient_RefusesAForgedLayerSize(t *testing.T) {
	srv := httptest.NewServer(forgedSizeRegistry(1 << 62))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	options, err := registryClientOptions("oci://"+host+"/charts/huge", helmSecret(host, map[string]string{"plainHTTP": "true"}), false, defaultRegistryFetchLimits)
	if err != nil {
		t.Fatal(err)
	}
	c, err := registry.NewClient(options...)
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		err      error
		panicked any
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- outcome{panicked: p}
			}
		}()
		_, err := c.Pull(host + "/charts/huge:1.0.0")
		done <- outcome{err: err}
	}()
	select {
	case o := <-done:
		if o.panicked != nil {
			t.Fatalf("pulling a chart whose manifest forges its layer size panicked: %v", o.panicked)
		}
		if o.err == nil {
			t.Fatal("pulling a chart whose manifest forges its layer size succeeded")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the pull did not return")
	}
}
