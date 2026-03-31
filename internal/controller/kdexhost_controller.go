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
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	nexuswebhook "github.com/kdex-tech/nexus-manager/internal/webhook"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	kdexWeb           = "kdex-host"
	hostFinalizerName = "kdex.dev/kdex-nexus-host-finalizer"
	hostIndexKey      = "spec.hostRef.name"
)

type helmOperation struct {
	cancel context.CancelFunc
	hash   string
}

// KDexHostReconciler reconciles a KDexHost object
type KDexHostReconciler struct {
	client.Client
	ControllerID      string
	Ctx               context.Context
	Configuration     configuration.NexusConfiguration
	HelmClientFactory func(namespace string, secrets kdexv1alpha1.Secrets, logger logr.Logger) (utils.HelmClientInterface, error)
	RequeueDelay      time.Duration
	Scheme            *runtime.Scheme

	activeHelmOperations map[types.NamespacedName]helmOperation
	mu                   sync.RWMutex
	helmClients          map[string]utils.HelmClientInterface
}

// nolint:gocyclo
func (r *KDexHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)
	log.Info("reconciling", "host", req.Name, "namespace", req.Namespace)

	var host kdexv1alpha1.KDexHost
	if err := r.Get(ctx, req.NamespacedName, &host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}

	// Defer status update
	defer func() {
		// If the object was not found, we don't need to update status
		if errors.IsNotFound(err) {
			return
		}

		latestHost := &kdexv1alpha1.KDexHost{}
		if getErr := r.Get(ctx, req.NamespacedName, latestHost); getErr != nil {
			if !errors.IsNotFound(getErr) {
				log.Error(getErr, "failed to get latest host for status update")
			}
			return
		}

		patch := client.MergeFrom(latestHost.DeepCopy())

		// Preserve Helm status attributes if they were updated by the goroutine
		if latestHost.Status.Attributes != nil {
			if host.Status.Attributes == nil {
				host.Status.Attributes = make(map[string]string)
			}
			for k, v := range latestHost.Status.Attributes {
				if strings.HasPrefix(k, "helm.release.") {
					host.Status.Attributes[k] = v
				}
			}
		}

		// Update conditions carefully - if helm failed in the cluster, don't overwrite it with something else
		currentHelmStatus := latestHost.Status.Attributes[AttributeHelmReleaseStatus]
		if currentHelmStatus == HelmStatusFailed {
			if meta.IsStatusConditionTrue(latestHost.Status.Conditions, string(kdexv1alpha1.ConditionTypeDegraded)) {
				// Helm failed in cluster, ensure our memory copy reflects this to avoid overwriting
				kdexv1alpha1.SetConditions(
					&host.Status.Conditions,
					kdexv1alpha1.ConditionStatuses{
						Progressing: metav1.ConditionFalse,
						Degraded:    metav1.ConditionTrue,
						Ready:       metav1.ConditionFalse,
					},
					kdexv1alpha1.ConditionReasonReconcileError,
					latestHost.Status.Attributes[AttributeHelmReleaseError],
				)
			}
		}

		latestHost.Status = host.Status
		latestHost.Status.ObservedGeneration = host.Generation

		if updateErr := r.Status().Patch(ctx, latestHost, patch); updateErr != nil {
			if errors.IsConflict(updateErr) {
				res = ctrl.Result{RequeueAfter: 50 * time.Millisecond}
			} else if !errors.IsNotFound(updateErr) {
				log.Error(updateErr, "failed to patch status")
				err = updateErr
				res = ctrl.Result{}
			}
		}

		log.V(2).Info("status", "status", host.Status, "err", err, "res", res)
	}()

	secrets, err := ResolveSecrets(ctx, r.Client, &host.Status, host.Namespace, host.Spec.Secrets)
	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			err.Error(),
		)
		return ctrl.Result{}, err
	}

	if host.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&host, hostFinalizerName) {
			controllerutil.AddFinalizer(&host, hostFinalizerName)
			if err := r.Update(ctx, &host); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
		}
	} else {
		if controllerutil.ContainsFinalizer(&host, hostFinalizerName) {
			internalHost := &kdexv1alpha1.KDexInternalHost{}
			err := r.Get(ctx, req.NamespacedName, internalHost)
			if err == nil {
				if internalHost.DeletionTimestamp.IsZero() {
					if err := r.Delete(ctx, internalHost); err != nil {
						return ctrl.Result{}, err
					}
				}
				// KDexInternalHost still exists. We wait.
				return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
			}
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			// Wait for internal utility pages to be gone
			internalUtilityPages := &kdexv1alpha1.KDexInternalUtilityPageList{}
			if err := r.List(ctx, internalUtilityPages, client.InNamespace(host.Namespace), client.MatchingFields{hostIndexKey: host.Name}); err != nil {
				return ctrl.Result{}, err
			}
			if len(internalUtilityPages.Items) > 0 {
				for _, iup := range internalUtilityPages.Items {
					if iup.DeletionTimestamp.IsZero() {
						if err := r.Delete(ctx, &iup); err != nil {
							return ctrl.Result{}, err
						}
					}
				}
				// KDexInternalUtilityPage still exists. We wait.
				return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
			}

			// Wait for internal translations to be gone
			internalTranslations := &kdexv1alpha1.KDexInternalTranslationList{}
			if err := r.List(ctx, internalTranslations, client.InNamespace(host.Namespace), client.MatchingFields{hostIndexKey: host.Name}); err != nil {
				return ctrl.Result{}, err
			}
			if len(internalTranslations.Items) > 0 {
				for _, it := range internalTranslations.Items {
					if it.DeletionTimestamp.IsZero() {
						if err := r.Delete(ctx, &it); err != nil {
							return ctrl.Result{}, err
						}
					}
				}
				// KDexInternalTranslation still exists. We wait.
				return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
			}

			c, err := r.getOrCreateHelmClient(
				host.Name,
				host.Namespace,
				secrets,
				log.WithName("helm"),
			)
			if err != nil {
				return ctrl.Result{}, err
			}

			// Uninstall host manager chart
			if err := c.Uninstall(host.Name); err != nil {
				log.Error(err, "failed to uninstall host manager release", "name", host.Name)
			}

			// Uninstall companion charts
			if host.Spec.Helm != nil {
				for _, companion := range host.Spec.Helm.CompanionCharts {
					if err := c.Uninstall(companion.Name); err != nil {
						log.Error(err, "failed to uninstall companion release", "name", companion.Name)
					}
				}
			}

			err = r.deleteHelmClient(host.Name, host.Namespace)
			if err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(&host, hostFinalizerName)
			if err := r.Update(ctx, &host); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	kdexv1alpha1.SetConditions(
		&host.Status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Degraded:    metav1.ConditionFalse,
			Progressing: metav1.ConditionTrue,
			Ready:       metav1.ConditionUnknown,
		},
		kdexv1alpha1.ConditionReasonReconciling,
		"Reconciling",
	)

	announcementRef, errorRef, loginRef, shouldReturn, r1, err := r.resolveUtilityPages(ctx, &host)
	if shouldReturn {
		log.Info("resolveUtilityPages requested return", "requeueAfter", r1.RequeueAfter, "err", err)
		if err == nil && r1.RequeueAfter > 0 {
			return r1, nil
		}

		message := "failed to resolve utility pages"
		if err != nil {
			message = err.Error()
		}

		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			message,
		)
		return r1, err
	}

	var themeObj runtime.Object
	if host.Spec.ThemeRef != nil {
		themeObj, shouldReturn, r1, err = ResolveKDexObjectReference(ctx, r.Client, &host, &host.Status.Conditions, host.Spec.ThemeRef, r.RequeueDelay)
		if shouldReturn {
			if err == nil && r1.RequeueAfter > 0 {
				return r1, nil
			}

			message := "failed to resolve theme"
			if err != nil {
				message = err.Error()
			}

			kdexv1alpha1.SetConditions(
				&host.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionTrue,
					Progressing: metav1.ConditionFalse,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				message,
			)
			return r1, err
		}

		if themeObj != nil {
			if host.Status.Attributes == nil {
				host.Status.Attributes = make(map[string]string)
			}
			host.Status.Attributes["theme.generation"] = fmt.Sprintf("%d", themeObj.(client.Object).GetGeneration())
		}
	}

	var scriptLibraryObj runtime.Object
	if host.Spec.ScriptLibraryRef != nil {
		var r1 ctrl.Result
		scriptLibraryObj, shouldReturn, r1, err = ResolveKDexObjectReference(ctx, r.Client, &host, &host.Status.Conditions, host.Spec.ScriptLibraryRef, r.RequeueDelay)
		if shouldReturn {
			if err == nil && r1.RequeueAfter > 0 {
				return r1, nil
			}

			message := "failed to resolve script library"
			if err != nil {
				message = err.Error()
			}

			kdexv1alpha1.SetConditions(
				&host.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionTrue,
					Progressing: metav1.ConditionFalse,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				message,
			)
			return r1, err
		}

		if scriptLibraryObj != nil {
			if host.Status.Attributes == nil {
				host.Status.Attributes = make(map[string]string)
			}
			host.Status.Attributes["scriptLibrary.generation"] = fmt.Sprintf("%d", scriptLibraryObj.(client.Object).GetGeneration())
		}
	}

	translationRefs, shouldReturn, r1, err := r.resolveTranslations(ctx, &host)
	if shouldReturn {
		log.Info("resolveTranslations requested return", "requeueAfter", r1.RequeueAfter, "err", err)
		if err == nil && r1.RequeueAfter > 0 {
			return r1, nil
		}

		message := "failed to resolve translations"
		if err != nil {
			message = err.Error()
		}

		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			message,
		)
		return r1, err
	}

	log.Info("calling reconcileHelmReleases", "host", host.Name)
	helmOp, err := r.reconcileHelmReleases(ctx, &host, secrets, log)
	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			err.Error(),
		)
		return ctrl.Result{}, err
	}

	// Always update the internal host resource, even if Helm is in progress or failed.
	// This ensures that utility page references and other attributes are updated in a timely manner,
	// which is especially important for tests that monitor these attributes.
	var internalHostOp controllerutil.OperationResult
	var internalHost *kdexv1alpha1.KDexInternalHost
	internalHostOp, internalHost, err = r.createOrUpdateInternalHostResource(ctx, &host, announcementRef, errorRef, loginRef, translationRefs)
	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			err.Error(),
		)
		return ctrl.Result{}, err
	}

	log.Info("reconciled", "host", host.Name, "namespace", host.Namespace, "helmOp", helmOp, "internalHostOp", internalHostOp)

	// Sequential readiness checks:
	// 1. Helm Operation Status
	helmStatus := host.Status.Attributes[AttributeHelmReleaseStatus]
	if helmStatus == HelmStatusInProgress {
		log.V(2).Info("helm in progress", "host", host.Name, "namespace", host.Namespace)
		return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
	}
	if helmStatus == HelmStatusFailed {
		log.V(2).Info("helm failed", "host", host.Name, "namespace", host.Namespace)
		return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
	}

	// 2. Deployment Status
	// Since we are using Helm, we'll query the deployment created by Helm.
	deployment := &appsv1.Deployment{}
	err = r.Get(ctx, req.NamespacedName, deployment)
	if err != nil {
		if errors.IsNotFound(err) {
			kdexv1alpha1.SetConditions(
				&host.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionFalse,
					Progressing: metav1.ConditionTrue,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				"Waiting for deployment to be created by Helm.",
			)
			return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
		}
		return ctrl.Result{}, err
	}

	for _, cond := range deployment.Status.Conditions {
		if cond.Type == appsv1.DeploymentAvailable && cond.Status != corev1.ConditionTrue {
			kdexv1alpha1.SetConditions(
				&host.Status.Conditions,
				kdexv1alpha1.ConditionStatuses{
					Degraded:    metav1.ConditionFalse,
					Progressing: metav1.ConditionTrue,
					Ready:       metav1.ConditionFalse,
				},
				kdexv1alpha1.ConditionReasonReconcileSuccess,
				fmt.Sprintf("Waiting for deployment %s/%s to be ready.", deployment.Namespace, deployment.Name),
			)
			return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
		}
	}

	// 3. Internal Host Readiness
	if meta.IsStatusConditionFalse(internalHost.Status.Conditions, string(kdexv1alpha1.ConditionTypeReady)) {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionFalse,
				Progressing: metav1.ConditionTrue,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			"Waiting for internal host to be ready.",
		)
		return ctrl.Result{RequeueAfter: r.RequeueDelay}, nil
	}

	if val, ok := internalHost.Status.Attributes["ingress"]; ok {
		if host.Status.Attributes == nil {
			host.Status.Attributes = make(map[string]string)
		}
		host.Status.Attributes["ingress"] = val
	}

	kdexv1alpha1.SetConditions(
		&host.Status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Degraded:    metav1.ConditionFalse,
			Progressing: metav1.ConditionFalse,
			Ready:       metav1.ConditionTrue,
		},
		kdexv1alpha1.ConditionReasonReconcileSuccess,
		"Reconciliation successful",
	)

	log.V(1).Info(
		"reconciled",
		"helmOp", helmOp,
		"internalHostOp", internalHostOp,
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *KDexHostReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if os.Getenv("ENABLE_WEBHOOKS") != FALSE {
		err := ctrl.NewWebhookManagedBy(mgr, &kdexv1alpha1.KDexHost{}).
			WithDefaulter(&nexuswebhook.KDexHostDefaulter[*kdexv1alpha1.KDexHost]{
				Configuration: r.Configuration,
			}).
			WithValidator(&nexuswebhook.KDexHostValidator[*kdexv1alpha1.KDexHost]{}).
			Complete()

		if err != nil {
			return err
		}
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kdexv1alpha1.KDexInternalTranslation{}, hostIndexKey, func(rawObj client.Object) []string {
		translation := rawObj.(*kdexv1alpha1.KDexInternalTranslation)
		if translation.Spec.HostRef.Name == "" {
			return nil
		}
		return []string{translation.Spec.HostRef.Name}
	}); err != nil {
		return err
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kdexv1alpha1.KDexInternalUtilityPage{}, hostIndexKey, func(rawObj client.Object) []string {
		utilityPage := rawObj.(*kdexv1alpha1.KDexInternalUtilityPage)
		if utilityPage.Spec.HostRef.Name == "" {
			return nil
		}
		return []string{utilityPage.Spec.HostRef.Name}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&kdexv1alpha1.KDexHost{}).
		Owns(&kdexv1alpha1.KDexInternalHost{}).
		Owns(&kdexv1alpha1.KDexInternalTranslation{}).
		Owns(&kdexv1alpha1.KDexInternalUtilityPage{}).
		Watches(
			&kdexv1alpha1.KDexScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterScriptLibrary{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.ScriptLibraryRef}")).
		Watches(
			&kdexv1alpha1.KDexFaaSAdaptor{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.FaaSAdaptorRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterFaaSAdaptor{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.FaaSAdaptorRef}")).
		Watches(
			&kdexv1alpha1.KDexTheme{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.ThemeRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterTheme{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.ThemeRef}")).
		Watches(
			&kdexv1alpha1.KDexTranslation{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.TranslationRefs[*]}")).
		Watches(
			&kdexv1alpha1.KDexClusterTranslation{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.TranslationRefs[*]}")).
		Watches(
			&kdexv1alpha1.KDexUtilityPage{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.UtilityPages.AnnouncementRef}", "{.Spec.UtilityPages.ErrorRef}", "{.Spec.UtilityPages.LoginRef}")).
		Watches(
			&kdexv1alpha1.KDexClusterUtilityPage{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.UtilityPages.AnnouncementRef}", "{.Spec.UtilityPages.ErrorRef}", "{.Spec.UtilityPages.LoginRef}")).
		Watches(
			&corev1.Secret{},
			MakeHandlerByReferencePath(r.Client, r.Scheme, &kdexv1alpha1.KDexHost{}, &kdexv1alpha1.KDexHostList{}, "{.Spec.Secrets}")).
		WithOptions(controller.TypedOptions[reconcile.Request]{
			LogConstructor: LogConstructor("kdexhost", mgr),
		}).
		Named("kdexhost").
		Complete(r)
}

func (r *KDexHostReconciler) createOrUpdateInternalHostResource(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
	announcementRef *corev1.LocalObjectReference,
	errorRef *corev1.LocalObjectReference,
	loginRef *corev1.LocalObjectReference,
	translationRefs []corev1.LocalObjectReference,
) (controllerutil.OperationResult, *kdexv1alpha1.KDexInternalHost, error) {
	internalHost := &kdexv1alpha1.KDexInternalHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:      host.Name,
			Namespace: host.Namespace,
		},
	}

	op, err := ctrl.CreateOrUpdate(ctx, r.Client, internalHost, func() error {
		if internalHost.Status.Attributes == nil {
			internalHost.Status.Attributes = make(map[string]string)
		}
		if internalHost.Annotations == nil {
			internalHost.Annotations = make(map[string]string)
		}
		maps.Copy(internalHost.Annotations, host.Annotations)
		if internalHost.Labels == nil {
			internalHost.Labels = make(map[string]string)
		}
		maps.Copy(internalHost.Labels, host.Labels)

		if internalHost.CreationTimestamp.IsZero() {
			internalHost.Labels["app.kubernetes.io/name"] = kdexWeb
			internalHost.Labels["kdex.dev/instance"] = host.Name
		}

		if internalHost.Labels == nil {
			internalHost.Labels = make(map[string]string)
		}
		internalHost.Labels["kdex.dev/generation"] = fmt.Sprintf("%d", host.Generation)
		internalHost.Spec.KDexHostSpec = host.Spec
		internalHost.Spec.AnnouncementRef = announcementRef
		internalHost.Spec.ErrorRef = errorRef
		internalHost.Spec.LoginRef = loginRef
		internalHost.Spec.InternalTranslationRefs = translationRefs

		return ctrl.SetControllerReference(host, internalHost, r.Scheme)
	})

	log := logf.FromContext(ctx)

	log.V(2).Info(
		"createOrUpdateInternalHostResource",
		"name", internalHost.Name,
		"op", op,
		"announcementRef", announcementRef,
		"errorRef", errorRef,
		"loginRef", loginRef,
		"translationRefs", translationRefs,
		"err", err,
	)

	if err != nil {
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Degraded:    metav1.ConditionTrue,
				Progressing: metav1.ConditionFalse,
				Ready:       metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileError,
			err.Error(),
		)

		return controllerutil.OperationResultNone, nil, err
	}

	return op, internalHost, nil
}

func (r *KDexHostReconciler) deleteHelmClient(name string, namespace string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.helmClients == nil {
		r.helmClients = map[string]utils.HelmClientInterface{}
	}

	delete(r.helmClients, name+"-"+namespace)

	return nil
}

func (r *KDexHostReconciler) getOrCreateHelmClient(name string, namespace string, secrets kdexv1alpha1.Secrets, logger logr.Logger) (utils.HelmClientInterface, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.helmClients == nil {
		r.helmClients = map[string]utils.HelmClientInterface{}
	}

	helmClient, ok := r.helmClients[name+"-"+namespace]

	if !ok {
		var err error
		if helmClient, err = r.HelmClientFactory(namespace, secrets, logger); err != nil {
			return nil, err
		}

		r.helmClients[name+"-"+namespace] = helmClient
	}

	return helmClient, nil
}
