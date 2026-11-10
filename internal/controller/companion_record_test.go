package controller

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/kdex-tech/nexus-manager/internal/utils"
	"k8s.io/apimachinery/pkg/runtime"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/configuration"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// helmReconcile runs one asynchronous helm reconcile of host over a fake
// cluster holding hosts, and returns the host as it then stands.
func helmReconcile(t *testing.T, mock *MockHelmClient, host string, hosts ...*kdexv1alpha1.KDexHost) *kdexv1alpha1.KDexHost {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kdexv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := configuration.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs := make([]client.Object, 0, len(hosts))
	for _, h := range hosts {
		objs = append(objs, h)
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithStatusSubresource(&kdexv1alpha1.KDexHost{}).Build()
	r := &KDexHostReconciler{
		Client:                fc,
		ControllerID:          "test",
		Ctx:                   context.Background(),
		Configuration:         configuration.LoadConfiguration("/nonexistent-config.yaml", scheme),
		Scheme:                scheme,
		HelmRenderConcurrency: 1,
		HelmClientFactory: func(string, kdexv1alpha1.Secrets, logr.Logger) (utils.HelmClientInterface, error) {
			return mock, nil
		},
	}
	r.runAsyncHelmReconcile(context.Background(), "ns", host, "hash", nil, logr.Discard())

	got := &kdexv1alpha1.KDexHost{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: host}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func helmHost(name string, companions ...string) *kdexv1alpha1.KDexHost {
	h := hostWithCompanions(name, companions...)
	h.Spec.BrandName = "KDex Tech"
	h.Spec.Organization = "KDex Tech Inc."
	h.Spec.ModulePolicy = kdexv1alpha1.LooseModulePolicy
	return h
}

// The record is the authority a later prune or teardown acts on, so a
// reconcile records every companion it installed, and drops what it pruned.
func TestHelmReconcile_RecordsInstalledCompanionsAndForgetsPrunedOnes(t *testing.T) {
	mock := NewMockHelmClient(map[string]map[string]string{
		"dropped": {LabelCompanionOf: "my-host"},
	})
	host := recorded(helmHost("my-host", "b", "a"), "dropped")

	got := helmReconcile(t, mock, "my-host", host)

	if rec := got.Status.Attributes[AttributeCompanionReleases]; rec != "a,b" {
		t.Errorf("recorded companions = %q, want %q", rec, "a,b")
	}
	if !slices.Equal(mock.Uninstalled(), []string{"dropped"}) {
		t.Errorf("uninstalled = %v, want [dropped]", mock.Uninstalled())
	}
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusCompleted {
		t.Errorf("helm status = %q, want %q (%s)", s, HelmStatusCompleted, got.Status.Attributes[AttributeHelmReleaseError])
	}
}

// A companion named after another KDexHost would be installed over that host's
// own release. The reconcile refuses it, installs no companion, and says why.
func TestHelmReconcile_RefusesACompanionNamedAfterAKDexHost(t *testing.T) {
	mock := NewMockHelmClient(nil)
	other := helmHost("other-host")
	other.Namespace = "ns"

	got := helmReconcile(t, mock, "my-host", helmHost("my-host", "ok", "other-host"), other)

	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusFailed {
		t.Errorf("helm status = %q, want %q", s, HelmStatusFailed)
	}
	if e := got.Status.Attributes[AttributeHelmReleaseError]; !strings.Contains(e, "other-host") {
		t.Errorf("helm error = %q, want it to name other-host", e)
	}
	for _, name := range mock.Installed() {
		if name == "other-host" || name == "ok" {
			t.Errorf("installed %q despite the refused companion", name)
		}
	}
}

// Every reconcile's own status patch overwrites the server's status with its
// snapshot, except for the helm.release. keys the async helm operation writes.
// The companion record is one of those: outside that prefix, a reconcile that
// overlapped the operation erased it, and the next prune found nothing to do.
func TestCompanionRecordIsPreservedByTheReconcileStatusPatch(t *testing.T) {
	if !strings.HasPrefix(AttributeCompanionReleases, "helm.release.") {
		t.Fatalf("%q is not a helm.release. key, so Reconcile's deferred status patch overwrites it", AttributeCompanionReleases)
	}
}
