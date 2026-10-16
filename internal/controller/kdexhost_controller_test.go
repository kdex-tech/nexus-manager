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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func readyResources(ctx context.Context, name, namespace string) {
	// 1. Create and ready the deployment
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "manager",
							Image: "manager:latest",
						},
					},
				},
			},
		},
	}
	Eventually(func() error {
		err := k8sClient.Create(ctx, deployment)
		if err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}, "5s").Should(Succeed())

	Eventually(func() error {
		latest := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Status.Conditions = []appsv1.DeploymentCondition{
			{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
			},
		}
		return k8sClient.Status().Patch(ctx, latest, patch)
	}, "5s").Should(Succeed())

	// 2. Ready the KDexInternalHost, KDexInternalTranslation, or KDexInternalUtilityPage
	Eventually(func() error {
		// Try KDexInternalHost
		ih := &kdexv1alpha1.KDexInternalHost{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ih); err == nil {
			patch := client.MergeFrom(ih.DeepCopy())
			if ih.Status.Attributes == nil {
				ih.Status.Attributes = make(map[string]string)
			}
			kdexv1alpha1.SetConditions(
				&ih.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Ready: metav1.ConditionTrue,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				"Mock ready",
			)
			return k8sClient.Status().Patch(ctx, ih, patch)
		}

		// Try KDexInternalTranslation
		it := &kdexv1alpha1.KDexInternalTranslation{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, it); err == nil {
			patch := client.MergeFrom(it.DeepCopy())
			if it.Status.Attributes == nil {
				it.Status.Attributes = make(map[string]string)
			}
			it.Status.Attributes["translation.generation"] = fmt.Sprintf("%d", it.Generation)
			kdexv1alpha1.SetConditions(
				&it.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Ready: metav1.ConditionTrue,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				"Mock ready",
			)
			return k8sClient.Status().Patch(ctx, it, patch)
		}

		// Try KDexInternalUtilityPage
		ip := &kdexv1alpha1.KDexInternalUtilityPage{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, ip); err == nil {
			patch := client.MergeFrom(ip.DeepCopy())
			if ip.Status.Attributes == nil {
				ip.Status.Attributes = make(map[string]string)
			}
			ip.Status.Attributes["utilitypage.generation"] = fmt.Sprintf("%d", ip.Generation)
			kdexv1alpha1.SetConditions(
				&ip.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Ready: metav1.ConditionTrue,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				"Mock ready",
			)
			return k8sClient.Status().Patch(ctx, ip, patch)
		}

		return fmt.Errorf("resource %s/%s not found as any internal type", namespace, name)
	}, "10s").Should(Succeed())
}

var _ = Describe("KDexHost Controller", func() {
	Context("When reconciling a resource", func() {
		var resourceName string
		ctx := context.Background()

		BeforeEach(func() {
			resourceName = fmt.Sprintf("host-%d", time.Now().UnixNano())
		})

		AfterEach(func() {
			cleanupResources(namespace)
		})

		It("it must not validate if it has missing brandName", func() {
			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("it must not validate if it has missing organization", func() {
			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName: "KDex Tech",
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("it must not validate if it has missing routing", func() {
			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("it must not validate if it has missing routing domains", func() {
			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{},
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("it reconciles if minimum required fields are present", func() {
			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{
							"kdex.dev",
						},
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, resource)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			readyResources(ctx, resourceName, namespace)

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexHost{}, true)
		})

		It("it reconciles if theme reference becomes available", func() {
			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{
							"kdex.dev",
						},
						Strategy: kdexv1alpha1.IngressRoutingStrategy,
					},
					ThemeRef: &kdexv1alpha1.KDexObjectReference{
						Kind: "KDexTheme",
						Name: "non-existent-theme",
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, resource)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			// Ensure internal host is created by the reconciler OR we create it manually with valid spec
			internalHost := &kdexv1alpha1.KDexInternalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexInternalHostSpec{
					KDexHostSpec: resource.Spec,
				},
			}
			// Initialize required fields if not present
			if internalHost.Spec.ModulePolicy == "" {
				internalHost.Spec.ModulePolicy = kdexv1alpha1.LooseModulePolicy
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, internalHost)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": resourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": resourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "manager", Image: "manager:latest"}},
						},
					},
				},
			}
			Eventually(func() error {
				err := k8sClient.Create(ctx, deployment)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			themeResource := &kdexv1alpha1.KDexTheme{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-existent-theme",
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexThemeSpec{
					Assets: kdexv1alpha1.Assets{
						{
							LinkHref: "http://foo.bar/style.css",
							Attributes: map[string]string{
								"rel": "stylesheet",
							},
						},
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, themeResource)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			assertResourceReady(
				ctx, k8sClient, themeResource.Name, namespace,
				&kdexv1alpha1.KDexTheme{}, true)

			// Now ready them all
			readyResources(ctx, resourceName, namespace)

			Eventually(func() bool {
				checkedHost := &kdexv1alpha1.KDexHost{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: namespace}, checkedHost)
				if err != nil {
					return false
				}
				return meta.IsStatusConditionTrue(checkedHost.Status.Conditions, string(kdexv1alpha1.ConditionTypeReady))
			}, "15s", "1s").Should(BeTrue(), "Expected KDexHost to be Ready after theme creation")
		})

		It("it reconciles if scriptlibrary reference becomes available", func() {
			resource := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					ModulePolicy: kdexv1alpha1.StrictModulePolicy,
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{
							"kdex.dev",
						},
						Strategy: kdexv1alpha1.IngressRoutingStrategy,
					},
					ScriptLibraryRef: &kdexv1alpha1.KDexObjectReference{
						Kind: "KDexScriptLibrary",
						Name: "non-existent-script-library",
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, resource)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			// We need to manually create the KDexInternalHost and Deployment
			internalHost := &kdexv1alpha1.KDexInternalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexInternalHostSpec{
					KDexHostSpec: resource.Spec,
				},
			}
			if internalHost.Spec.ModulePolicy == "" {
				internalHost.Spec.ModulePolicy = kdexv1alpha1.LooseModulePolicy
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, internalHost)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": resourceName},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": resourceName},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "manager", Image: "manager:latest"}},
						},
					},
				},
			}
			Eventually(func() error {
				err := k8sClient.Create(ctx, deployment)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			addOrUpdateScriptLibrary(
				ctx, k8sClient,
				kdexv1alpha1.KDexScriptLibrary{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "non-existent-script-library",
						Namespace: namespace,
					},
					Spec: kdexv1alpha1.KDexScriptLibrarySpec{
						Scripts: []kdexv1alpha1.ScriptDef{
							{
								Attributes: map[string]string{
									"type": "text/module",
								},
								ScriptSrc: "http://foo.bar/script.js",
							},
						},
					},
				},
			)

			readyResources(ctx, resourceName, namespace)

			Eventually(func() bool {
				checkedHost := &kdexv1alpha1.KDexHost{}
				err := k8sClient.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: namespace}, checkedHost)
				if err != nil {
					return false
				}
				return meta.IsStatusConditionTrue(checkedHost.Status.Conditions, string(kdexv1alpha1.ConditionTypeReady))
			}, "15s", "1s").Should(BeTrue(), "Expected KDexHost to be Ready after scriptlibrary creation")
		})

		It("it reconciles a referenced utility page", func() {
			archetypeName := resourceName + "-archetype"
			pageArchetype := &kdexv1alpha1.KDexPageArchetype{
				ObjectMeta: metav1.ObjectMeta{
					Name:      archetypeName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexPageArchetypeSpec{
					Content: "[[ .Content.main ]]",
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, pageArchetype)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			assertResourceReady(
				ctx, k8sClient, archetypeName, namespace,
				&kdexv1alpha1.KDexPageArchetype{}, true)

			announcementPageName := resourceName + "-announcement"
			announcementPage := &kdexv1alpha1.KDexUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      announcementPageName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<h1>Announcement</h1>",
							},
						},
					},
					PageArchetypeRef: &kdexv1alpha1.KDexObjectReference{
						Kind: "KDexPageArchetype",
						Name: archetypeName,
					},
					Type: kdexv1alpha1.AnnouncementUtilityPageType,
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, announcementPage)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			assertResourceReady(
				ctx, k8sClient, announcementPageName, namespace,
				&kdexv1alpha1.KDexUtilityPage{}, true)

			host := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{
							"kdex.dev",
						},
					},
					UtilityPages: &kdexv1alpha1.UtilityPages{
						AnnouncementRef: &kdexv1alpha1.KDexObjectReference{
							Kind: "KDexUtilityPage",
							Name: announcementPageName,
						},
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, host)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			// Manually create the KDexInternalUtilityPage
			internalUtilityPageResource := &kdexv1alpha1.KDexInternalUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-%s", host.Name, strings.ToLower(string(kdexv1alpha1.AnnouncementUtilityPageType))),
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexInternalUtilityPageSpec{
					KDexUtilityPageSpec: announcementPage.Spec,
					HostRef:             corev1.LocalObjectReference{Name: host.Name},
				},
			}
			Eventually(func() error {
				err := k8sClient.Create(ctx, internalUtilityPageResource)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			// Manually create the KDexInternalHost and Deployment
			internalHost := &kdexv1alpha1.KDexInternalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      host.Name,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexInternalHostSpec{
					KDexHostSpec: host.Spec,
				},
			}
			internalHost.Spec.ModulePolicy = kdexv1alpha1.LooseModulePolicy

			Eventually(func() error {
				err := k8sClient.Create(ctx, internalHost)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      host.Name,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": host.Name},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": host.Name},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "manager", Image: "manager:latest"}},
						},
					},
				},
			}
			Eventually(func() error {
				err := k8sClient.Create(ctx, deployment)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			readyResources(ctx, internalUtilityPageResource.Name, namespace)
			readyResources(ctx, host.Name, namespace)

			checkedHost := &kdexv1alpha1.KDexHost{}
			var err error
			Eventually(func() bool {
				err = k8sClient.Get(ctx, types.NamespacedName{Name: host.Name, Namespace: namespace}, checkedHost)
				if err != nil {
					return false
				}
				return meta.IsStatusConditionTrue(checkedHost.Status.Conditions, string(kdexv1alpha1.ConditionTypeReady))
			}, "15s", "1s").Should(BeTrue(), "Expected KDexHost to be Ready after utility page creation")

			Eventually(
				checkedHost.Status.Attributes["announcement.utilitypage.generation"], "5s",
			).Should(Equal("1"))

			internalUtilityPage := &kdexv1alpha1.KDexInternalUtilityPage{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-announcement", host.Name),
				Namespace: namespace,
			}, internalUtilityPage)

			Expect(err).NotTo(HaveOccurred())
			Expect(internalUtilityPage.Spec.ContentEntries).To(HaveLen(1))
			Expect(internalUtilityPage.Spec.PageArchetypeRef.Name).To(Equal(archetypeName))
			Expect(internalUtilityPage.Spec.Type).To(Equal(kdexv1alpha1.AnnouncementUtilityPageType))
			Expect(internalUtilityPage.Spec.ContentEntries[0].Slot).To(Equal("main"))
			Expect(internalUtilityPage.Spec.ContentEntries[0].ContentEntryStatic.RawHTML).To(Equal("<h1>Announcement</h1>"))
		})

		It("it reconciles a default utility page if a default is available and none is specified", func() {
			pageArchetype := &kdexv1alpha1.KDexClusterPageArchetype{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kdex-default-page-archetype",
				},
				Spec: kdexv1alpha1.KDexPageArchetypeSpec{
					Content: "[[ .Content.main ]]",
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, pageArchetype)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			assertResourceReady(
				ctx, k8sClient, pageArchetype.Name, "",
				&kdexv1alpha1.KDexClusterPageArchetype{}, true)

			announcementPage := &kdexv1alpha1.KDexClusterUtilityPage{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kdex-default-utility-page-announcement",
				},
				Spec: kdexv1alpha1.KDexUtilityPageSpec{
					ContentEntries: []kdexv1alpha1.ContentEntry{
						{
							Slot: "main",
							ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{
								RawHTML: "<h1>Announcement</h1>",
							},
						},
					},
					PageArchetypeRef: &kdexv1alpha1.KDexObjectReference{
						Kind: "KDexClusterPageArchetype",
						Name: pageArchetype.Name,
					},
					Type: kdexv1alpha1.AnnouncementUtilityPageType,
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, announcementPage)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			assertResourceReady(
				ctx, k8sClient, announcementPage.Name, "",
				&kdexv1alpha1.KDexClusterUtilityPage{}, true)

			host := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{
							"kdex.dev",
						},
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, host)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			readyResources(ctx, host.Name, namespace)

			checkedHost := &kdexv1alpha1.KDexHost{}
			var err error
			assertResourceReady(
				ctx, k8sClient, host.Name, namespace,
				checkedHost, true)

			Eventually(
				checkedHost.Status.Attributes["announcement.utilitypage.generation"], "5s",
			).Should(Equal("1"))

			internalUtilityPage := &kdexv1alpha1.KDexInternalUtilityPage{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-announcement", host.Name),
				Namespace: namespace,
			}, internalUtilityPage)

			Expect(err).NotTo(HaveOccurred())
			Expect(internalUtilityPage.Spec.ContentEntries).To(HaveLen(1))
			Expect(internalUtilityPage.Spec.PageArchetypeRef.Name).To(Equal(pageArchetype.Name))
			Expect(internalUtilityPage.Spec.Type).To(Equal(kdexv1alpha1.AnnouncementUtilityPageType))
			Expect(internalUtilityPage.Spec.ContentEntries[0].Slot).To(Equal("main"))
			Expect(internalUtilityPage.Spec.ContentEntries[0].ContentEntryStatic.RawHTML).To(Equal("<h1>Announcement</h1>"))
		})

		It("it reconciles a referenced translation", func() {
			translation := &kdexv1alpha1.KDexTranslation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "non-existent-translation",
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexNamespacedTranslationSpec{KDexTranslationSpec: kdexv1alpha1.KDexTranslationSpec{
					Translations: []kdexv1alpha1.Translation{
						{
							Lang: "en",
							KeysAndValues: map[string]string{
								"brandName":    "KDex Tech",
								"organization": "KDex Tech Inc.",
							},
						},
					},
				}},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, translation)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			assertResourceReady(
				ctx, k8sClient, translation.Name, namespace,
				&kdexv1alpha1.KDexTranslation{}, true)

			host := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{
							"kdex.dev",
						},
					},
					TranslationRefs: []kdexv1alpha1.KDexObjectReference{
						{
							Kind: "KDexTranslation",
							Name: translation.Name,
						},
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, host)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			// Manually create the KDexInternalTranslation
			internalTranslationResource := &kdexv1alpha1.KDexInternalTranslation{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("%s-%s", host.Name, translation.Name),
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexInternalTranslationSpec{
					KDexTranslationSpec: translation.Spec.KDexTranslationSpec,
					HostRef:             corev1.LocalObjectReference{Name: host.Name},
				},
			}
			Eventually(func() error {
				err := k8sClient.Create(ctx, internalTranslationResource)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			// Manually create the KDexInternalHost and Deployment
			internalHost := &kdexv1alpha1.KDexInternalHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      host.Name,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexInternalHostSpec{
					KDexHostSpec: host.Spec,
				},
			}
			internalHost.Spec.ModulePolicy = kdexv1alpha1.LooseModulePolicy

			Eventually(func() error {
				err := k8sClient.Create(ctx, internalHost)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      host.Name,
					Namespace: namespace,
				},
				Spec: appsv1.DeploymentSpec{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": host.Name},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{"app": host.Name},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "manager", Image: "manager:latest"}},
						},
					},
				},
			}
			Eventually(func() error {
				err := k8sClient.Create(ctx, deployment)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			readyResources(ctx, internalTranslationResource.Name, namespace)
			readyResources(ctx, host.Name, namespace)

			checkedHost := &kdexv1alpha1.KDexHost{}
			var err error
			Eventually(func() bool {
				err = k8sClient.Get(ctx, types.NamespacedName{Name: host.Name, Namespace: namespace}, checkedHost)
				if err != nil {
					return false
				}
				return meta.IsStatusConditionTrue(checkedHost.Status.Conditions, string(kdexv1alpha1.ConditionTypeReady))
			}, "15s", "1s").Should(BeTrue(), "Expected KDexHost to be Ready after translation creation")

			Eventually(
				checkedHost.Status.Attributes[translation.Name+".translation.generation"], "5s",
			).Should(Equal("1"))

			internalTranslation := &kdexv1alpha1.KDexInternalTranslation{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-%s", host.Name, translation.Name),
				Namespace: namespace,
			}, internalTranslation)

			Expect(err).NotTo(HaveOccurred())
			Expect(internalTranslation.Spec.Translations).To(HaveLen(1))
			Expect(internalTranslation.Spec.Translations[0].Lang).To(Equal("en"))
			Expect(internalTranslation.Spec.Translations[0].KeysAndValues).To(HaveLen(2))
			Expect(internalTranslation.Spec.Translations[0].KeysAndValues["brandName"]).To(Equal("KDex Tech"))
			Expect(internalTranslation.Spec.Translations[0].KeysAndValues["organization"]).To(Equal("KDex Tech Inc."))
		})

		It("it reconciles a default translation if a default is available", func() {
			translation := &kdexv1alpha1.KDexClusterTranslation{
				ObjectMeta: metav1.ObjectMeta{
					Name: "kdex-default-translation",
				},
				Spec: kdexv1alpha1.KDexTranslationSpec{
					Translations: []kdexv1alpha1.Translation{
						{
							Lang: "en",
							KeysAndValues: map[string]string{
								"brandName":    "KDex Tech",
								"organization": "KDex Tech Inc.",
							},
						},
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, translation)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			assertResourceReady(
				ctx, k8sClient, translation.Name, "",
				&kdexv1alpha1.KDexClusterTranslation{}, true)

			host := &kdexv1alpha1.KDexHost{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexHostSpec{
					BrandName:    "KDex Tech",
					Organization: "KDex Tech Inc.",
					Routing: kdexv1alpha1.Routing{
						Domains: []string{
							"kdex.dev",
						},
					},
				},
			}

			Eventually(func() error {
				err := k8sClient.Create(ctx, host)
				if err != nil && !errors.IsAlreadyExists(err) {
					return err
				}
				return nil
			}, "10s").Should(Succeed())

			readyResources(ctx, host.Name, namespace)

			checkedHost := &kdexv1alpha1.KDexHost{}
			var err error
			assertResourceReady(
				ctx, k8sClient, host.Name, namespace,
				checkedHost, true)

			Eventually(
				checkedHost.Status.Attributes["kdex-default-translation.translation.generation"], "5s",
			).Should(Equal("1"))

			internalTranslation := &kdexv1alpha1.KDexInternalTranslation{}
			err = k8sClient.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-%s", host.Name, translation.Name),
				Namespace: namespace,
			}, internalTranslation)

			Expect(err).NotTo(HaveOccurred())
			Expect(internalTranslation.Spec.Translations).To(HaveLen(1))
			Expect(internalTranslation.Spec.Translations[0].Lang).To(Equal("en"))
			Expect(internalTranslation.Spec.Translations[0].KeysAndValues).To(HaveLen(2))
			Expect(internalTranslation.Spec.Translations[0].KeysAndValues["brandName"]).To(Equal("KDex Tech"))
			Expect(internalTranslation.Spec.Translations[0].KeysAndValues["organization"]).To(Equal("KDex Tech Inc."))
		})
	})
})

var _ = Describe("KDexHost Consent utility page", func() {
	ctx := context.Background()
	var hostName string

	BeforeEach(func() {
		hostName = fmt.Sprintf("consent-host-%d", time.Now().UnixNano())
	})

	AfterEach(func() {
		cleanupResources(namespace)
	})

	newPage := func(name string, pageType kdexv1alpha1.KDexUtilityPageType) *kdexv1alpha1.KDexUtilityPage {
		return &kdexv1alpha1.KDexUtilityPage{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kdexv1alpha1.KDexUtilityPageSpec{
				ContentEntries: []kdexv1alpha1.ContentEntry{{
					Slot:               "main",
					ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{RawHTML: "<p>x</p>"},
				}},
				Type: pageType,
			},
		}
	}

	newHost := func(consentPage string) *kdexv1alpha1.KDexHost {
		return &kdexv1alpha1.KDexHost{
			ObjectMeta: metav1.ObjectMeta{Name: hostName, Namespace: namespace},
			Spec: kdexv1alpha1.KDexHostSpec{
				BrandName:    "KDex Tech",
				Organization: "KDex Tech Inc.",
				Routing:      kdexv1alpha1.Routing{Domains: []string{"kdex.dev"}},
				UtilityPages: &kdexv1alpha1.UtilityPages{
					ConsentRef: &kdexv1alpha1.KDexObjectReference{Kind: "KDexUtilityPage", Name: consentPage},
				},
			},
		}
	}

	It("resolves consentRef into <host>-consent and the internal host ref", func() {
		page := newPage(hostName+"-page", kdexv1alpha1.ConsentUtilityPageType)
		Expect(k8sClient.Create(ctx, page)).To(Succeed())
		assertResourceReady(ctx, k8sClient, page.Name, namespace, &kdexv1alpha1.KDexUtilityPage{}, true)

		Expect(k8sClient.Create(ctx, newHost(page.Name))).To(Succeed())

		internalPage := &kdexv1alpha1.KDexInternalUtilityPage{}
		Eventually(func() error {
			return k8sClient.Get(ctx, types.NamespacedName{Name: hostName + "-consent", Namespace: namespace}, internalPage)
		}, "15s", "500ms").Should(Succeed())
		Expect(internalPage.Spec.Type).To(Equal(kdexv1alpha1.ConsentUtilityPageType))

		Eventually(func(g Gomega) {
			internalHost := &kdexv1alpha1.KDexInternalHost{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, internalHost)).To(Succeed())
			g.Expect(internalHost.Spec.ConsentRef).NotTo(BeNil())
			g.Expect(internalHost.Spec.ConsentRef.Name).To(Equal(hostName + "-consent"))
		}, "15s", "500ms").Should(Succeed())
	})

	internalConsentPage := func() (*kdexv1alpha1.KDexInternalUtilityPage, error) {
		page := &kdexv1alpha1.KDexInternalUtilityPage{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: hostName + "-consent", Namespace: namespace}, page)
		return page, err
	}

	// A broken consentRef degrades to host-manager's built-in consent page: the
	// host is not Degraded, the internal host is still written (with no
	// consentRef), and the problem is surfaced on the host's conditions.
	expectProceedsWithoutConsent := func(reason string) {
		Eventually(func(g Gomega) {
			internalHost := &kdexv1alpha1.KDexInternalHost{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, internalHost)).To(Succeed())
			g.Expect(internalHost.Spec.ConsentRef).To(BeNil())
			g.Expect(internalHost.Spec.LoginRef).NotTo(BeNil(), "the other utility pages still resolve")

			host := &kdexv1alpha1.KDexHost{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, host)).To(Succeed())
			g.Expect(meta.IsStatusConditionTrue(host.Status.Conditions, string(kdexv1alpha1.ConditionTypeDegraded))).To(BeFalse())
			cond := meta.FindStatusCondition(host.Status.Conditions, string(kdexv1alpha1.ConditionTypeProgressing))
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Message).To(ContainSubstring("built-in consent page"))
			g.Expect(cond.Message).To(ContainSubstring(reason))
			g.Expect(host.Status.Attributes).NotTo(HaveKey("consent.utilitypage.generation"))
		}, "15s", "500ms").Should(Succeed())
	}

	It("proceeds with the built-in page when consentRef names a page of another type", func() {
		page := newPage(hostName+"-login", kdexv1alpha1.LoginUtilityPageType)
		Expect(k8sClient.Create(ctx, page)).To(Succeed())
		assertResourceReady(ctx, k8sClient, page.Name, namespace, &kdexv1alpha1.KDexUtilityPage{}, true)

		Expect(k8sClient.Create(ctx, newHost(page.Name))).To(Succeed())

		expectProceedsWithoutConsent("does not match requested type Consent")
	})

	It("proceeds with the built-in page when consentRef names a missing page", func() {
		Expect(k8sClient.Create(ctx, newHost("no-such-consent-page"))).To(Succeed())

		expectProceedsWithoutConsent("not found")
	})

	It("deletes <host>-consent when consentRef stops resolving", func() {
		page := newPage(hostName+"-page", kdexv1alpha1.ConsentUtilityPageType)
		Expect(k8sClient.Create(ctx, page)).To(Succeed())
		assertResourceReady(ctx, k8sClient, page.Name, namespace, &kdexv1alpha1.KDexUtilityPage{}, true)
		Expect(k8sClient.Create(ctx, newHost(page.Name))).To(Succeed())

		Eventually(func() error {
			_, err := internalConsentPage()
			return err
		}, "15s", "500ms").Should(Succeed())
		Eventually(func(g Gomega) {
			host := &kdexv1alpha1.KDexHost{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, host)).To(Succeed())
			g.Expect(host.Status.Attributes).To(HaveKey("consent.utilitypage.generation"))
		}, "15s", "500ms").Should(Succeed())

		Eventually(func() error {
			latest := &kdexv1alpha1.KDexHost{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, latest); err != nil {
				return err
			}
			latest.Spec.UtilityPages.ConsentRef = &kdexv1alpha1.KDexObjectReference{Kind: "KDexUtilityPage", Name: "no-such-consent-page"}
			return k8sClient.Update(ctx, latest)
		}, "10s").Should(Succeed())

		Eventually(func() bool {
			p, err := internalConsentPage()
			return errors.IsNotFound(err) || (err == nil && !p.DeletionTimestamp.IsZero())
		}, "15s", "500ms").Should(BeTrue(), "the stale internal consent page is deleted")
		expectProceedsWithoutConsent("not found")
	})

	It("defaults consentRef to the default cluster consent page", func() {
		host := newHost("")
		host.Spec.UtilityPages = nil
		Expect(k8sClient.Create(ctx, host)).To(Succeed())

		got := &kdexv1alpha1.KDexHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, got)).To(Succeed())
		Expect(got.Spec.UtilityPages.ConsentRef).NotTo(BeNil())
		Expect(got.Spec.UtilityPages.ConsentRef.Kind).To(Equal("KDexClusterUtilityPage"))
		Expect(got.Spec.UtilityPages.ConsentRef.Name).To(Equal("kdex-default-utility-page-consent"))

		Eventually(func(g Gomega) {
			page, err := internalConsentPage()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(page.Spec.Type).To(Equal(kdexv1alpha1.ConsentUtilityPageType))
		}, "15s", "500ms").Should(Succeed())
	})

	// Hosts created before Consent existed (or while the fail-open defaulter
	// was unavailable) have no stored consentRef; the controller falls back to
	// the default cluster consent page for them.
	It("resolves a stored nil consentRef to the default cluster consent page", func() {
		restore := bypassHostDefaulter(ctx)
		defer restore()

		Eventually(func(g Gomega) {
			host := newHost("")
			host.Labels = map[string]string{skipHostDefaulterLabel: "true"}
			host.Spec.UtilityPages = nil
			// What the defaulter would set and the schema requires.
			host.Spec.DefaultLang = "en"
			host.Spec.ModulePolicy = kdexv1alpha1.StrictModulePolicy
			host.Spec.Routing.Strategy = kdexv1alpha1.IngressRoutingStrategy
			g.Expect(k8sClient.Create(ctx, host)).To(Succeed())

			got := &kdexv1alpha1.KDexHost{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, got)).To(Succeed())
			if got.Spec.UtilityPages != nil && got.Spec.UtilityPages.ConsentRef != nil {
				// The apiserver had not yet seen the webhook change; try again.
				_ = k8sClient.Delete(ctx, got)
				g.Eventually(func() bool {
					return errors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, &kdexv1alpha1.KDexHost{}))
				}, "10s", "200ms").Should(BeTrue())
			}
			g.Expect(got.Spec.UtilityPages).To(BeNil(), "the defaulter was bypassed")
		}, "30s", "1s").Should(Succeed())

		Eventually(func(g Gomega) {
			page, err := internalConsentPage()
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(page.Spec.Type).To(Equal(kdexv1alpha1.ConsentUtilityPageType))

			internalHost := &kdexv1alpha1.KDexInternalHost{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, internalHost)).To(Succeed())
			g.Expect(internalHost.Spec.ConsentRef).NotTo(BeNil())
			g.Expect(internalHost.Spec.ConsentRef.Name).To(Equal(hostName + "-consent"))
		}, "15s", "500ms").Should(Succeed())
	})

	It("keeps an explicit consentRef", func() {
		Expect(k8sClient.Create(ctx, newHost("my-consent"))).To(Succeed())

		got := &kdexv1alpha1.KDexHost{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hostName, Namespace: namespace}, got)).To(Succeed())
		Expect(got.Spec.UtilityPages.ConsentRef.Kind).To(Equal("KDexUtilityPage"))
		Expect(got.Spec.UtilityPages.ConsentRef.Name).To(Equal("my-consent"))
	})
})

// skipHostDefaulterLabel marks a KDexHost the host defaulter must not see, for
// specs that need a stored object the fail-open defaulter never touched.
const skipHostDefaulterLabel = "test.kdex.dev/skip-host-defaulter"

// bypassHostDefaulter gives the KDexHost mutating webhook an objectSelector
// that skips objects labelled skipHostDefaulterLabel, and returns a func that
// restores the webhook configuration.
func bypassHostDefaulter(ctx context.Context) func() {
	const configName = "mutating-webhook-configuration"
	const webhookName = "mutate.kdexhost.kdex.dev"

	cfg := &admissionregistrationv1.MutatingWebhookConfiguration{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configName}, cfg)).To(Succeed())
	original := cfg.DeepCopy()

	found := false
	for i := range cfg.Webhooks {
		if cfg.Webhooks[i].Name == webhookName {
			cfg.Webhooks[i].ObjectSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{
				{Key: skipHostDefaulterLabel, Operator: metav1.LabelSelectorOpDoesNotExist},
			}}
			found = true
		}
	}
	Expect(found).To(BeTrue(), "the KDexHost defaulter is installed")
	Expect(k8sClient.Update(ctx, cfg)).To(Succeed())

	return func() {
		Eventually(func() error {
			latest := &admissionregistrationv1.MutatingWebhookConfiguration{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: configName}, latest); err != nil {
				return err
			}
			latest.Webhooks = original.Webhooks
			return k8sClient.Update(ctx, latest)
		}, "10s").Should(Succeed())
	}
}
