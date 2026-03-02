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

var _ = Describe("KDexScriptLibrary Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		AfterEach(func() {
			cleanupResources(namespace)
		})

		It("it must not become ready if it has empty script reference", func() {
			resource := &kdexv1alpha1.KDexScriptLibrary{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexScriptLibrarySpec{},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("it should become ready if library has a valid package reference", func() {
			resource := &kdexv1alpha1.KDexScriptLibrary{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexScriptLibrarySpec{
					PackageReference: &kdexv1alpha1.PackageReference{
						Name:    "@my-scope/my-package",
						Version: "1.0.0",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexScriptLibrary{}, true)
		})

		It("it should become ready if library has a both package reference and scripts", func() {
			resource := &kdexv1alpha1.KDexScriptLibrary{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexScriptLibrarySpec{
					PackageReference: &kdexv1alpha1.PackageReference{
						Name:    "@my-scope/my-package",
						Version: "1.0.0",
					},
					Scripts: []kdexv1alpha1.ScriptDef{
						{
							FootScript: false,
							Script:     `console.log('test');`,
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexScriptLibrary{}, true)
		})

		It("it should not validate if relative scripts but no static or server image specified", func() {
			resource := &kdexv1alpha1.KDexScriptLibrary{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexScriptLibrarySpec{
					Scripts: []kdexv1alpha1.ScriptDef{
						{
							ScriptSrc: `/foo.js`,
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("should not become ready when referenced secret is not found", func() {
			resource := &kdexv1alpha1.KDexScriptLibrary{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexScriptLibrarySpec{
					PackageReference: &kdexv1alpha1.PackageReference{
						Name: "@my-scope/my-package",
						SecretRef: &kdexv1alpha1.KDexObjectReference{
							Name: "non-existent-secret",
						},
						Version: "1.0.0",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexScriptLibrary{}, false)
		})

		It("should become ready when referenced secret is found", func() {
			resource := &kdexv1alpha1.KDexScriptLibrary{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexScriptLibrarySpec{
					PackageReference: &kdexv1alpha1.PackageReference{
						Name: "@my-scope/my-package",
						SecretRef: &kdexv1alpha1.KDexObjectReference{
							Name: "non-existent-secret",
						},
						Version: "1.0.0",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexScriptLibrary{}, false)

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"kdex.dev/secret-type": "npm",
					},
					Name:      "non-existent-secret",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					"username": []byte("username"),
					"password": []byte("password"),
				},
			}

			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexScriptLibrary{}, true)
		})
	})
})
