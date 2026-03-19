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
	"time"

	"os"

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

// KDexPageFooterReconciler reconciles a KDexPageFooter object
type KDexPageFooterReconciler struct {
	client.Client
	RequeueDelay time.Duration
	Scheme       *runtime.Scheme
}

func (r *KDexPageFooterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	var status *kdexv1alpha1.KDexObjectStatus
	var spec kdexv1alpha1.KDexPageFooterSpec
	var om metav1.ObjectMeta
	var o client.Object

	if req.Namespace == "" {
		var clusterPageFooter kdexv1alpha1.KDexClusterPageFooter
		if err := r.Get(ctx, req.NamespacedName, &clusterPageFooter); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &clusterPageFooter.Status
		spec = clusterPageFooter.Spec
		om = clusterPageFooter.ObjectMeta
		o = &clusterPageFooter
	} else {
		var pageFooter kdexv1alpha1.KDexPageFooter
		if err := r.Get(ctx, req.NamespacedName, &pageFooter); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &pageFooter.Status
		spec = pageFooter.Spec
		om = pageFooter.ObjectMeta
		o = &pageFooter
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
func (r *KDexPageFooterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if os.Getenv("ENABLE_WEBHOOKS") != FALSE {
		err := ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexPageFooter{}).
			WithDefaulter(&nexuswebhook.KDexPageFooterDefaulter[*kdexv1alpha1.KDexPageFooter]{}).
			WithValidator(&nexuswebhook.PageContentValidator[*kdexv1alpha1.KDexPageFooter]{}).
			Complete()
		if err != nil {
			return err
		}

		err = ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexClusterPageFooter{}).
			WithDefaulter(&nexuswebhook.KDexPageFooterDefaulter[*kdexv1alpha1.KDexClusterPageFooter]{}).
			WithValidator(&nexuswebhook.PageContentValidator[*kdexv1alpha1.KDexClusterPageFooter]{}).
			Complete()
		if err != nil {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexPageFooter{}).
		Watches(
			&kdexv1alpha1.KDexClusterPageFooter{},
			&handler.EnqueueRequestForObject{}).
		Watches(
			&kdexv1alpha1.KDexScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexPageFooter{}, &kdexv1alpha1.KDexPageFooterList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexPageFooter{}, &kdexv1alpha1.KDexPageFooterList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterPageFooter{}, &kdexv1alpha1.KDexClusterPageFooterList{}, "{.Spec.ScriptLibraryRef}")).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			LogConstructor: LogConstructor("kdexpagefooter", mgr),
		}).
		Named("kdexpagefooter").
		Complete(r)
}
