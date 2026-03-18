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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("KDexUtilityPage Controller", func() {
	const (
		KDexUtilityPageName = "test-utility-page"
		timeout             = time.Second * 10
		duration            = time.Second * 10
		interval            = time.Millisecond * 250
	)

	Context("When reconciling a KDexUtilityPage", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		AfterEach(func() {
			cleanupResources(namespace)
		})

		It("should not validate without type", func() {
			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{},
			}

			err := k8sClient.Create(ctx, utilityPage)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`supported values: "Announcement", "Error", "Login"`))
		})

		It("should not validate without content entries", func() {
			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					Type: "Announcement",
				},
			}

			err := k8sClient.Create(ctx, utilityPage)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`spec.contentEntries in body must be of type array: "null"`))
		})

		It("should not validate with empty content entries", func() {
			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{},
					Type:           "Announcement",
				},
			}

			err := k8sClient.Create(ctx, utilityPage)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`spec.contentEntries in body should have at least 1 items`))
		})

		It("should not validate without 'main' content entry", func() {
			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "sidebar",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<h1>sidebar</h1>",
							},
						},
					},
					Type: "Announcement",
				},
			}

			err := k8sClient.Create(ctx, utilityPage)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`slot 'main' must be specified`))
		})

		It("should not validate with slot with no content", func() {
			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
						},
					},
					Type: "Announcement",
				},
			}

			err := k8sClient.Create(ctx, utilityPage)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`exactly one of the fields in [appRef rawHTML] must be set`))
		})

		It("should not validate with no page archetype", func() {
			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "",
							},
						},
					},
					Type: "Announcement",
				},
			}

			err := k8sClient.Create(ctx, utilityPage)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`pageArchetypeRef.name must not be empty`))
		})

		It("should not validate with empty content entry", func() {
			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "",
							},
						},
					},
					PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
						Name: "test-utility-archetype",
					},
					Type: "Announcement",
				},
			}

			err := k8sClient.Create(ctx, utilityPage)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`exactly one of the fields in [appRef rawHTML] must be set`))
		})

		It("should not validate with invalid rawHTML content entry", func() {
			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<foo",
							},
						},
					},
					PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
						Kind: "KDexPageArchetype",
						Name: "test-utility-archetype",
					},
					Type: "Announcement",
				},
			}

			err := k8sClient.Create(ctx, utilityPage)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(`invalid go template in spec.contentEntries[0].rawHTML: html/template:main: ends in a non-text context`))
		})

		It("Should update KDexUtilityPage Status", func() {
			By("Creating a new KDexUtilityPage")

			// Create prerequisite PageArchetype
			archetype := &kdexv1alpha1.KDexPageArchetype{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-utility-archetype",
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageArchetypeSpec{
					Content: "<html><body>[[.Content.main]]</body></html>",
				},
			}

			Expect(k8sClient.Create(ctx, archetype)).Should(Succeed())

			assertResourceReady(
				ctx, k8sClient, "test-utility-archetype", namespace,
				&kdexv1alpha1.KDexPageArchetype{}, true)

			utilityPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      KDexUtilityPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					Type: kdexv1alpha1.AnnouncementUtilityPageType,
					PageArchetypeRef: kdexv1alpha1.KDexObjectReference{
						Kind: "KDexPageArchetype",
						Name: archetype.Name,
					},
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<h1>Hello World</h1>",
							},
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, utilityPage)).Should(Succeed())

			createdUtilityPage := &kdexv1alpha1.KDexUtilityPage{}
			assertResourceReady(
				ctx, k8sClient, KDexUtilityPageName, namespace,
				createdUtilityPage, true)

			utilityPageLookupKey := types.NamespacedName{Name: KDexUtilityPageName, Namespace: namespace}

			Eventually(func() string {
				err := k8sClient.Get(ctx, utilityPageLookupKey, createdUtilityPage)
				if err != nil {
					return ""
				}
				if createdUtilityPage.Status.Attributes == nil {
					return ""
				}
				return createdUtilityPage.Status.Attributes["archetype.generation"]
			}, timeout, interval).ShouldNot(BeEmpty())
		})
	})
})
