package controller

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

var _ = Describe("Consent utility page CEL", func() {
	ctx := context.Background()

	const consentRuleMessage = "a Consent page cannot reference scripts"

	newPage := func(pageType kdexv1alpha1.KDexUtilityPageType, mutate func(*kdexv1alpha1.KDexUtilityPageSpec)) *kdexv1alpha1.KDexClusterUtilityPage {
		page := &kdexv1alpha1.KDexClusterUtilityPage{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("cel-%d", time.Now().UnixNano())},
			Spec: kdexv1alpha1.KDexUtilityPageSpec{
				Type: pageType,
				ContentEntries: []kdexv1alpha1.ContentEntry{{
					Slot:               "main",
					ContentEntryStatic: kdexv1alpha1.ContentEntryStatic{RawHTML: "<p>x</p>"},
				}},
			},
		}
		if mutate != nil {
			mutate(&page.Spec)
		}
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, page) })
		return page
	}

	scriptLibrary := func(s *kdexv1alpha1.KDexUtilityPageSpec) {
		s.ScriptLibraryRef = &kdexv1alpha1.KDexObjectReference{Kind: "KDexScriptLibrary", Name: "lib"}
	}

	It("accepts a Consent page with only rawHTML content", func() {
		Expect(k8sClient.Create(ctx, newPage(kdexv1alpha1.ConsentUtilityPageType, nil))).To(Succeed())
	})

	It("rejects a Consent page with a scriptLibraryRef", func() {
		err := k8sClient.Create(ctx, newPage(kdexv1alpha1.ConsentUtilityPageType, scriptLibrary))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(consentRuleMessage))
	})

	It("rejects a Consent page with appRef content", func() {
		err := k8sClient.Create(ctx, newPage(kdexv1alpha1.ConsentUtilityPageType, func(s *kdexv1alpha1.KDexUtilityPageSpec) {
			s.ContentEntries = append(s.ContentEntries, kdexv1alpha1.ContentEntry{
				Slot: "extra",
				ContentEntryApp: kdexv1alpha1.ContentEntryApp{
					AppRef:            &kdexv1alpha1.KDexObjectReference{Kind: "KDexApp", Name: "app"},
					CustomElementName: "x-app",
				},
			})
		}))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring(consentRuleMessage))
	})

	It("still accepts a Login page with a scriptLibraryRef", func() {
		Expect(k8sClient.Create(ctx, newPage(kdexv1alpha1.LoginUtilityPageType, scriptLibrary))).To(Succeed())
	})
})
