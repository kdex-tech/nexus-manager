package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Proves the KDexRoleBinding CRD installs and the apiserver admits/rejects
// spec.subjectKinds as specified (host-manager#255). Objects are built as
// unstructured because the typed field is omitempty: an explicit empty list
// (the minItems case) could not otherwise reach the apiserver.
var _ = Describe("KDexRoleBinding subjectKinds", func() {
	ctx := context.Background()
	var seq int

	// binding returns an otherwise-valid KDexRoleBinding; kinds==nil omits
	// spec.subjectKinds entirely.
	binding := func(kinds []any) *unstructured.Unstructured {
		seq++
		spec := map[string]any{
			"hostRef": map[string]any{"name": "some-host"},
			"roles":   []any{"some-role"},
			"subject": "someone",
		}
		if kinds != nil {
			spec["subjectKinds"] = kinds
		}
		u := &unstructured.Unstructured{Object: map[string]any{
			"spec": spec,
		}}
		u.SetAPIVersion(kdexv1alpha1.GroupVersion.String())
		u.SetKind("KDexRoleBinding")
		u.SetName(fmt.Sprintf("subject-kinds-%d", seq))
		u.SetNamespace(namespace)
		return u
	}

	AfterEach(func() {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion(kdexv1alpha1.GroupVersion.String())
		list.SetKind("KDexRoleBindingList")
		Expect(k8sClient.List(ctx, list)).To(Succeed())
		for i := range list.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &list.Items[i]))).To(Succeed())
		}
	})

	DescribeTable("admits and stores",
		func(kinds []any, want []any) {
			b := binding(kinds)
			Expect(k8sClient.Create(ctx, b)).To(Succeed())

			got := &unstructured.Unstructured{}
			got.SetAPIVersion(b.GetAPIVersion())
			got.SetKind(b.GetKind())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: b.GetName()}, got)).To(Succeed())

			stored, found, err := unstructured.NestedSlice(got.Object, "spec", "subjectKinds")
			Expect(err).NotTo(HaveOccurred())
			if want == nil {
				Expect(found).To(BeFalse())
				return
			}
			Expect(found).To(BeTrue(), "subjectKinds was pruned")
			Expect(stored).To(Equal(want))
		},
		Entry("field absent", nil, nil),
		Entry("[User]", []any{"User"}, []any{"User"}),
		Entry("[Client]", []any{"Client"}, []any{"Client"}),
		Entry("[User, Client]", []any{"User", "Client"}, []any{"User", "Client"}),
	)

	DescribeTable("rejects",
		func(kinds []any) {
			err := k8sClient.Create(ctx, binding(kinds))
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected Invalid, got: %v", err)
		},
		Entry("empty list (minItems)", []any{}),
		Entry("unknown kind (enum)", []any{"ServiceAccount"}),
		Entry("duplicate (set)", []any{"User", "User"}),
		// A 2-value enum plus listType=set makes any nine entries a duplicate
		// too, so this cannot isolate maxItems; kdex-crds' schema test pins
		// maxItems == 8 exactly.
		Entry("nine entries (maxItems or set duplicate)",
			[]any{"User", "Client", "User", "Client", "User", "Client", "User", "Client", "User"}),
	)
})
