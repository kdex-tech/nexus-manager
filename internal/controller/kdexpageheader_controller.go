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

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nexuswebhook "github.com/kdex-tech/nexus-manager/internal/webhook"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// KDexPageHeaderReconciler reconciles a KDexPageHeader object
type KDexPageHeaderReconciler struct {
	client.Client
	RequeueDelay time.Duration
	Scheme       *runtime.Scheme
}

func (r *KDexPageHeaderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	var status *kdexv1alpha1.KDexObjectStatus
	var spec kdexv1alpha1.KDexPageHeaderSpec
	var om metav1.ObjectMeta
	var o client.Object

	if req.Namespace == "" {
		var clusterPageHeader kdexv1alpha1.KDexClusterPageHeader
		if err := r.Get(ctx, req.NamespacedName, &clusterPageHeader); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &clusterPageHeader.Status
		spec = clusterPageHeader.Spec
		om = clusterPageHeader.ObjectMeta
		o = &clusterPageHeader
	} else {
		var pageHeader kdexv1alpha1.KDexPageHeader
		if err := r.Get(ctx, req.NamespacedName, &pageHeader); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &pageHeader.Status
		spec = pageHeader.Spec
		om = pageHeader.ObjectMeta
		o = &pageHeader
	}

	if status.Attributes == nil {
		status.Attributes = make(map[string]string)
	}

	// Snapshot the status as observed. The deferred write is skipped when the
	// reconcile produces an identical status, so a settled KDexPageHeader is
	// not rewritten on every pass. Without this guard the write is never a
	// no-op: resourceVersion churns and the For(KDexPageHeader) watch
	// self-fires, a reconcile storm that also re-enqueues every downstream
	// page. See issue #31.
	observedStatus := status.DeepCopy()

	// Defer status update
	defer func() {
		status.ObservedGeneration = om.Generation
		if kdexObjectStatusEqual(observedStatus, status) {
			log.V(3).Info("status unchanged, skipping update")
			return
		}
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

	// NOTE: intentionally no unconditional "Reconciling" pulse here. Pulsing
	// Ready=Unknown/Progressing=True at the top of every reconcile and then
	// settling back moves the conditions' lastTransitionTime each pass, which
	// (with the guard above) is never even persisted but, before it, was the
	// engine of the self-fire loop. Conditions are set below once the reconcile
	// reaches a definitive state. See issue #31.

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
func (r *KDexPageHeaderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if os.Getenv("ENABLE_WEBHOOKS") != FALSE {
		err := ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexPageHeader{}).
			WithDefaulter(&nexuswebhook.KDexPageHeaderDefaulter[*kdexv1alpha1.KDexPageHeader]{}).
			WithValidator(&nexuswebhook.PageContentValidator[*kdexv1alpha1.KDexPageHeader]{}).
			Complete()
		if err != nil {
			return err
		}

		err = ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexClusterPageHeader{}).
			WithDefaulter(&nexuswebhook.KDexPageHeaderDefaulter[*kdexv1alpha1.KDexClusterPageHeader]{}).
			WithValidator(&nexuswebhook.PageContentValidator[*kdexv1alpha1.KDexClusterPageHeader]{}).
			Complete()
		if err != nil {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexPageHeader{}).
		Watches(
			&kdexv1alpha1.KDexClusterPageHeader{},
			&handler.EnqueueRequestForObject{}).
		Watches(
			&kdexv1alpha1.KDexScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexPageHeader{}, &kdexv1alpha1.KDexPageHeaderList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexPageHeader{}, &kdexv1alpha1.KDexPageHeaderList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterPageHeader{}, &kdexv1alpha1.KDexClusterPageHeaderList{}, "{.Spec.ScriptLibraryRef}")).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			LogConstructor: LogConstructor("kdexpageheader", mgr),
		}).
		Named("kdexpageheader").
		Complete(r)
}
