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
	"encoding/base64"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("KDexApp Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"

		ctx := context.Background()

		AfterEach(func() {
			cleanupResources(namespace)
		})

		It("it must not become ready if it has missing package reference", func() {
			resource := &kdexv1alpha1.KDexApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexAppSpec{
					CustomElements: []kdexv1alpha1.CustomElement{
						{
							Description: "",
							Name:        "foo",
						},
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("it should become ready if it has a valid package reference", func() {
			resource := &kdexv1alpha1.KDexApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexAppSpec{
					CustomElements: []kdexv1alpha1.CustomElement{
						{
							Description: "",
							Name:        "foo",
						},
					},
					PackageReference: kdexv1alpha1.PackageReference{
						Name:    "@my-scope/my-package",
						Version: "1.0.0",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexApp{}, true)
		})

		It("should not become ready if it has a unscoped package reference", func() {
			resource := &kdexv1alpha1.KDexApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexAppSpec{
					CustomElements: []kdexv1alpha1.CustomElement{
						{
							Description: "",
							Name:        "foo",
						},
					},
					PackageReference: kdexv1alpha1.PackageReference{
						Name:    "my-scope/my-package",
						Version: "1.0.0",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).NotTo(Succeed())
		})

		It("it must not become ready if it has a valid package reference but the package is missing", func() {
			resource := &kdexv1alpha1.KDexApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexAppSpec{
					CustomElements: []kdexv1alpha1.CustomElement{
						{
							Description: "",
							Name:        "foo",
						},
					},
					PackageReference: kdexv1alpha1.PackageReference{
						Name:    "@my-scope/missing",
						Version: "1.0.0",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexApp{}, false)
		})

		It("should not become ready when referenced secret is not found", func() {
			resource := &kdexv1alpha1.KDexApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexAppSpec{
					CustomElements: []kdexv1alpha1.CustomElement{
						{
							Description: "",
							Name:        "foo",
						},
					},
					PackageReference: kdexv1alpha1.PackageReference{
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
				&kdexv1alpha1.KDexApp{}, false)
		})

		It("should become ready when referenced secret is found", func() {
			resource := &kdexv1alpha1.KDexApp{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: namespace,
				},
				Spec: kdexv1alpha1.KDexAppSpec{
					CustomElements: []kdexv1alpha1.CustomElement{
						{
							Description: "",
							Name:        "foo",
						},
					},
					PackageReference: kdexv1alpha1.PackageReference{
						Name: "@my-scope/my-package",
						SecretRef: &kdexv1alpha1.KDexObjectReference{
							Name: "existent-secret",
						},
						Version: "1.0.0",
					},
				},
			}

			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexApp{}, false)

			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"kdex.dev/secret-type": "npm",
					},
					Name:      "existent-secret",
					Namespace: namespace,
				},
				Data: map[string][]byte{
					".npmrc": []byte("//registry.npmjs.org/:_auth=" + base64.StdEncoding.EncodeToString([]byte("username:password"))),
				},
			}

			Expect(k8sClient.Create(ctx, secret)).To(Succeed())

			assertResourceReady(
				ctx, k8sClient, resourceName, namespace,
				&kdexv1alpha1.KDexApp{}, true)
		})
	})
})
