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

// ListReleases owns no releases: this stub exists to measure render
// concurrency, and returning none keeps the prune pass a no-op.
func (m *concurrencyTrackingHelmClient) ListReleases(map[string]string) ([]string, error) {
	return nil, nil
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

// pruneTrackingHelmClient renders instantly but dwells inside Uninstall, so a
// test can observe whether renders proceed while a prune is in flight.
type pruneTrackingHelmClient struct {
	utils.HelmClientInterface
	pruneDwell time.Duration

	pruning atomic.Int32
	// rendersDuringPrune counts renders that began while some other host's
	// prune was still running -- the property the early slot release buys.
	rendersDuringPrune atomic.Int32
	renders            atomic.Int32
}

func (m *pruneTrackingHelmClient) InstallOrUpgrade(_ *utils.ChartSpec) error {
	if m.pruning.Load() > 0 {
		m.rendersDuringPrune.Add(1)
	}
	m.renders.Add(1)
	return nil
}

// One owned release, never declared by any host, so every prune uninstalls it.
func (m *pruneTrackingHelmClient) ListReleases(map[string]string) ([]string, error) {
	return []string{"orphaned-companion"}, nil
}

func (m *pruneTrackingHelmClient) Uninstall(string) error {
	m.pruning.Add(1)
	time.Sleep(m.pruneDwell)
	m.pruning.Add(-1)
	return nil
}

// The render slot bounds MEMORY -- concurrent chart loads. A prune loads no
// chart; it lists releases and uninstalls, and each uninstall waits on
// foreground deletion for up to five minutes. Holding the slot across that lets
// one companion blocked by a finalizer stall every other host's render, which
// is the failure the slot exists to prevent rather than cause.
//
// With a single slot, a render can only overlap another host's prune if the
// slot was released before that prune began.
func TestRenderSlotIsReleasedBeforeThePrune(t *testing.T) {
	const numHosts = 4

	scheme := runtime.NewScheme()
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kdex scheme: %v", err)
	}
	if err := configuration.AddToScheme(scheme); err != nil {
		t.Fatalf("add configuration scheme: %v", err)
	}

	objs := make([]client.Object, 0, numHosts)
	for i := range numHosts {
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

	mock := &pruneTrackingHelmClient{pruneDwell: 60 * time.Millisecond}

	r := &KDexHostReconciler{
		Client:                fc,
		ControllerID:          "test",
		Ctx:                   context.Background(),
		Configuration:         configuration.LoadConfiguration("/nonexistent-config.yaml", scheme),
		Scheme:                scheme,
		HelmRenderConcurrency: 1,
		HelmClientFactory: func(string, kdexv1alpha1.Secrets, logr.Logger) (utils.HelmClientInterface, error) {
			return mock, nil
		},
	}

	var wg sync.WaitGroup
	for i := range numHosts {
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

	if got := mock.renders.Load(); got != int32(numHosts) {
		t.Fatalf("rendered %d hosts, want %d", got, numHosts)
	}
	if mock.rendersDuringPrune.Load() == 0 {
		t.Error("no render overlapped a prune: the render slot is being held across the prune, " +
			"so one stuck uninstall stalls the whole fleet")
	}
}
