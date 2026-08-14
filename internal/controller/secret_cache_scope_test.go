package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The operator watches Secrets only to react to PackageReference.SecretRef and
// the cluster-default npm credential, but controller-runtime's default cache is
// unscoped: one shared informer LISTs and WATCHes every Secret in the cluster.
// Helm release history dominates that set (measured in rsi-gke-cluster: 440 of
// 493 Secrets, 38.66 MiB, retaining 32.5 MB of a 61.35 MB live heap) and is
// never read through the cached client — Helm's own action.NewList uses its own
// clientset. CacheOptions must keep that history out of the informer.
var _ = Describe("Secret cache scoping", func() {
	It("caches ordinary Secrets but not helm.sh/release.v1 Secrets", func() {
		releaseSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sh.helm.release.v1.cache-scope.v1",
				Namespace: namespace,
			},
			Type: "helm.sh/release.v1",
			Data: map[string][]byte{"release": []byte("release-payload")},
		}
		opaqueSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cache-scope-npm-credential",
				Namespace: namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{"token": []byte("t")},
		}

		Expect(k8sClient.Create(ctx, releaseSecret)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, releaseSecret))).To(Succeed())
		})
		Expect(k8sClient.Create(ctx, opaqueSecret)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, opaqueSecret))).To(Succeed())
		})

		opts := CacheOptions()
		opts.Scheme = scheme.Scheme

		scopedCache, err := cache.New(cfg, opts)
		Expect(err).NotTo(HaveOccurred())

		cacheCtx, stopCache := context.WithCancel(context.Background())
		DeferCleanup(stopCache)
		go func() {
			defer GinkgoRecover()
			Expect(scopedCache.Start(cacheCtx)).To(Succeed())
		}()
		Expect(scopedCache.WaitForCacheSync(cacheCtx)).To(BeTrue())

		var cached corev1.SecretList
		Expect(scopedCache.List(cacheCtx, &cached, client.InNamespace(namespace))).To(Succeed())

		names := make([]string, 0, len(cached.Items))
		for _, s := range cached.Items {
			names = append(names, s.Name)
		}

		// Asserted together on purpose: without the positive case a selector that
		// cached nothing at all would pass the exclusion vacuously.
		Expect(names).To(ContainElement(opaqueSecret.Name),
			"the scoped cache must still serve the Secrets the operator actually reads")
		Expect(names).NotTo(ContainElement(releaseSecret.Name),
			"helm release history must never enter the informer cache")
	})
})
