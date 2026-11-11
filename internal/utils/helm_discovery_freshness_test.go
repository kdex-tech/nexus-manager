package utils

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/chart/common"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// fakeDiscoveryServer is an API server serving discovery for core v1 and
// group freshness.test/v1, whose kind Foo is served only once fooServed is
// set -- a CRD installed while the operator runs. It answers a list of the
// namespace's Secrets (Helm's release records) with none.
func fakeDiscoveryServer(t *testing.T, fooServed *atomic.Bool) *httptest.Server {
	t.Helper()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]string{"major": "1", "minor": "35", "gitVersion": "v1.35.0"})
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		write(w, metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}})
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		write(w, metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: "v1",
			APIResources: []metav1.APIResource{{Name: "secrets", SingularName: "secret", Namespaced: true, Kind: "Secret", Verbs: []string{"get", "list"}}}})
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		gv := metav1.GroupVersionForDiscovery{GroupVersion: "freshness.test/v1", Version: "v1"}
		write(w, metav1.APIGroupList{TypeMeta: metav1.TypeMeta{Kind: "APIGroupList", APIVersion: "v1"},
			Groups: []metav1.APIGroup{{Name: "freshness.test", Versions: []metav1.GroupVersionForDiscovery{gv}, PreferredVersion: gv}}})
	})
	mux.HandleFunc("/apis/freshness.test/v1", func(w http.ResponseWriter, _ *http.Request) {
		list := metav1.APIResourceList{TypeMeta: metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"}, GroupVersion: "freshness.test/v1",
			APIResources: []metav1.APIResource{}}
		if fooServed.Load() {
			list.APIResources = append(list.APIResources, metav1.APIResource{Name: "foos", SingularName: "foo", Namespaced: true, Kind: "Foo", Verbs: []string{"get", "list", "create"}})
		}
		write(w, list)
	})
	mux.HandleFunc("/api/v1/namespaces/ns/secrets", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"kind": "SecretList", "apiVersion": "v1", "items": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

var fooKind = schema.GroupKind{Group: "freshness.test", Kind: "Foo"}

// A cached client lives across many operations. Each operation sees the
// cluster as it is when it starts: a kind that appeared after the client last
// discovered is mapped, and the chart is rendered against fresh Capabilities.
func TestHelmClient_EachOperationRediscovers(t *testing.T) {
	isolateFromKubeconfig(t)
	var fooServed atomic.Bool
	srv := fakeDiscoveryServer(t, &fooServed)

	h, err := NewHelmClient("ns", &rest.Config{Host: srv.URL}, nil, logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	getter := h.actionConfig.RESTClientGetter

	// An earlier operation discovered, and rendered with Capabilities, before
	// Foo was served.
	if err := h.Uninstall("absent"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	mapper, err := getter.ToRESTMapper()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapper.RESTMapping(fooKind, "v1"); err == nil {
		t.Fatal("Foo mapped before it was served")
	}
	h.actionConfig.Capabilities = common.DefaultCapabilities.Copy()

	fooServed.Store(true)

	// The next operation: an install or upgrade (no chart reference: refused
	// before any fetch), then an uninstall.
	if err := h.InstallOrUpgrade(&ChartSpec{ReleaseName: "r", ChartName: ""}); err == nil {
		t.Fatal("InstallOrUpgrade without a chart reference succeeded")
	}
	if h.actionConfig.Capabilities != nil {
		t.Error("InstallOrUpgrade kept the Capabilities an earlier operation read")
	}
	if err := h.Uninstall("absent"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	mapper, err = getter.ToRESTMapper()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapper.RESTMapping(fooKind, "v1"); err != nil {
		t.Errorf("Foo, served since the last operation, is not mapped: %v", err)
	}
}
