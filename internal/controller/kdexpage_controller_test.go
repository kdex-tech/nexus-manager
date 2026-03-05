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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("KDexPage Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		AfterEach(func() {
			cleanupResources(namespace)
			cleanupResources(secondNamespace)
		})

		It("must not validate if basePath is empty", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`spec.basePath in body should match '^/'`))
		})

		It("must not validate if no contentEntries are provided", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`spec.contentEntries: Invalid value: "null"`))
		})

		It("must not validate if contentEntries is empty", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`spec.contentEntries in body should have at least 1 items`))
		})

		It("must not validate if contentEntries doesn't have a 'main' slot", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{},
					},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`slot 'main' must be specified`))
		})

		It("must not validate if contentEntries doesn't have either 'rawHTML' or 'appRef'", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
						},
					},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`exactly one of the fields in [appRef rawHTML] must be set`))
		})

		It("must not validate if label is not set", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<h1>Hello, World!</h1>",
							},
						},
					},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`spec.label in body should be at least 3 chars long`))
		})

		It("must not validate if contentEntries has both 'rawHTML' and 'appRef'", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryApp: kdexv1alpha1.ContentEntryApp{
								AppRef:            &kdexv1alpha1.KDexObjectReference{},
								CustomElementName: "test-custom-element",
							},
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<h1>Hello, World!</h1>",
							},
						},
					},
					HostRef: corev1.LocalObjectReference{
						Name: "test-host",
					},
					Label: "test-label",
					PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
						Name: "test-page-archetype",
					},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`exactly one of the fields in [appRef rawHTML] must be set`))
		})

		It("must not validate if hostRef.name is empty", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<invalid html",
							},
						},
					},
					Label: "test",
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`hostRef.name must not be empty`))
		})

		It("must not validate if pageArchetypeRef.name is missing name", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<invalid html",
							},
						},
					},
					HostRef: corev1.LocalObjectReference{
						Name: "test-host",
					},
					Label: "test",
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`pageArchetypeRef.name must not be empty`))
		})

		It("must not validate if contentEntries has invalid rawHTML", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<invalid html",
							},
						},
					},
					HostRef: corev1.LocalObjectReference{
						Name: "test-host",
					},
					Label: "test",
					PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
						Name: "test-page-archetype",
					},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`invalid go template in spec.contentEntries[0].rawHTML`))
		})

		It("must not validate if app contentEntry is missing customElementName", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryApp: kdexv1alpha1.ContentEntryApp{
								AppRef: &kdexv1alpha1.KDexObjectReference{},
							},
						},
					},
					HostRef: corev1.LocalObjectReference{
						Name: "test-host",
					},
					Label: "test",
					PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
						Name: "test-page-archetype",
					},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`no such key: customElementName evaluating rule: appRef must be accompanied by customElementName`))
		})

		It("must not validate if app contentEntry appRef is missing name", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryApp: kdexv1alpha1.ContentEntryApp{
								AppRef:            &kdexv1alpha1.KDexObjectReference{},
								CustomElementName: "test-element",
							},
						},
					},
					HostRef: corev1.LocalObjectReference{
						Name: "test-host",
					},
					Label: "test",
					PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
						Name: "test-page-archetype",
					},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`spec.contentEntries[0].appRef.name is required`))
		})

		It("will validate with minimum fields", func() {
			resource := &kdexv1alpha1.KDexPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryApp: kdexv1alpha1.ContentEntryApp{
								AppRef: &kdexv1alpha1.KDexObjectReference{
									Name: "test-app",
								},
								CustomElementName: "test-element",
							},
						},
					},
					HostRef: corev1.LocalObjectReference{
						Name: "test-host",
					},
					Label: "test",
					PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
						Name: "test-page-archetype",
					},
					Paths: kdexv1alpha1.Paths{
						BasePath: "/",
					},
				},
			}

			err := k8sClient.Create(ctx, resource)
			Expect(err).ToNot(HaveOccurred())
		})
	})
})
