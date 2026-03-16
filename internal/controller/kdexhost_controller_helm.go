package controller

import (
	"context"
	"fmt"

	"github.com/kdex-tech/nexus-manager/internal/utils"
	helmclient "github.com/mittwald/go-helm-client"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *KDexHostReconciler) reconcileHelmReleases(ctx context.Context, host *kdexv1alpha1.KDexHost) (controllerutil.OperationResult, error) {
	c, err := r.HelmClientFactory(host.Namespace)
	if err != nil {
		return controllerutil.OperationResultNone, fmt.Errorf("failed to create helm client: %w", err)
	}

	// 1. Reconcile kdex-host-manager chart
	if err := r.reconcileHostManagerChart(ctx, c, host); err != nil {
		return controllerutil.OperationResultNone, err
	}

	// 2. Reconcile companion charts
	if host.Spec.Helm != nil {
		for _, companion := range host.Spec.Helm.CompanionCharts {
			if err := r.reconcileCompanionChart(ctx, c, host, companion); err != nil {
				return controllerutil.OperationResultNone, err
			}
		}
	}

	return controllerutil.OperationResultUpdated, nil
}

func (r *KDexHostReconciler) reconcileHostManagerChart(ctx context.Context, client utils.HelmClientInterface, host *kdexv1alpha1.KDexHost) error {
	// For now, assuming the chart is available locally or at a known path.
	// In production, this would be a remote repository.
	// For this task, the goal is to use the chart in kdex-host-manager/chart.

	// We need to pass the configuration to the chart via values.
	configString, err := r.getConfiguration()
	if err != nil {
		return err
	}

	valuesYaml := fmt.Sprintf("config:\n  config.yaml: |\n    %s\n", configString)
	if host.Spec.Helm != nil && host.Spec.Helm.HostManager != nil {
		valuesYaml += host.Spec.Helm.HostManager.Values
	}

	spec := &helmclient.ChartSpec{
		ReleaseName: host.Name,
		ChartName:   "/home/rotty/projects/kdex/workspace/kdex-host-manager/chart",
		Namespace:   host.Namespace,
		ValuesYaml:  valuesYaml,
		Wait:        false, // Don't block the reconciler
		UpgradeCRDs: true,
	}

	return client.InstallOrUpgrade(ctx, spec)
}

func (r *KDexHostReconciler) reconcileCompanionChart(ctx context.Context, client utils.HelmClientInterface, host *kdexv1alpha1.KDexHost, companion kdexv1alpha1.CompanionChart) error {
	if companion.Repository != "" {
		if err := client.AddRepository(companion.Name, companion.Repository); err != nil {
			return err
		}
	}

	spec := &helmclient.ChartSpec{
		ReleaseName: companion.Name,
		ChartName:   companion.Chart,
		Namespace:   host.Namespace,
		ValuesYaml:  companion.Values,
		Version:     companion.Version,
		Wait:        false,
		UpgradeCRDs: true,
	}

	return client.InstallOrUpgrade(ctx, spec)
}
