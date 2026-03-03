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
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	nexuswebhook "github.com/kdex-tech/nexus-manager/internal/webhook"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
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

// KDexHostReconciler reconciles a KDexHost object
type KDexHostReconciler struct {
	client.Client
	Configuration configuration.NexusConfiguration
	RequeueDelay  time.Duration
	Scheme        *runtime.Scheme

	mu                 sync.RWMutex
	memoizedDeployment *appsv1.DeploymentSpec
	memoizedService    *corev1.ServiceSpec
}

// nolint:gocyclo
func (r *KDexHostReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	log := logf.FromContext(ctx)

	var host kdexv1alpha1.KDexHost
	if err := r.Get(ctx, req.NamespacedName, &host); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}

	// Defer status update
	defer func() {
		host.Status.ObservedGeneration = host.Generation
		if updateErr := r.Status().Update(ctx, &host); updateErr != nil {
			err = updateErr
			res = ctrl.Result{}
		}

		log.V(2).Info("status", "status", host.Status, "err", err, "res", res)
	}()

	if host.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&host, hostFinalizerName) {
			controllerutil.AddFinalizer(&host, hostFinalizerName)
			if err := r.Update(ctx, &host); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
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
				return ctrl.Result{Requeue: true}, nil
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
				return ctrl.Result{Requeue: true}, nil
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
				return ctrl.Result{Requeue: true}, nil
			}

			deployment := &appsv1.Deployment{}
			err = r.Get(ctx, req.NamespacedName, deployment)
			if err == nil {
				if deployment.DeletionTimestamp.IsZero() {
					if err := r.Delete(ctx, deployment); err != nil {
						return ctrl.Result{}, err
					}
				}
				// Deployment still exists. We wait.
				return ctrl.Result{Requeue: true}, nil
			}
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}

			// Deployment is gone. Clean up RBAC finalizers.
			if err := r.cleanupRbacFinalizers(ctx, &host); err != nil {
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

	// Resolve direct requirements from host spec

	announcementRef, errorRef, loginRef, shouldReturn, err := r.resolveUtilityPages(ctx, &host)
	if shouldReturn {
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

	themeObj, shouldReturn, _, err := ResolveKDexObjectReference(ctx, r.Client, &host, &host.Status.Conditions, host.Spec.ThemeRef, r.RequeueDelay)
	if shouldReturn {
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

	if themeObj != nil {
		host.Status.Attributes["theme.generation"] = fmt.Sprintf("%d", themeObj.GetGeneration())
	}

	scriptLibraryObj, shouldReturn, _, err := ResolveKDexObjectReference(ctx, r.Client, &host, &host.Status.Conditions, host.Spec.ScriptLibraryRef, r.RequeueDelay)
	if shouldReturn {
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

	if scriptLibraryObj != nil {
		host.Status.Attributes["scriptLibrary.generation"] = fmt.Sprintf("%d", scriptLibraryObj.GetGeneration())
	}

	translationRefs, shouldReturn, err := r.resolveTranslations(ctx, &host)
	if shouldReturn {
		if err == nil {
			err = fmt.Errorf("failed to resolve translations")
		}
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

	configMapOp, configHash, err := r.createOrUpdateConfigMap(ctx, &host)
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

	serviceAccountOp, err := r.createOrUpdateServiceAccount(ctx, &host)
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

	clusterRoleBindingOp, err := r.createOrUpdateClusterRoleBinding(ctx, &host)
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

	deploymentOp, deployment, err := r.createOrUpdateDeployment(ctx, &host, configHash)
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

	serviceOp, err := r.createOrUpdateService(ctx, &host)
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
			return ctrl.Result{RequeueAfter: r.RequeueDelay}, err
		}
	}

	internalHostOp, internalHost, err := r.createOrUpdateInternalHostResource(ctx, &host, announcementRef, errorRef, loginRef, translationRefs)
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
		return ctrl.Result{RequeueAfter: r.RequeueDelay}, err
	}

	if val, ok := internalHost.Status.Attributes["ingress"]; ok {
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
		"configMapOp", configMapOp,
		"serviceAccountOp", serviceAccountOp,
		"clusterRoleBindingOp", clusterRoleBindingOp,
		"deploymentOp", deploymentOp,
		"serviceOp", serviceOp,
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
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&kdexv1alpha1.KDexInternalHost{}).
		Owns(&kdexv1alpha1.KDexInternalTranslation{}).
		Owns(&kdexv1alpha1.KDexInternalUtilityPage{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
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
		WithOptions(controller.TypedOptions[reconcile.Request]{
			LogConstructor: LogConstructor("kdexhost", mgr),
		}).
		Named("kdexhost").
		Complete(r)
}

func (r *KDexHostReconciler) cleanupRbacFinalizers(ctx context.Context, host *kdexv1alpha1.KDexHost) error {
	// ClusterRoleBinding
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{}
	clusterRoleBinding.Name = fmt.Sprintf("%s-%s", host.Name, host.Namespace)
	if err := r.Get(ctx, types.NamespacedName{Name: clusterRoleBinding.Name}, clusterRoleBinding); err == nil {
		if controllerutil.RemoveFinalizer(clusterRoleBinding, hostFinalizerName) {
			if err := r.Update(ctx, clusterRoleBinding); err != nil {
				return err
			}
		}
	} else if !errors.IsNotFound(err) {
		return err
	}

	// ServiceAccount
	serviceAccount := &corev1.ServiceAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: host.Name, Namespace: host.Namespace}, serviceAccount); err == nil {
		if controllerutil.RemoveFinalizer(serviceAccount, hostFinalizerName) {
			if err := r.Update(ctx, serviceAccount); err != nil {
				return err
			}
		}
	} else if !errors.IsNotFound(err) {
		return err
	}

	return nil
}

func (r *KDexHostReconciler) getConfiguration() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	codecs := serializer.NewCodecFactory(r.Scheme)

	info, ok := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), "application/yaml")
	if !ok {
		return "", fmt.Errorf("no YAML serializer found")
	}

	encoder := codecs.EncoderForVersion(info.Serializer, configuration.GroupVersion)

	var buf bytes.Buffer
	if err := encoder.Encode(&r.Configuration, &buf); err != nil {
		return "", fmt.Errorf("failed to encode object to YAML: %w", err)
	}

	return buf.String(), nil
}

func (r *KDexHostReconciler) getMemoizedDeployment() *appsv1.DeploymentSpec {
	r.mu.RLock()

	if r.memoizedDeployment != nil {
		r.mu.RUnlock()
		return r.memoizedDeployment
	}

	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()

	r.memoizedDeployment = r.Configuration.HostDefault.Deployment.DeepCopy()

	return r.memoizedDeployment
}

func (r *KDexHostReconciler) getMemoizedService() *corev1.ServiceSpec {
	r.mu.RLock()

	if r.memoizedService != nil {
		r.mu.RUnlock()
		return r.memoizedService
	}

	r.mu.RUnlock()
	r.mu.Lock()
	defer r.mu.Unlock()

	r.memoizedService = r.Configuration.HostDefault.Service.DeepCopy()

	return r.memoizedService
}

func (r *KDexHostReconciler) createOrUpdateConfigMap(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
) (controllerutil.OperationResult, string, error) {
	configString, err := r.getConfiguration()
	if err != nil {
		return controllerutil.OperationResultNone, "", err
	}

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      host.Name,
			Namespace: host.Namespace,
		},
	}

	op, err := ctrl.CreateOrUpdate(ctx, r.Client, configMap, func() error {
		if configMap.CreationTimestamp.IsZero() {
			if configMap.Annotations == nil {
				configMap.Annotations = make(map[string]string)
			}
			maps.Copy(configMap.Annotations, host.Annotations)
			if configMap.Labels == nil {
				configMap.Labels = make(map[string]string)
			}
			maps.Copy(configMap.Labels, host.Labels)

			configMap.Labels["app.kubernetes.io/name"] = kdexWeb
			configMap.Labels["kdex.dev/instance"] = host.Name
		}

		configMap.Data = map[string]string{
			"config.yaml": configString,
		}

		return ctrl.SetControllerReference(host, configMap, r.Scheme)
	})

	log := logf.FromContext(ctx)

	log.V(2).Info(
		"createOrUpdateConfigMap",
		"name", configMap.Name,
		"op", op,
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
		return controllerutil.OperationResultNone, "", err
	}

	return op, Hash(configString), nil
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
		if internalHost.CreationTimestamp.IsZero() {
			if internalHost.Annotations == nil {
				internalHost.Annotations = make(map[string]string)
			}
			maps.Copy(internalHost.Annotations, host.Annotations)
			if internalHost.Labels == nil {
				internalHost.Labels = make(map[string]string)
			}
			maps.Copy(internalHost.Labels, host.Labels)

			internalHost.Labels["app.kubernetes.io/name"] = kdexWeb
			internalHost.Labels["kdex.dev/instance"] = host.Name
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

func (r *KDexHostReconciler) createOrUpdateDeployment(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
	configHash string,
) (controllerutil.OperationResult, *appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      host.Name,
			Namespace: host.Namespace,
		},
	}

	op, err := ctrl.CreateOrUpdate(
		ctx,
		r.Client,
		deployment,
		func() error {
			if deployment.CreationTimestamp.IsZero() {
				if deployment.Annotations == nil {
					deployment.Annotations = make(map[string]string)
				}
				maps.Copy(deployment.Annotations, host.Annotations)
				if deployment.Labels == nil {
					deployment.Labels = make(map[string]string)
				}
				maps.Copy(deployment.Labels, host.Labels)

				deployment.Labels["app.kubernetes.io/name"] = kdexWeb
				deployment.Labels["kdex.dev/instance"] = host.Name

				deployment.Spec = *r.getMemoizedDeployment().DeepCopy()

				if deployment.Spec.Selector == nil {
					deployment.Spec.Selector = &metav1.LabelSelector{
						MatchLabels: map[string]string{},
					}
				}
				deployment.Spec.Selector.MatchLabels["app.kubernetes.io/name"] = kdexWeb
				deployment.Spec.Selector.MatchLabels["kdex.dev/instance"] = host.Name

				if deployment.Spec.Template.Annotations == nil {
					deployment.Spec.Template.Annotations = make(map[string]string)
				}
				if deployment.Spec.Template.Labels == nil {
					deployment.Spec.Template.Labels = make(map[string]string)
				}
				deployment.Spec.Template.Labels["app.kubernetes.io/name"] = kdexWeb
				deployment.Spec.Template.Labels["kdex.dev/instance"] = host.Name

				deployment.Spec.Template.Spec = *r.getMemoizedDeployment().Template.Spec.DeepCopy()
			}

			deployment.Annotations["checksum/config"] = configHash
			deployment.Spec.Template.Annotations["checksum/config"] = configHash

			foundFocalHost := false
			foundServiceName := false
			for idx, value := range deployment.Spec.Template.Spec.Containers[0].Args {
				if strings.Contains(value, "--focal-host") {
					deployment.Spec.Template.Spec.Containers[0].Args[idx] = "--focal-host=" + host.Name
					foundFocalHost = true
				}
				if strings.Contains(value, "--service-name") {
					deployment.Spec.Template.Spec.Containers[0].Args[idx] = "--service-name=" + host.Name
					foundServiceName = true
				}
			}
			for idx, value := range deployment.Spec.Template.Spec.Containers[0].Command {
				if strings.Contains(value, "--focal-host") {
					deployment.Spec.Template.Spec.Containers[0].Command[idx] = "--focal-host=" + host.Name
					foundFocalHost = true
				}
				if strings.Contains(value, "--service-name") {
					deployment.Spec.Template.Spec.Containers[0].Command[idx] = "--service-name=" + host.Name
					foundServiceName = true
				}
			}
			if !foundFocalHost {
				deployment.Spec.Template.Spec.Containers[0].Args = append(deployment.Spec.Template.Spec.Containers[0].Args, "--focal-host="+host.Name)
			}
			if !foundServiceName {
				deployment.Spec.Template.Spec.Containers[0].Args = append(deployment.Spec.Template.Spec.Containers[0].Args, "--service-name="+host.Name)
			}

			deployment.Spec.Template.Spec.Containers[0].Name = host.Name
			deployment.Spec.Template.Spec.ServiceAccountName = host.Name

			for idx, volume := range deployment.Spec.Template.Spec.Volumes {
				if volume.Name == "config" {
					deployment.Spec.Template.Spec.Volumes[idx].ConfigMap.Name = host.Name
				}
			}

			if len(host.Spec.Env) > 0 {
				deployment.Spec.Template.Spec.Containers[0].Env = MergeEnvVars(deployment.Spec.Template.Spec.Containers[0].Env, host.Spec.Env)
			}

			if host.Spec.Resources.Size() > 0 {
				deployment.Spec.Template.Spec.Containers[0].Resources = host.Spec.Resources
			}

			if host.Spec.Replicas != nil {
				deployment.Spec.Replicas = host.Spec.Replicas
			}

			return ctrl.SetControllerReference(host, deployment, r.Scheme)
		},
	)

	log := logf.FromContext(ctx)

	log.V(2).Info(
		"createOrUpdateDeployment",
		"name", deployment.Name,
		"op", op,
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

	return op, deployment, nil
}

func (r *KDexHostReconciler) createOrUpdateClusterRoleBinding(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
) (controllerutil.OperationResult, error) {
	clusterRoleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", host.Name, host.Namespace),
		},
	}

	op, err := ctrl.CreateOrUpdate(
		ctx,
		r.Client,
		clusterRoleBinding,
		func() error {
			if clusterRoleBinding.CreationTimestamp.IsZero() {
				if clusterRoleBinding.Annotations == nil {
					clusterRoleBinding.Annotations = make(map[string]string)
				}
				maps.Copy(clusterRoleBinding.Annotations, host.Annotations)
				if clusterRoleBinding.Labels == nil {
					clusterRoleBinding.Labels = make(map[string]string)
				}
				maps.Copy(clusterRoleBinding.Labels, host.Labels)

				clusterRoleBinding.Labels["app.kubernetes.io/name"] = kdexWeb
				clusterRoleBinding.Labels["kdex.dev/instance"] = host.Name
			}

			clusterRoleBinding.RoleRef = *r.Configuration.HostDefault.RoleRef.DeepCopy()
			clusterRoleBinding.Subjects = []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      host.Name,
					Namespace: host.Namespace,
				},
			}

			controllerutil.AddFinalizer(clusterRoleBinding, hostFinalizerName)
			return nil
		},
	)

	log := logf.FromContext(ctx)

	log.V(2).Info(
		"createOrUpdateClusterRoleBinding",
		"name", clusterRoleBinding.Name,
		"op", op,
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

		return controllerutil.OperationResultNone, err
	}

	return op, nil
}

func (r *KDexHostReconciler) createOrUpdateService(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
) (controllerutil.OperationResult, error) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      host.Name,
			Namespace: host.Namespace,
		},
	}

	op, err := ctrl.CreateOrUpdate(
		ctx,
		r.Client,
		service,
		func() error {
			if service.CreationTimestamp.IsZero() {
				if service.Annotations == nil {
					service.Annotations = make(map[string]string)
				}
				maps.Copy(service.Annotations, host.Annotations)
				if service.Labels == nil {
					service.Labels = make(map[string]string)
				}
				maps.Copy(service.Labels, host.Labels)

				service.Labels["app.kubernetes.io/name"] = kdexWeb
				service.Labels["kdex.dev/instance"] = host.Name

				service.Spec = *r.getMemoizedService().DeepCopy()

				if service.Spec.Selector == nil {
					service.Spec.Selector = make(map[string]string)
				}

				service.Spec.Selector["app.kubernetes.io/name"] = kdexWeb
				service.Spec.Selector["kdex.dev/instance"] = host.Name
			}

			return ctrl.SetControllerReference(host, service, r.Scheme)
		},
	)

	log := logf.FromContext(ctx)

	log.V(2).Info(
		"createOrUpdateService",
		"name", service.Name,
		"op", op,
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

		return controllerutil.OperationResultNone, err
	}

	return op, nil
}

func (r *KDexHostReconciler) createOrUpdateServiceAccount(
	ctx context.Context,
	host *kdexv1alpha1.KDexHost,
) (controllerutil.OperationResult, error) {
	serviceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      host.Name,
			Namespace: host.Namespace,
		},
	}

	op, err := ctrl.CreateOrUpdate(
		ctx,
		r.Client,
		serviceAccount,
		func() error {
			if serviceAccount.CreationTimestamp.IsZero() {
				if serviceAccount.Annotations == nil {
					serviceAccount.Annotations = make(map[string]string)
				}
				maps.Copy(serviceAccount.Annotations, host.Annotations)
				if serviceAccount.Labels == nil {
					serviceAccount.Labels = make(map[string]string)
				}
				maps.Copy(serviceAccount.Labels, host.Labels)

				serviceAccount.Labels["app.kubernetes.io/name"] = kdexWeb
				serviceAccount.Labels["kdex.dev/instance"] = host.Name
			}

			controllerutil.AddFinalizer(serviceAccount, hostFinalizerName)
			return ctrl.SetControllerReference(host, serviceAccount, r.Scheme)
		},
	)

	log := logf.FromContext(ctx)

	log.V(2).Info(
		"createOrUpdateServiceAccount",
		"name", serviceAccount.Name,
		"op", op,
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

		return controllerutil.OperationResultNone, err
	}

	return op, nil
}
