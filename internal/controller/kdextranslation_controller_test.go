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

var _ = Describe("KDexTranslation Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-translation"

		ctx := context.Background()

		AfterEach(func() {
			cleanupResources(namespace)
		})

		It("should not validate without host", func() {
			resource := &kdexv1alpha1.KDexTranslation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexNamespacedTranslationSpec{KDexTranslationSpec: kdexv1alpha1.KDexTranslationSpec{}},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should not validate without translations", func() {
			resource := &kdexv1alpha1.KDexTranslation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexNamespacedTranslationSpec{KDexTranslationSpec: kdexv1alpha1.KDexTranslationSpec{}},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should not validate with empty translations", func() {
			resource := &kdexv1alpha1.KDexTranslation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexNamespacedTranslationSpec{KDexTranslationSpec: kdexv1alpha1.KDexTranslationSpec{
					Translations: []kdexv1alpha1.Translation{},
				}},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should validate with translations", func() {
			resource := &kdexv1alpha1.KDexTranslation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexNamespacedTranslationSpec{KDexTranslationSpec: kdexv1alpha1.KDexTranslationSpec{
					Translations: []kdexv1alpha1.Translation{
						{
							Lang: "fr",
							KeysAndValues: map[string]string{
								"test": "test",
							},
						},
					},
				}},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		It("should reconcile when valid", func() {
			resource := &kdexv1alpha1.KDexTranslation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexNamespacedTranslationSpec{KDexTranslationSpec: kdexv1alpha1.KDexTranslationSpec{
					Translations: []kdexv1alpha1.Translation{
						{
							Lang: "fr",
							KeysAndValues: map[string]string{
								"test": "test",
							},
						},
					},
				}},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexTranslation{}, true)
		})
	})
})
