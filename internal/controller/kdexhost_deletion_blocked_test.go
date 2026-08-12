package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// stuckUninstallHelmClient models a release whose teardown cannot complete --
// e.g. a PVC held by a terminating pod, which makes Helm's foreground deletion
// time out.
type stuckUninstallHelmClient struct {
	utils.HelmClientInterface
}

func (c *stuckUninstallHelmClient) Uninstall(releaseName string) error {
	return fmt.Errorf("failed to uninstall release %s: timed out waiting for deletion", releaseName)
}

func (c *stuckUninstallHelmClient) ListReleases(map[string]string) ([]string, error) {
	return nil, nil
}

// Keeping the finalizer when an uninstall fails (#51) is correct -- releasing it
// would delete the CR while its releases still run. But it converts a silent
// orphan into a host that sits in Terminating indefinitely, and the operator
// needs to be able to see WHY without reading controller logs. The reason has
// to reach the CR's own status, because that is all `kubectl describe` shows.
func TestBlockedDeletionSurfacesTheReasonInStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kdex scheme: %v", err)
	}
	if err := configuration.AddToScheme(scheme); err != nil {
		t.Fatalf("add configuration scheme: %v", err)
	}

	now := metav1.Now()
	host := &kdexv1alpha1.KDexHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "doomed",
			Namespace:         "default",
			Finalizers:        []string{hostFinalizerName},
			DeletionTimestamp: &now,
		},
		Spec: kdexv1alpha1.KDexHostSpec{
			BrandName:    "KDex Tech",
			Organization: "KDex Tech Inc.",
			ModulePolicy: kdexv1alpha1.LooseModulePolicy,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(host).
		WithStatusSubresource(&kdexv1alpha1.KDexHost{}).
		WithIndex(&kdexv1alpha1.KDexInternalUtilityPage{}, hostIndexKey, func(client.Object) []string { return nil }).
		WithIndex(&kdexv1alpha1.KDexInternalTranslation{}, hostIndexKey, func(client.Object) []string { return nil }).
		Build()

	r := &KDexHostReconciler{
		Client:        fc,
		ControllerID:  "test",
		Ctx:           context.Background(),
		Configuration: configuration.LoadConfiguration("/nonexistent-config.yaml", scheme),
		Scheme:        scheme,
		HelmClientFactory: func(string, kdexv1alpha1.Secrets, logr.Logger) (utils.HelmClientInterface, error) {
			return &stuckUninstallHelmClient{}, nil
		},
	}

	key := types.NamespacedName{Name: "doomed", Namespace: "default"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})

	if err == nil {
		t.Fatal("expected the failed uninstall to be returned, got nil")
	}

	latest := &kdexv1alpha1.KDexHost{}
	if getErr := fc.Get(context.Background(), key, latest); getErr != nil {
		t.Fatalf("get host: %v", getErr)
	}

	// The finalizer must survive: that is the fix from #51.
	if !controllerutil.ContainsFinalizer(latest, hostFinalizerName) {
		t.Error("finalizer was removed despite the uninstall failing")
	}

	cond := meta.FindStatusCondition(latest.Status.Conditions, string(kdexv1alpha1.ConditionTypeDegraded))
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Degraded condition = %v, want True: a host stuck in Terminating must say why", cond)
	}
	if !strings.Contains(cond.Message, "doomed") {
		t.Errorf("Degraded message %q does not name the release that could not be uninstalled", cond.Message)
	}
}
