package controller

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// withTeardownIndexes registers the indexes a host's teardown lists its
// internal pages and translations by, as SetupWithManager does. b must
// already have its scheme.
func withTeardownIndexes(b *fake.ClientBuilder) *fake.ClientBuilder {
	return b.
		WithIndex(&kdexv1alpha1.KDexInternalUtilityPage{}, hostIndexKey, indexInternalUtilityPageByHost).
		WithIndex(&kdexv1alpha1.KDexInternalTranslation{}, hostIndexKey, indexInternalTranslationByHost)
}

// hookedHelmClient runs its hooks around a MockHelmClient's calls, standing in
// for whatever else happens in the cluster meanwhile, and records every
// install it was asked for -- whether or not it then went ahead -- and every
// listing.
type hookedHelmClient struct {
	*MockHelmClient
	afterInstall func(releaseName string)
	onUninstall  func(releaseName string)

	mu        sync.Mutex
	attempted []string
	lists     atomic.Int32
}

func (h *hookedHelmClient) InstallOrUpgrade(spec *utils.ChartSpec) error {
	h.mu.Lock()
	h.attempted = append(h.attempted, spec.ReleaseName)
	h.mu.Unlock()
	err := h.MockHelmClient.InstallOrUpgrade(spec)
	if h.afterInstall != nil {
		h.afterInstall(spec.ReleaseName)
	}
	return err
}

func (h *hookedHelmClient) Uninstall(releaseName string) error {
	if h.onUninstall != nil {
		h.onUninstall(releaseName)
	}
	return h.MockHelmClient.Uninstall(releaseName)
}

func (h *hookedHelmClient) ListReleases(labels map[string]string) ([]string, error) {
	h.lists.Add(1)
	return h.MockHelmClient.ListReleases(labels)
}

// Attempted is every release an install was asked for, in order.
func (h *hookedHelmClient) Attempted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.attempted)
}
