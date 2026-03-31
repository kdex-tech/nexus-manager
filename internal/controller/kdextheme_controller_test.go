/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("KDexTheme Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		AfterEach(func() {
			cleanupResources(namespace)
		})

		It("should not validate without assets", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets:  kdexv1alpha1.Assets{},
					Backend: kdexv1alpha1.Backend{},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should validate with only absolute assets and no static image", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: []kdexv1alpha1.Asset{
						{
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
							LinkHref: "http://kdex.dev/style.css",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexTheme{}, true)
		})

		It("should not validate with relative assets but no static image", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: []kdexv1alpha1.Asset{
						{
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
							LinkHref: "/style.css",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should not validate with static image but invalid assets", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: []kdexv1alpha1.Asset{
						{
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
							LinkHref: "/style.css",
						},
					},
					Backend: kdexv1alpha1.Backend{
						StaticImage: "kdex/theme:123",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should not validate with ingressPath but no static image", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: []kdexv1alpha1.Asset{
						{
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
							LinkHref: "/-/theme/style.css",
						},
					},
					Backend: kdexv1alpha1.Backend{
						IngressPath: "/-/theme",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should not validate with image, ingressPath and relative assets that are not prefixed by ingressPath", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: kdexv1alpha1.Assets{
						{
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
							LinkHref: "/style.css",
						},
					},
					Backend: kdexv1alpha1.Backend{
						IngressPath: "/-/theme",
						StaticImage: "foo/bar",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should validate with image, ingressPath and relative assets that are prefixed by ingressPath", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: kdexv1alpha1.Assets{
						{
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
							LinkHref: "/-/theme/style.css",
						},
					},
					Backend: kdexv1alpha1.Backend{
						IngressPath: "/-/theme",
						StaticImage: "foo/bar",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		It("should validate with staticImage and relative assets that are prefixed by default ingressPath", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: kdexv1alpha1.Assets{
						{
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
							LinkHref: "/-/theme/style.css",
						},
					},
					Backend: kdexv1alpha1.Backend{
						StaticImage: "foo/bar",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		It("should validate with serverImage and relative assets that are prefixed by default ingressPath", func() {
			resource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: kdexv1alpha1.Assets{
						{
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
							LinkHref: "/-/theme/style.css",
						},
					},
					Backend: kdexv1alpha1.Backend{
						ServerImage: "foo/bar",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})
	})
})
