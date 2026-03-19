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

// KDexTranslationReconciler reconciles a KDexTranslation object
type KDexTranslationReconciler struct {
	client.Client
	RequeueDelay time.Duration
	Scheme       *runtime.Scheme
}

func (r *KDexTranslationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	var status *kdexv1alpha1.KDexObjectStatus
	var om metav1.ObjectMeta
	var o client.Object

	if req.Namespace == "" {
		var clusterTranslation kdexv1alpha1.KDexClusterTranslation
		if err := r.Get(ctx, req.NamespacedName, &clusterTranslation); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &clusterTranslation.Status
		om = clusterTranslation.ObjectMeta
		o = &clusterTranslation
	} else {
		var translation kdexv1alpha1.KDexTranslation
		if err := r.Get(ctx, req.NamespacedName, &translation); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &translation.Status
		om = translation.ObjectMeta
		o = &translation
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
func (r *KDexTranslationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if os.Getenv("ENABLE_WEBHOOKS") != FALSE {
		err := ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexTranslation{}).
			WithValidator(&nexuswebhook.KDexTranslationValidator[*kdexv1alpha1.KDexTranslation]{}).
			Complete()
		if err != nil {
			return err
		}

		err = ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexClusterTranslation{}).
			WithValidator(&nexuswebhook.KDexTranslationValidator[*kdexv1alpha1.KDexClusterTranslation]{}).
			Complete()
		if err != nil {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexTranslation{}).
		Watches(
			&kdexv1alpha1.KDexClusterTranslation{},
			&handler.EnqueueRequestForObject{}).
		WithOptions(
			controller.TypedOptions[reconcile.Request]{
				LogConstructor: LogConstructor("kdextranslation", mgr)}).
		Named("kdextranslation").
		Complete(r)
}
