package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("KDexTranslation hostRef", func() {
	ctx := context.Background()
	var hostA, hostB string

	BeforeEach(func() {
		suffix := time.Now().UnixNano()
		hostA = fmt.Sprintf("tr-host-a-%d", suffix)
		hostB = fmt.Sprintf("tr-host-b-%d", suffix)
	})

	AfterEach(func() {
		cleanupResources(namespace)
	})

	newHost := func(name string) *kdexv1alpha1.KDexHost {
		return &kdexv1alpha1.KDexHost{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing:      kdexv1alpha1.Routing{Domains: []string{name + ".example.test"}},
			},
		}
	}

	attached := func(name, host string) *kdexv1alpha1.KDexTranslation {
		return &kdexv1alpha1.KDexTranslation{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kdexv1alpha1.KDexNamespacedTranslationSpec{
				HostRef: &corev1.LocalObjectReference{Name: host},
				KDexTranslationSpec: kdexv1alpha1.KDexTranslationSpec{Translations: []kdexv1alpha1.Translation{
					{Lang: "en", KeysAndValues: map[string]string{"shop.title": "Shop"}},
				}},
			},
		}
	}

	internalExists := func(name string) func() bool {
		return func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &kdexv1alpha1.KDexInternalTranslation{})
			return err == nil
		}
	}
	internalGone := func(name string) func() bool {
		return func() bool {
			it := &kdexv1alpha1.KDexInternalTranslation{}
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, it)
			return apierrors.IsNotFound(err) || (err == nil && !it.DeletionTimestamp.IsZero())
		}
	}

	It("rejects a hostRef with an empty name", func() {
		tr := attached("empty-hostref", "")
		err := k8sClient.Create(ctx, tr)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("hostRef.name must not be empty"))
	})

	It("attaches, prunes on delete, and follows a moved hostRef", func() {
		Expect(k8sClient.Create(ctx, newHost(hostA))).To(Succeed())
		Expect(k8sClient.Create(ctx, newHost(hostB))).To(Succeed())

		tr := attached("shop-strings", hostA)
		Expect(k8sClient.Create(ctx, tr)).To(Succeed())

		Eventually(internalExists(hostA+"-shop-strings"), "20s", "500ms").Should(BeTrue(),
			"a self-attached translation is copied to its host with no edit to the host")

		Eventually(func() []string {
			ih := &kdexv1alpha1.KDexInternalHost{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hostA}, ih); err != nil {
				return nil
			}
			out := []string{}
			for _, r := range ih.Spec.InternalTranslationRefs {
				out = append(out, r.Name)
			}
			return out
		}, "20s", "500ms").Should(Equal([]string{hostA + "-kdex-default-translation", hostA + "-shop-strings"}))

		// Move the hostRef from A to B: A prunes its copy, B gains one.
		Eventually(func() error {
			latest := &kdexv1alpha1.KDexTranslation{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "shop-strings"}, latest); err != nil {
				return err
			}
			latest.Spec.HostRef = &corev1.LocalObjectReference{Name: hostB}
			return k8sClient.Update(ctx, latest)
		}, "10s").Should(Succeed())

		Eventually(internalGone(hostA+"-shop-strings"), "20s", "500ms").Should(BeTrue())
		Eventually(internalExists(hostB+"-shop-strings"), "20s", "500ms").Should(BeTrue())

		// Delete the translation: B prunes its copy.
		Expect(k8sClient.Delete(ctx, tr)).To(Succeed())
		Eventually(internalGone(hostB+"-shop-strings"), "20s", "500ms").Should(BeTrue())
	})

	It("prunes the copy when hostRef is removed", func() {
		Expect(k8sClient.Create(ctx, newHost(hostA))).To(Succeed())
		Expect(k8sClient.Create(ctx, attached("detach-strings", hostA))).To(Succeed())
		Eventually(internalExists(hostA+"-detach-strings"), "20s", "500ms").Should(BeTrue())

		Eventually(func() error {
			latest := &kdexv1alpha1.KDexTranslation{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "detach-strings"}, latest); err != nil {
				return err
			}
			latest.Spec.HostRef = nil
			return k8sClient.Update(ctx, latest)
		}, "10s").Should(Succeed())

		Eventually(internalGone(hostA+"-detach-strings"), "20s", "500ms").Should(BeTrue())
	})

	// Regression: before this change, removing a translationRefs entry left its
	// KDexInternalTranslation (and so its strings) live until the host was deleted.
	It("prunes a host-declared translation when its translationRefs entry is removed", func() {
		declared := attached("site-strings", "")
		declared.Spec.HostRef = nil
		Expect(k8sClient.Create(ctx, declared)).To(Succeed())

		host := newHost(hostA)
		host.Spec.TranslationRefs = []kdexv1alpha1.KDexObjectReference{{Kind: "KDexTranslation", Name: "site-strings"}}
		Expect(k8sClient.Create(ctx, host)).To(Succeed())
		Eventually(internalExists(hostA+"-site-strings"), "20s", "500ms").Should(BeTrue())

		Eventually(func() error {
			latest := &kdexv1alpha1.KDexHost{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: hostA}, latest); err != nil {
				return err
			}
			latest.Spec.TranslationRefs = nil
			return k8sClient.Update(ctx, latest)
		}, "10s").Should(Succeed())

		Eventually(internalGone(hostA+"-site-strings"), "20s", "500ms").Should(BeTrue())
	})
})
