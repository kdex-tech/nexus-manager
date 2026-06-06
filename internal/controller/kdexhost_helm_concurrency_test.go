package controller

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// concurrencyTrackingHelmClient records how many InstallOrUpgrade calls run at
// the same time so a test can assert the global render concurrency bound.
type concurrencyTrackingHelmClient struct {
	utils.HelmClientInterface
	dwell   time.Duration
	current atomic.Int32
	max     atomic.Int32
	total   atomic.Int32
}

func (m *concurrencyTrackingHelmClient) InstallOrUpgrade(_ *utils.ChartSpec) error {
	cur := m.current.Add(1)
	for {
		prev := m.max.Load()
		if cur <= prev || m.max.CompareAndSwap(prev, cur) {
			break
		}
	}
	m.total.Add(1)
	time.Sleep(m.dwell)
	m.current.Add(-1)
	return nil
}

// TestHelmRenderConcurrencyIsBounded verifies that no more than
// HelmRenderConcurrency in-process Helm renders run simultaneously, regardless
// of how many hosts dispatch renders at once. See issue #24.
func TestHelmRenderConcurrencyIsBounded(t *testing.T) {
	const (
		numHosts = 6
		limit    = 2
	)

	scheme := runtime.NewScheme()
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kdex scheme: %v", err)
	}
	if err := configuration.AddToScheme(scheme); err != nil {
		t.Fatalf("add configuration scheme: %v", err)
	}

	objs := make([]client.Object, 0, numHosts)
	for i := 0; i < numHosts; i++ {
		objs = append(objs, &kdexv1alpha1.KDexHost{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("host-%d", i),
				Namespace: "default",
			},
			Spec: kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				ModulePolicy: kdexv1alpha1.LooseModulePolicy,
			},
		})
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&kdexv1alpha1.KDexHost{}).
		Build()

	mock := &concurrencyTrackingHelmClient{dwell: 40 * time.Millisecond}

	r := &KDexHostReconciler{
		Client:                fc,
		ControllerID:          "test",
		Ctx:                   context.Background(),
		Configuration:         configuration.LoadConfiguration("/nonexistent-config.yaml", scheme),
		Scheme:                scheme,
		HelmRenderConcurrency: limit,
		HelmClientFactory: func(string, kdexv1alpha1.Secrets, logr.Logger) (utils.HelmClientInterface, error) {
			return mock, nil
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < numHosts; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r.runAsyncHelmReconcile(
				context.Background(),
				"default",
				fmt.Sprintf("host-%d", n),
				"hash",
				nil,
				logr.Discard(),
			)
		}(i)
	}
	wg.Wait()

	if got := mock.max.Load(); got > int32(limit) {
		t.Fatalf("max concurrent helm renders = %d, want <= %d", got, limit)
	}
	if got := mock.total.Load(); got != int32(numHosts) {
		t.Fatalf("rendered %d hosts, want %d", got, numHosts)
	}
}
