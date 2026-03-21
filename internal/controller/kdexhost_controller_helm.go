package controller

import (
	"context"
	"fmt"
	"strconv"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	"helm.sh/helm/v4/pkg/chart/common"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	AttributeHelmReleaseStatus     = "helm.release.status"
	AttributeHelmReleaseGeneration = "helm.release.generation"
	AttributeHelmReleaseError      = "helm.release.error"
	AttributeHelmReleaseOwner      = "helm.release.owner"

	HelmStatusInProgress = "in_progress"
	HelmStatusCompleted  = "completed"
	HelmStatusFailed     = "failed"
)

func (r *KDexHostReconciler) trySetHelmOperationActive(key types.NamespacedName, cancel context.CancelFunc, generation int64) bool {
	r.mu.Lock()
	if r.activeHelmOperations == nil {
		r.activeHelmOperations = make(map[types.NamespacedName]helmOperation)
	}
	if old, active := r.activeHelmOperations[key]; active {
		if old.generation == generation {
			r.mu.Unlock()
			return false
		}
		// Cancel old generation if it's still running
		// We do this outside the lock to avoid any potential deadlock if cancel()
		// synchronously triggers something that needs the lock.
		r.mu.Unlock()
		old.cancel()

		r.mu.Lock()
	}
	r.activeHelmOperations[key] = helmOperation{cancel, generation}
	r.mu.Unlock()
	return true
}

func (r *KDexHostReconciler) clearHelmOperationActive(key types.NamespacedName, generation int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeHelmOperations != nil {
		if op, exists := r.activeHelmOperations[key]; exists && op.generation == generation {
			delete(r.activeHelmOperations, key)
		}
	}
}

func (r *KDexHostReconciler) isHelmOperationActive(key types.NamespacedName, generation int64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.activeHelmOperations == nil {
		return false
	}
	op, active := r.activeHelmOperations[key]
	return active && op.generation == generation
}

func (r *KDexHostReconciler) reconcileHelmReleases(ctx context.Context, host *kdexv1alpha1.KDexHost, log logr.Logger) (controllerutil.OperationResult, error) {
	log.V(2).Info("reconcileHelmReleases#1", "host", host.Name, "namespace", host.Namespace)

	// Fetch a fresh copy of the host to ensure we have the latest status and generation
	latestHost := &kdexv1alpha1.KDexHost{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(host), latestHost); err != nil {
		log.Error(err, "failed to get latest host", "namespace", host.Namespace, "name", host.Name)
		return controllerutil.OperationResultNone, fmt.Errorf("failed to get latest host: %w", err)
	}

	// Update the local object's generation and resource version to match the latest from the server.
	// We DO NOT overwrite host.Status directly because it may contain changes from earlier in the reconcile loop
	// (like resolved utility page generations) that haven't been persisted yet.
	host.Generation = latestHost.Generation
	host.ResourceVersion = latestHost.ResourceVersion

	// Check if a Helm operation is already in progress using the latest status from the server
	status := latestHost.Status.Attributes[AttributeHelmReleaseStatus]
	genStr := latestHost.Status.Attributes[AttributeHelmReleaseGeneration]
	gen, _ := strconv.ParseInt(genStr, 10, 64)

	key := client.ObjectKeyFromObject(host)
	if status == HelmStatusInProgress {
		// If it's marked in progress in status, check if we actually have a goroutine running for it
		if r.isHelmOperationActive(key, host.Generation) {
			// Still working, requeue
			log.V(2).Info("reconcileHelmReleases#2", "host", host.Name, "namespace", host.Namespace)
			return controllerutil.OperationResultNone, nil
		}

		// If not active in memory, it means either:
		// 1. This controller instance just started and we are recovering a zombie.
		// 2. Another controller instance was running it and we are taking over (if it's no longer leader).
		owner := latestHost.Status.Attributes[AttributeHelmReleaseOwner]
		if owner == r.ControllerID {
			log.V(2).Info("Found zombie Helm operation in status from this instance, restarting", "host", host.Name, "namespace", host.Namespace)
		} else {
			log.V(2).Info("Found zombie Helm operation in status from another instance, taking over", "host", host.Name, "namespace", host.Namespace, "previousOwner", owner)
		}
	}

	if (status == HelmStatusCompleted || status == HelmStatusFailed) && gen == host.Generation {
		// Already attempted for this generation
		log.Info("reconcileHelmReleases#3", "host", host.Name, "namespace", host.Namespace)
		return controllerutil.OperationResultNone, nil
	}

	// Start a new Helm operation asynchronously
	log.V(2).Info("reconcileHelmReleases#4", "host", host.Name, "namespace", host.Namespace)
	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}
	host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusInProgress
	host.Status.Attributes[AttributeHelmReleaseGeneration] = strconv.FormatInt(host.Generation, 10)
	host.Status.Attributes[AttributeHelmReleaseOwner] = r.ControllerID
	delete(host.Status.Attributes, AttributeHelmReleaseError)

	kdexv1alpha1.SetConditions(
		&host.Status.Conditions,
		kdexv1alpha1.ConditionStatuses{
			Progressing: metav1.ConditionTrue,
			Ready:       metav1.ConditionFalse,
		},
		kdexv1alpha1.ConditionReasonReconciling,
		"Helm installation in progress",
	)

	// We need to update the status now before starting the goroutine.
	// We use Patch to avoid conflicts with the defer block.
	patch := client.MergeFrom(latestHost.DeepCopy())
	latestHost.Status = host.Status
	if err := r.Status().Patch(ctx, latestHost, patch); err != nil {
		log.Error(err, "failed to patch status for helm progress", "namespace", host.Namespace, "name", host.Name)
		return controllerutil.OperationResultNone, fmt.Errorf("failed to patch status for helm progress: %w", err)
	}
	// Update the local object with the new resource version
	host.ResourceVersion = latestHost.ResourceVersion

	// Start the goroutine with a tracked context
	asyncCtx, cancel := context.WithCancel(r.Ctx)
	if !r.trySetHelmOperationActive(key, cancel, host.Generation) {
		cancel()
		log.V(2).Info("reconcileHelmReleases#5", "host", host.Name, "namespace", host.Namespace, "msg", "already active")
		return controllerutil.OperationResultNone, nil
	}

	go r.runAsyncHelmReconcile(asyncCtx, host.Namespace, host.Name, host.Generation, host.Spec.ServiceAccountSecrets, log)

	return controllerutil.OperationResultUpdated, nil
}

func (r *KDexHostReconciler) runAsyncHelmReconcile(
	ctx context.Context,
	namespace, name string,
	generation int64,
	serviceAccountSecrets kdexv1alpha1.ServiceAccountSecrets,
	log logr.Logger,
) {
	log.V(2).Info("runAsyncHelmReconcile#1", "namespace", namespace, "name", name)

	// Fetch a fresh copy of the host
	host := &kdexv1alpha1.KDexHost{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, host); err != nil {
		log.Error(err, "failed to get host", "namespace", namespace, "name", name)
		r.clearHelmOperationActive(client.ObjectKey{Namespace: namespace, Name: name}, generation)
		return // Host might have been deleted
	}

	if r.HelmClientFactory == nil {
		err := fmt.Errorf("HelmClientFactory is nil")
		log.Error(err, "controller misconfigured")
		r.updateHelmStatus(ctx, namespace, name, generation, err, log)
		return
	}

	c, err := r.HelmClientFactory(
		host.Namespace,
		serviceAccountSecrets,
		logr.ToSlogHandler(log.WithName("helm")),
	)
	if err != nil {
		r.updateHelmStatus(ctx, namespace, name, generation, err, log)
		return
	}

	// 1. Reconcile kdex-host-manager chart
	if err := r.reconcileHostManagerChart(c, host); err != nil {
		r.updateHelmStatus(ctx, namespace, name, generation, err, log)
		return
	}

	// 2. Reconcile companion charts
	if host.Spec.Helm != nil {
		for _, companion := range host.Spec.Helm.CompanionCharts {
			if err := r.reconcileCompanionChart(c, host, companion); err != nil {
				r.updateHelmStatus(ctx, namespace, name, generation, err, log)
				return
			}
		}
	}

	r.updateHelmStatus(ctx, namespace, name, generation, nil, log)
}

func (r *KDexHostReconciler) updateHelmStatus(ctx context.Context, namespace, name string, generation int64, err error, log logr.Logger) {
	defer r.clearHelmOperationActive(types.NamespacedName{Namespace: namespace, Name: name}, generation)

	host := &kdexv1alpha1.KDexHost{}
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, host); getErr != nil {
		log.Error(getErr, "failed to get host", "namespace", namespace, "name", name)
		return
	}

	patch := client.MergeFrom(host.DeepCopy())

	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}

	if err != nil {
		host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusFailed
		host.Status.Attributes[AttributeHelmReleaseError] = err.Error()
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Progressing: metav1.ConditionFalse,
				Degraded:    metav1.ConditionTrue,
			},
			kdexv1alpha1.ConditionReasonReconcileError,
			err.Error(),
		)
	} else {
		host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusCompleted
		delete(host.Status.Attributes, AttributeHelmReleaseError)
		kdexv1alpha1.SetConditions(
			&host.Status.Conditions,
			kdexv1alpha1.ConditionStatuses{
				Progressing: metav1.ConditionFalse,
			},
			kdexv1alpha1.ConditionReasonReconcileSuccess,
			"Helm installation completed",
		)
	}
	host.Status.Attributes[AttributeHelmReleaseGeneration] = strconv.FormatInt(generation, 10)
	host.Status.Attributes[AttributeHelmReleaseOwner] = r.ControllerID

	if patchErr := r.Status().Patch(ctx, host, patch); patchErr != nil {
		if !errors.IsNotFound(patchErr) && !errors.IsConflict(patchErr) {
			log.Error(patchErr, "failed to patch status")
		}
		log.V(2).Info("updateHelmStatus", "namespace", namespace, "name", name, "patchErr", patchErr)
	}
}

func (r *KDexHostReconciler) reconcileHostManagerChart(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost) error {
	// We need to pass the configuration to the chart via values.
	vals := map[string]any{}

	if host.Spec.Helm != nil && host.Spec.Helm.HostManager != nil {
		var err error
		vals, err = common.ReadValues([]byte(host.Spec.Helm.HostManager.Values))
		if err != nil {
			return err
		}
	}

	hostDefault := r.Configuration.HostDefault
	chartName := hostDefault.Chart.Name
	version := hostDefault.Chart.Version

	if host.Spec.Helm != nil && host.Spec.Helm.HostManager != nil && host.Spec.Helm.HostManager.Version != "" {
		version = host.Spec.Helm.HostManager.Version
	}

	// Set last so they override all other configurations

	vals["focalHost"] = host.Name
	vals["fullnameOverride"] = host.Name

	roleRef := map[string]string{}
	roleRef["apiGroup"] = "rbac.authorization.k8s.io"
	roleRef["kind"] = "ClusterRole"
	roleRef["name"] = host.Name + "-host-controller"
	vals["roleRef"] = roleRef

	spec := &utils.ChartSpec{
		ReleaseName: host.Name,
		ChartName:   chartName,
		Namespace:   host.Namespace,
		Values:      vals,
		Version:     version,
		Wait:        false, // Don't block the reconciler
		UpgradeCRDs: false,
	}

	return helmClient.InstallOrUpgrade(spec)
}

func (r *KDexHostReconciler) reconcileCompanionChart(helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, companion kdexv1alpha1.CompanionChart) error {
	if companion.Repository != "" {
		if err := helmClient.AddRepository(companion.Name, companion.Repository); err != nil {
			return err
		}
	}

	vals, err := common.ReadValues([]byte(companion.Values))
	if err != nil {
		return err
	}

	spec := &utils.ChartSpec{
		ReleaseName: companion.Name,
		ChartName:   companion.Chart,
		Namespace:   host.Namespace,
		Values:      vals,
		Version:     companion.Version,
		Wait:        false,
		UpgradeCRDs: false,
	}

	return helmClient.InstallOrUpgrade(spec)
}
