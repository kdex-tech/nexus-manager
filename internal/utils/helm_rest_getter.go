package utils

import (
	"net/http"
	"sync"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/kube"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// Discovery's client-side rate limits: cli-runtime's ConfigFlags defaults,
// which Helm's own getter used.
const (
	discoveryBurst = 300
	discoveryQPS   = 50.0
)

// restConfigGetter is the genericclioptions.RESTClientGetter a HelmClient acts
// through: a fixed REST config, impersonating or not, in place of Helm's
// kubeconfig loading (see NewHelmClient). It mirrors what Helm's own getter
// adds to a config -- its rate limits and its retrying transport.
type restConfigGetter struct {
	config    *rest.Config
	namespace string

	mu        sync.Mutex
	discovery discovery.CachedDiscoveryInterface
}

var _ genericclioptions.RESTClientGetter = (*restConfigGetter)(nil)

// newRESTConfigGetter copies base, acting as impersonate when it is not
// empty and as base's own identity when it is, whatever base impersonated.
func newRESTConfigGetter(base *rest.Config, namespace, impersonate string, settings *cli.EnvSettings) *restConfigGetter {
	c := rest.CopyConfig(base)
	c.Impersonate = rest.ImpersonationConfig{UserName: impersonate}
	c.Burst = settings.BurstLimit
	c.QPS = settings.QPS
	c.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &kube.RetryingRoundTripper{Wrapped: rt}
	})
	return &restConfigGetter{config: c, namespace: namespace}
}

// ToRESTConfig returns a copy: callers adjust what they are given (discovery
// sets its own rate limits).
func (g *restConfigGetter) ToRESTConfig() (*rest.Config, error) {
	return rest.CopyConfig(g.config), nil
}

// ToDiscoveryClient returns one memory-cached discovery client for the
// getter's lifetime, which every REST mapper it hands out reads. Nothing
// refreshes it on its own: Helm invalidates it only when it first reads a
// configuration's Capabilities or installs a chart's crds/, and a mapper
// re-reads it on a miss only while it is not yet populated. So a HelmClient
// invalidates it as each operation starts (freshen), or a kind that appeared
// after the client first ran would never be mapped.
func (g *restConfigGetter) ToDiscoveryClient() (discovery.CachedDiscoveryInterface, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.discovery != nil {
		return g.discovery, nil
	}
	c := rest.CopyConfig(g.config)
	c.Burst = discoveryBurst
	c.QPS = discoveryQPS
	dc, err := discovery.NewDiscoveryClientForConfig(c)
	if err != nil {
		return nil, err
	}
	g.discovery = memory.NewMemCacheClient(dc)
	return g.discovery, nil
}

// invalidateDiscovery makes the next discovery read go to the server. It is
// safe while an operation is running: the cache takes its own lock. A nil
// getter -- a client assembled over a fake kube client -- has nothing to
// invalidate.
func (g *restConfigGetter) invalidateDiscovery() {
	if g == nil {
		return
	}
	g.mu.Lock()
	dc := g.discovery
	g.mu.Unlock()
	if dc != nil {
		dc.Invalidate()
	}
}

// ToRESTMapper returns a new mapper over the shared discovery cache: no mapper
// outlives the call that asked for it, so none needs resetting.
func (g *restConfigGetter) ToRESTMapper() (meta.RESTMapper, error) {
	dc, err := g.ToDiscoveryClient()
	if err != nil {
		return nil, err
	}
	return restmapper.NewShortcutExpander(restmapper.NewDeferredDiscoveryRESTMapper(dc), dc, nil), nil
}

// ToRawKubeConfigLoader answers what Helm asks of a kubeconfig loader: the
// namespace a manifest's namespaced object without one lands in, and the
// same REST config.
func (g *restConfigGetter) ToRawKubeConfigLoader() clientcmd.ClientConfig {
	return fixedClientConfig{g}
}

// fixedClientConfig is the clientcmd.ClientConfig of a restConfigGetter. It
// loads nothing: there is no kubeconfig behind it.
type fixedClientConfig struct {
	g *restConfigGetter
}

// RawConfig is empty: no kubeconfig was loaded. Nothing Helm runs reads it.
func (c fixedClientConfig) RawConfig() (clientcmdapi.Config, error) {
	return clientcmdapi.Config{}, nil
}

func (c fixedClientConfig) ClientConfig() (*rest.Config, error) {
	return c.g.ToRESTConfig()
}

func (c fixedClientConfig) Namespace() (string, bool, error) {
	return c.g.namespace, true, nil
}

// ConfigAccess is nil: there is no kubeconfig to load or persist. Nothing
// Helm runs reads it.
func (c fixedClientConfig) ConfigAccess() clientcmd.ConfigAccess {
	return nil
}
