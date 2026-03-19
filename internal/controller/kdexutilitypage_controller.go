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
	"maps"
	"os"
	"time"

	nexuswebhook "github.com/kdex-tech/nexus-manager/internal/webhook"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// KDexUtilityPageReconciler reconciles a KDexUtilityPage or KDexClusterUtilityPage object
type KDexUtilityPageReconciler struct {
	client.Client
	RequeueDelay time.Duration
	Scheme       *runtime.Scheme
}

//nolint:gocyclo
func (r *KDexUtilityPageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	var status *kdexv1alpha1.KDexObjectStatus
	var spec kdexv1alpha1.KDexUtilityPageSpec
	var om metav1.ObjectMeta
	var o client.Object

	if req.Namespace == "" {
		var clusterUtilityPage kdexv1alpha1.KDexClusterUtilityPage
		if err := r.Get(ctx, req.NamespacedName, &clusterUtilityPage); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &clusterUtilityPage.Status
		spec = clusterUtilityPage.Spec
		om = clusterUtilityPage.ObjectMeta
		o = &clusterUtilityPage
	} else {
		var utilityPage kdexv1alpha1.KDexUtilityPage
		if err := r.Get(ctx, req.NamespacedName, &utilityPage); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &utilityPage.Status
		spec = utilityPage.Spec
		om = utilityPage.ObjectMeta
		o = &utilityPage
	}

	if status.Attributes == nil {
		status.Attributes = make(map[string]string)
	}

	// Defer status update
	defer func() {
		status.ObservedGeneration = om.Generation
		updateErr := r.Status().Update(ctx, o)
		if updateErr != nil {
			if errors.IsConflict(updateErr) {
				err = nil
				res = ctrl.Result{RequeueAfter: 50 * time.Millisecond}
			} else {
				err = updateErr
				res = ctrl.Result{}
			}
		}

		log.V(3).Info("status", "status", status, "err", err, "res", res)
	}()

	kdexv1alpha1.SetConditions(
		&status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Degraded:    metav1.ConditionFalse,
			Progressing: metav1.ConditionTrue,
			Ready:       metav1.ConditionUnknown,
		},
		kdexv1alpha1.ConditionReasonReconciling,
		"Reconciling",
	)

	archetypeObj, shouldReturn, r1, err := ResolveKDexObjectReference(ctx, r.Client, o, &status.Conditions, &spec.PageArchetypeRef, r.RequeueDelay)
	if shouldReturn {
		return r1, err
	}
	if archetypeObj != nil {
		status.Attributes["archetype.generation"] = fmt.Sprintf("%d", archetypeObj.GetGeneration())
	}

	var pageArchetypeSpec kdexv1alpha1.KDexPageArchetypeSpec

	switch v := archetypeObj.(type) {
	case *kdexv1alpha1.KDexPageArchetype:
		pageArchetypeSpec = v.Spec
	case *kdexv1alpha1.KDexClusterPageArchetype:
		pageArchetypeSpec = v.Spec
	}

	contents, shouldReturn, response, err := ResolveContents(ctx, r.Client, o, &status.Conditions, spec.ContentEntries, r.RequeueDelay)
	if shouldReturn {
		return response, err
	}

	for k, content := range contents {
		if content.AppObj != nil {
			status.Attributes[k+".content.generation"] = fmt.Sprintf("%d", content.AppObj.GetGeneration())
		}
	}

	headerRef := spec.OverrideHeaderRef
	headerObj, shouldReturn, r1, err := ResolveKDexObjectReference(ctx, r.Client, o, &status.Conditions, headerRef, r.RequeueDelay)
	if shouldReturn {
		return r1, err
	}
	if headerObj != nil {
		status.Attributes["header.generation"] = fmt.Sprintf("%d", headerObj.GetGeneration())
	}

	footerRef := spec.OverrideFooterRef
	footerObj, shouldReturn, r1, err := ResolveKDexObjectReference(ctx, r.Client, o, &status.Conditions, footerRef, r.RequeueDelay)
	if shouldReturn {
		return r1, err
	}
	if footerObj != nil {
		status.Attributes["footer.generation"] = fmt.Sprintf("%d", footerObj.GetGeneration())
	}

	navigationRefs := maps.Clone(pageArchetypeSpec.DefaultNavigationRefs)
	if len(spec.OverrideNavigationRefs) > 0 {
		if navigationRefs == nil {
			navigationRefs = make(map[string]*kdexv1alpha1.KDexObjectReference)
		}
		maps.Copy(navigationRefs, spec.OverrideNavigationRefs)
	}
	navigations, shouldReturn, r1, err := ResolvePageNavigations(ctx, r.Client, o, &status.Conditions, navigationRefs, r.RequeueDelay)
	if shouldReturn {
		return r1, err
	}

	for k, navigation := range navigations {
		status.Attributes[k+".navigation.generation"] = fmt.Sprintf("%d", navigation.Generation)
	}

	scriptLibraryObj, shouldReturn, r1, err := ResolveKDexObjectReference(ctx, r.Client, o, &status.Conditions, spec.ScriptLibraryRef, r.RequeueDelay)
	if shouldReturn {
		return r1, err
	}
	if scriptLibraryObj != nil {
		status.Attributes["scriptLibrary.generation"] = fmt.Sprintf("%d", scriptLibraryObj.GetGeneration())
	}

	kdexv1alpha1.SetConditions(
		&status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Degraded:    metav1.ConditionFalse,
			Progressing: metav1.ConditionFalse,
			Ready:       metav1.ConditionTrue,
		},
		kdexv1alpha1.ConditionReasonReconcileSuccess,
		"Reconciliation successful",
	)

	log.V(1).Info("reconciled")

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KDexUtilityPageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		err := ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexUtilityPage{}).
			WithDefaulter(&nexuswebhook.KDexUtilityPageDefaulter[*kdexv1alpha1.KDexUtilityPage]{}).
			WithValidator(&nexuswebhook.KDexUtilityPageValidator[*kdexv1alpha1.KDexUtilityPage]{}).
			Complete()

		if err != nil {
			return err
		}

		err = ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexClusterUtilityPage{}).
			WithDefaulter(&nexuswebhook.KDexUtilityPageDefaulter[*kdexv1alpha1.KDexClusterUtilityPage]{}).
			WithValidator(&nexuswebhook.KDexUtilityPageValidator[*kdexv1alpha1.KDexClusterUtilityPage]{}).
			Complete()

		if err != nil {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexUtilityPage{}). // Primary watch
		Watches(
			&kdexv1alpha1.KDexClusterUtilityPage{}, // Also watch cluster scoped
			&handler.EnqueueRequestForObject{}).
		Watches(
			&kdexv1alpha1.KDexApp{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.ContentEntries[*].AppRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterApp{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.ContentEntries[*].AppRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterApp{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterUtilityPage{}, &kdexv1alpha1.KDexClusterUtilityPageList{}, "{.Spec.ContentEntries[*].AppRef}")).
		Watches(
			&kdexv1alpha1.KDexPageArchetype{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.PageArchetypeRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterPageArchetype{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.PageArchetypeRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterPageArchetype{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterUtilityPage{}, &kdexv1alpha1.KDexClusterUtilityPageList{}, "{.Spec.PageArchetypeRef}")).
		Watches(
			&kdexv1alpha1.KDexPageFooter{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.OverrideFooterRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterPageFooter{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.OverrideFooterRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterPageFooter{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterUtilityPage{}, &kdexv1alpha1.KDexClusterUtilityPageList{}, "{.Spec.OverrideFooterRef}")).
		Watches(
			&kdexv1alpha1.KDexPageHeader{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.OverrideHeaderRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterPageHeader{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.OverrideHeaderRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterPageHeader{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterUtilityPage{}, &kdexv1alpha1.KDexClusterUtilityPageList{}, "{.Spec.OverrideHeaderRef}")).
		Watches(
			&kdexv1alpha1.KDexPageNavigation{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.OverrideNavigationRefs.*}")).
		Watches(
			&kdexv1alpha1.KDexClusterPageNavigation{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.OverrideNavigationRefs.*}")).
		Watches(
			&kdexv1alpha1.KDexClusterPageNavigation{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterUtilityPage{}, &kdexv1alpha1.KDexClusterUtilityPageList{}, "{.Spec.OverrideNavigationRefs.*}")).
		Watches(
			&kdexv1alpha1.KDexScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexUtilityPage{}, &kdexv1alpha1.KDexUtilityPageList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterUtilityPage{}, &kdexv1alpha1.KDexClusterUtilityPageList{}, "{.Spec.ScriptLibraryRef}")).
		WithOptions(
			controller.TypedOptions[reconcile.Request]{
				LogConstructor: LogConstructor("kdexutilitypage", mgr)}).
		Named("kdexutilitypage").
		Complete(r)
}
