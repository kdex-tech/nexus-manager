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
	"os"
	"strconv"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/validation"
	nexuswebhook "github.com/kdex-tech/nexus-manager/internal/webhook"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	"kdex.dev/crds/npm"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// KDexScriptLibraryReconciler reconciles a KDexScriptLibrary object
type KDexScriptLibraryReconciler struct {
	client.Client
	Configuration           configuration.NexusConfiguration
	PackageValidatorFactory npm.PackageValidatorFactory
	RequeueDelay            time.Duration
	Scheme                  *runtime.Scheme
}

func (r *KDexScriptLibraryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	var status *kdexv1alpha1.KDexObjectStatus
	var spec kdexv1alpha1.KDexScriptLibrarySpec
	var om metav1.ObjectMeta
	var o client.Object

	if req.Namespace == "" {
		var clusterScriptLibrary kdexv1alpha1.KDexClusterScriptLibrary
		if err := r.Get(ctx, req.NamespacedName, &clusterScriptLibrary); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &clusterScriptLibrary.Status
		spec = clusterScriptLibrary.Spec
		om = clusterScriptLibrary.ObjectMeta
		o = &clusterScriptLibrary
	} else {
		var scriptLibrary kdexv1alpha1.KDexScriptLibrary
		if err := r.Get(ctx, req.NamespacedName, &scriptLibrary); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &scriptLibrary.Status
		spec = scriptLibrary.Spec
		om = scriptLibrary.ObjectMeta
		o = &scriptLibrary
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

	if spec.PackageReference != nil {
		secret, shouldReturn, r1, err := ResolveSecret(ctx, r.Client, o, &status.Conditions, spec.PackageReference.SecretRef, r.RequeueDelay)
		if shouldReturn {
			return r1, err
		}

		if secret != nil {
			status.Attributes["secret.generation"] = fmt.Sprintf("%d", secret.Generation)
		}

		attempts, err := strconv.Atoi(status.Attributes["validation.attempts"])
		if err != nil {
			attempts = 1
		}

		if err := validation.ValidatePackageReference(spec.PackageReference, secret, r.PackageValidatorFactory, r.Configuration.DefaultNpmRegistry); err != nil {
			kdexv1alpha1.SetConditions(
				&status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionTrue,
					Progressing: metav1.ConditionFalse,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileError,
				err.Error(),
			)

			attempts++
			status.Attributes["validation.attempts"] = strconv.Itoa(attempts)

			if attempts <= MAX_ATTEMPTS {
				return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
			}

			return ctrl.Result{}, err
		}
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
func (r *KDexScriptLibraryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if os.Getenv("ENABLE_WEBHOOKS") != FALSE {
		err := ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexScriptLibrary{}).
			WithDefaulter(&nexuswebhook.KDexScriptLibraryDefaulter[*kdexv1alpha1.KDexScriptLibrary]{}).
			WithValidator(&nexuswebhook.KDexScriptLibraryValidator[*kdexv1alpha1.KDexScriptLibrary]{}).
			Complete()

		if err != nil {
			return err
		}

		err = ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexClusterScriptLibrary{}).
			WithDefaulter(&nexuswebhook.KDexScriptLibraryDefaulter[*kdexv1alpha1.KDexClusterScriptLibrary]{}).
			WithValidator(&nexuswebhook.KDexScriptLibraryValidator[*kdexv1alpha1.KDexClusterScriptLibrary]{}).
			Complete()

		if err != nil {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexScriptLibrary{}).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			&handler.EnqueueRequestForObject{}).
		Watches(
			&corev1.Secret{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexScriptLibrary{}, &kdexv1alpha1.KDexScriptLibraryList{}, "{.Spec.PackageReference.SecretRef}")).
		Watches(
			&corev1.Secret{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterScriptLibrary{}, &kdexv1alpha1.KDexClusterScriptLibraryList{}, "{.Spec.PackageReference.SecretRef}")).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			LogConstructor: LogConstructor("kdexscriptlibrary", mgr),
		}).
		Named("kdexscriptlibrary").
		Complete(r)
}
