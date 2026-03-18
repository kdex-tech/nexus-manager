package controller

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	AttributeHelmReleaseStatus     = "helm.release.status"
	AttributeHelmReleaseGeneration = "helm.release.generation"
	AttributeHelmReleaseError      = "helm.release.error"

	HelmStatusInProgress = "in_progress"
	HelmStatusCompleted  = "completed"
	HelmStatusFailed     = "failed"
)

func (r *KDexHostReconciler) reconcileHelmReleases(ctx context.Context, host *kdexv1alpha1.KDexHost) (controllerutil.OperationResult, error) {
	// Check if a Helm operation is already in progress
	status := host.Status.Attributes[AttributeHelmReleaseStatus]
	genStr := host.Status.Attributes[AttributeHelmReleaseGeneration]
	gen, _ := strconv.ParseInt(genStr, 10, 64)

	if status == HelmStatusInProgress {
		// Still working, requeue
		return controllerutil.OperationResultNone, nil
	}

	if (status == HelmStatusCompleted || status == HelmStatusFailed) && gen == host.Generation {
		// Already attempted for this generation
		return controllerutil.OperationResultNone, nil
	}

	// Start a new Helm operation asynchronously
	if host.Status.Attributes == nil {
		host.Status.Attributes = make(map[string]string)
	}
	host.Status.Attributes[AttributeHelmReleaseStatus] = HelmStatusInProgress
	host.Status.Attributes[AttributeHelmReleaseGeneration] = strconv.FormatInt(host.Generation, 10)
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
	latestHost := &kdexv1alpha1.KDexHost{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(host), latestHost); err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("failed to get latest host: %w", err)
	}

	patch := client.MergeFrom(latestHost.DeepCopy())
	latestHost.Status = host.Status
	if err := r.Status().Patch(ctx, latestHost, patch); err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("failed to patch status for helm progress: %w", err)
	}
	// Update the local object with the new resource version
	host.ResourceVersion = latestHost.ResourceVersion

	// Start the goroutine
	go r.runAsyncHelmReconcile(host.Namespace, host.Name, host.Generation)

	return controllerutil.OperationResultUpdated, nil
}

func (r *KDexHostReconciler) runAsyncHelmReconcile(namespace, name string, generation int64) {
	ctx := r.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Fetch a fresh copy of the host
	host := &kdexv1alpha1.KDexHost{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, host); err != nil {
		return // Host might have been deleted
	}

	c, err := r.HelmClientFactory(host.Namespace)
	if err != nil {
		r.updateHelmStatus(namespace, name, generation, err)
		return
	}

	// 1. Reconcile kdex-host-manager chart
	if err := r.reconcileHostManagerChart(ctx, c, host); err != nil {
		r.updateHelmStatus(namespace, name, generation, err)
		return
	}

	// 2. Reconcile companion charts
	if host.Spec.Helm != nil {
		for _, companion := range host.Spec.Helm.CompanionCharts {
			if err := r.reconcileCompanionChart(ctx, c, host, companion); err != nil {
				r.updateHelmStatus(namespace, name, generation, err)
				return
			}
		}
	}

	r.updateHelmStatus(namespace, name, generation, nil)
}

func (r *KDexHostReconciler) updateHelmStatus(namespace, name string, generation int64, err error) {
	ctx := r.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	host := &kdexv1alpha1.KDexHost{}
	if getErr := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, host); getErr != nil {
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

	if patchErr := r.Status().Patch(ctx, host, patch); patchErr != nil {
		if !errors.IsNotFound(patchErr) && !errors.IsConflict(patchErr) {
			logf.FromContext(ctx).Error(patchErr, "failed to patch status")
		}
	}
}

func (r *KDexHostReconciler) reconcileHostManagerChart(ctx context.Context, helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost) error {
	// We need to pass the configuration to the chart via values.
	configString, err := r.getConfiguration()
	if err != nil {
		return err
	}

	vals, err := common.ReadValues([]byte(configString))
	if err != nil {
		return err
	}
	if host.Spec.Helm != nil && host.Spec.Helm.HostManager != nil {
		overrideVals, err := common.ReadValues([]byte(host.Spec.Helm.HostManager.Values))
		if err != nil {
			return err
		}
		vals = util.CoalesceTables(overrideVals, vals)
	}

	chartName := "oci://ghcr.io/kdex-tech/charts/host-manager"
	version := "0.2.18" // Default version as requested

	if host.Spec.Helm != nil && host.Spec.Helm.HostManager != nil && host.Spec.Helm.HostManager.Version != "" {
		version = host.Spec.Helm.HostManager.Version
	}

	spec := &utils.ChartSpec{
		ReleaseName: host.Name,
		ChartName:   chartName,
		Namespace:   host.Namespace,
		Values:      vals,
		Version:     version,
		Wait:        false, // Don't block the reconciler
		UpgradeCRDs: false,
	}

	return helmClient.InstallOrUpgrade(ctx, spec)
}

func (r *KDexHostReconciler) reconcileCompanionChart(ctx context.Context, helmClient utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, companion kdexv1alpha1.CompanionChart) error {
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

	return helmClient.InstallOrUpgrade(ctx, spec)
}
