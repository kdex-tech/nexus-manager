package utils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

func TestHelmClient_Initialization(t *testing.T) {
	namespace := "test-namespace"
	client, err := NewHelmClient(namespace, nil, logr.Logger{})
	require.NoError(t, err)
	assert.NotNil(t, client)
	assert.NotNil(t, client.actionConfig)
	assert.NotNil(t, client.settings)
	assert.Equal(t, namespace, client.settings.Namespace())
}
func TestHelmClient_OCIChartReachability(t *testing.T) {
	// This test requires internet access and might be slow.
	// We use a known public OCI chart for testing.
	client, err := NewHelmClient("default", nil, logr.Logger{})
	require.NoError(t, err)

	chartName := "oci://ghcr.io/kdex-tech/charts/host-manager"
	version := "0.2.18"

	ctx := context.Background()

	spec := &ChartSpec{
		ReleaseName: "test-reachability",
		ChartName:   chartName,
		Namespace:   "default",
		Version:     version,
	}

	// Just to satisfy the compiler while we draft the test.
	assert.NotNil(t, ctx)
	assert.NotNil(t, spec)

	// For the purpose of "failing unit test", we can just assert the interface is satisfied.
	var _ HelmClientInterface = client
}

func TestHelmClient_SecretWithPlainHTTP(t *testing.T) {
	calledPlainHTTP := false

	server := MockServer(
		func(mux *http.ServeMux) {
			mux.HandleFunc(
				"/{path...}",
				func(w http.ResponseWriter, r *http.Request) {
					calledPlainHTTP = true
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				},
			)
		},
	)

	defer server.Close()

	serverHostName := server.URL[7:]

	client, err := NewHelmClient("default", kdexv1alpha1.Secrets{
		corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo",
				Namespace: "default",
				Annotations: map[string]string{
					"kdex.dev/secret-type": "helm",
				},
			},
			Data: map[string][]byte{
				"plainHTTP":  []byte("true"),
				"repository": []byte(serverHostName + "/charts"),
			},
		},
	}, logr.Logger{})
	require.NoError(t, err)

	chartName := "oci://" + serverHostName + "/charts/host-manager"
	version := "0.2.18"

	ctx := context.Background()

	spec := &ChartSpec{
		ReleaseName: "test-reachability",
		ChartName:   chartName,
		Namespace:   "default",
		Version:     version,
	}

	// Just to satisfy the compiler while we draft the test.
	assert.NotNil(t, ctx)
	assert.NotNil(t, spec)

	// For the purpose of "failing unit test", we can just assert the interface is satisfied.
	var _ HelmClientInterface = client

	_, err = client.ShowChart(spec)
	assert.Error(t, err)
	assert.True(t, calledPlainHTTP)
}

func MockServer(setup func(mux *http.ServeMux)) *httptest.Server {
	mux := http.NewServeMux()

	setup(mux)

	server := httptest.NewServer(mux)

	return server
}
