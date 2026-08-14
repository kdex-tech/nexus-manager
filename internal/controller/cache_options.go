package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretTypeHelmRelease is the Secret type Helm's storage driver writes for
// every release revision.
const SecretTypeHelmRelease = "helm.sh/release.v1"

// CacheOptions returns the manager cache configuration shared by the operator
// binary and the test suite.
//
// The Secret informer is scoped away from Helm release history. The operator
// watches Secrets only to react to PackageReference.SecretRef and the
// cluster-default npm credential, but controller-runtime's default cache is
// unscoped, so a single shared informer LISTs and WATCHes every Secret in the
// cluster. Release history dominates that set and is never read through the
// cached client — Helm's own action.NewList runs on its own clientset — so
// caching it buys nothing and costs the largest single term in the heap.
//
// Measured in rsi-gke-cluster on v0.4.1: 440 of 493 Secrets were
// helm.sh/release.v1 totalling 38.66 MiB, retaining 32.5 MB of a 61.35 MB live
// heap (53%). Because GOGC doubles the live heap to set the collection target,
// that floor produced NextGC 119 MB and Sys 143 MB — over a 128Mi limit on
// steady state alone.
//
// `type` is one of the few field selectors kube-apiserver registers for
// v1.Secret, so this is served server-side and the excluded objects never reach
// the client.
func CacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Field: fields.OneTermNotEqualSelector("type", SecretTypeHelmRelease),
			},
		},
	}
}
