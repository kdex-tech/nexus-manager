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
	"sort"
	"strconv"
	"time"

	"github.com/kdex-tech/nexus-manager/internal/validation"
	nexuswebhook "github.com/kdex-tech/nexus-manager/internal/webhook"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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

// KDexAppReconciler reconciles a KDexApp object
type KDexAppReconciler struct {
	client.Client
	Configuration           configuration.NexusConfiguration
	PackageValidatorFactory npm.PackageValidatorFactory
	RequeueDelay            time.Duration
	Scheme                  *runtime.Scheme
}

func (r *KDexAppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	var status *kdexv1alpha1.KDexObjectStatus
	var spec kdexv1alpha1.KDexAppSpec
	var om metav1.ObjectMeta
	var o client.Object

	if req.Namespace == "" {
		var clusterApp kdexv1alpha1.KDexClusterApp
		if err := r.Get(ctx, req.NamespacedName, &clusterApp); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &clusterApp.Status
		spec = clusterApp.Spec
		om = clusterApp.ObjectMeta
		o = &clusterApp
	} else {
		var app kdexv1alpha1.KDexApp
		if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		status = &app.Status
		spec = app.Spec
		om = app.ObjectMeta
		o = &app
	}

	if status.Attributes == nil {
		status.Attributes = make(map[string]string)
	}

	// Snapshot the status as observed. The deferred write is skipped when the
	// reconcile produces an identical status, so a settled KDexApp is not
	// rewritten on every pass. Without this guard the write is never a no-op:
	// resourceVersion churns and the For(KDexApp) watch self-fires, a reconcile
	// storm that also re-enqueues every downstream page. See issue #31.
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

	// Fall back to the cluster-default npm credential when the resource brings
	// no secretRef of its own. This lets bundled cluster-defaults (and any
	// resource that omits a secretRef) authenticate against an authenticated
	// DefaultNpmRegistry. Resources with their own secretRef are unaffected.
	secretRef := spec.PackageReference.SecretRef
	if secretRef == nil {
		secretRef = r.Configuration.DefaultNpmSecretRef
	}

	secret, shouldReturn, r1, err := ResolveSecret(ctx, r.Client, o, &status.Conditions, secretRef, r.RequeueDelay)
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

	if err := validation.ValidatePackageReference(&spec.PackageReference, secret, r.PackageValidatorFactory, r.Configuration.DefaultNpmRegistry); err != nil {
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

	// Validation succeeded: reset the retry counter so a future transient
	// failure gets the full retry budget instead of resuming from a stale count.
	delete(status.Attributes, "validation.attempts")

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
func (r *KDexAppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if os.Getenv("ENABLE_WEBHOOKS") != FALSE {
		err := ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexApp{}).
			WithDefaulter(&nexuswebhook.KDexAppDefaulter[*kdexv1alpha1.KDexApp]{}).
			WithValidator(&nexuswebhook.KDexAppValidator[*kdexv1alpha1.KDexApp]{}).
			Complete()

		if err != nil {
			return err
		}

		err = ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexClusterApp{}).
			WithDefaulter(&nexuswebhook.KDexAppDefaulter[*kdexv1alpha1.KDexClusterApp]{}).
			WithValidator(&nexuswebhook.KDexAppValidator[*kdexv1alpha1.KDexClusterApp]{}).
			Complete()

		if err != nil {
			return err
		}
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexApp{}).
		Watches(
			&kdexv1alpha1.KDexClusterApp{},
			&handler.EnqueueRequestForObject{}).
		Watches(
			&corev1.Secret{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexApp{}, &kdexv1alpha1.KDexAppList{}, "{.Spec.PackageReference.SecretRef}")).
		Watches(
			&corev1.Secret{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexClusterApp{}, &kdexv1alpha1.KDexClusterAppList{}, "{.Spec.PackageReference.SecretRef}")).
		// Requeue resources that fall back to the cluster-default npm credential
		// when that default Secret changes (e.g. rotation). The reference-path
		// watches above only catch resources that name a Secret explicitly.
		Watches(
			&corev1.Secret{},
			MakeHandlerForDefaultSecret(r.Client, &kdexv1alpha1.KDexAppList{}, r.Configuration.DefaultNpmSecretRef)).
		Watches(
			&corev1.Secret{},
			MakeHandlerForDefaultSecret(r.Client, &kdexv1alpha1.KDexClusterAppList{}, r.Configuration.DefaultNpmSecretRef)).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			LogConstructor: LogConstructor("kdexapp", mgr),
		}).
		Named("kdexapp").
		Complete(r)
}

// kdexObjectStatusEqual reports whether two KDexObjectStatus values are
// semantically equal, ignoring each condition's LastTransitionTime. Per the
// Kubernetes API convention, LastTransitionTime advances only on a real status
// transition, so ignoring it lets a settled reconcile recognize a genuine
// no-op and skip the status write that would otherwise churn resourceVersion
// and self-fire the For() watch. See issue #31.
func kdexObjectStatusEqual(a, b *kdexv1alpha1.KDexObjectStatus) bool {
	ac := a.DeepCopy()
	bc := b.DeepCopy()
	normalizeStatusConditionsForCompare(ac)
	normalizeStatusConditionsForCompare(bc)
	return apiequality.Semantic.DeepEqual(ac, bc)
}

// normalizeStatusConditionsForCompare zeroes each condition's
// LastTransitionTime and sorts the conditions by type so two statuses can be
// compared for a semantic (transition-time-independent) difference.
func normalizeStatusConditionsForCompare(s *kdexv1alpha1.KDexObjectStatus) {
	for i := range s.Conditions {
		s.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	sort.Slice(s.Conditions, func(i, j int) bool {
		return s.Conditions[i].Type < s.Conditions[j].Type
	})
}
