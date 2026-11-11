package controller

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCheckHostManagerChartVersion(t *testing.T) {
	for _, v := range []string{"0.33.0", "0.34.1", "1.0.0"} {
		if err := checkHostManagerChartVersion(v); err != nil {
			t.Errorf("%q: unexpected error %v", v, err)
		}
	}
	for _, v := range []string{"0.32.0", "0.32.9", "^0.33", "latest", "0.33"} {
		if err := checkHostManagerChartVersion(v); err == nil {
			t.Errorf("%q: want an error", v)
		}
	}
}

// The shipped default leaves the version empty (Helm resolves the latest), and
// the pre-run check must not refuse it.
func TestCheckHostManagerChart_DefaultConfigurationAccepted(t *testing.T) {
	if err := checkHostManagerChartVersion(""); err != nil {
		t.Errorf("empty version: %v", err)
	}
	r := hashReconciler(t)
	if err := r.checkHostManagerChart(baseHost()); err != nil {
		t.Errorf("default configuration refused: %v", err)
	}
}

func TestBuildHostManagerChartSpec_NoRoleRef(t *testing.T) {
	r := hashReconciler(t)
	spec, err := r.buildHostManagerChartSpec(baseHost())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spec.Values["roleRef"]; ok {
		t.Errorf("values carry a roleRef: %v", spec.Values["roleRef"])
	}
	src, err := os.ReadFile("kdexhost_controller_helm.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "HOST_MANAGER_CLUSTER_ROLE") {
		t.Error("kdexhost_controller_helm.go still reads HOST_MANAGER_CLUSTER_ROLE")
	}
}

// The ServiceAccount granted is the one the live Deployment runs as, read
// after the install -- not one nexus works out from the values.
func TestHelmRun_GrantsTheDeploymentsServiceAccount(t *testing.T) {
	host := helmHost("my-host")
	mock := &hookedHelmClient{MockHelmClient: NewMockHelmClient(nil)}
	r, fc := newHelmReconciler(t, mock, nil, host)
	mock.afterInstall = func(release string) {
		if release != "my-host" {
			return
		}
		d := &appsv1.Deployment{}
		if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "my-host"}, d); err != nil {
			t.Error(err)
			return
		}
		d.Spec.Template.Spec.ServiceAccountName = "from-the-deployment"
		if err := fc.Update(context.Background(), d); err != nil {
			t.Error(err)
		}
	}
	r.runAsyncHelmReconcile(t.Context(), "ns", "my-host", "hash", nil, discardLogger())

	if subs := readClusterReaderSubjects(t, fc); len(subs) != 1 || subs[0].Name != "from-the-deployment" || subs[0].Namespace != "ns" {
		t.Errorf("subjects = %v, want ns/from-the-deployment", subs)
	}
	if got := recordedHostManagerServiceAccount(getHost(t, fc, "my-host")); got != "from-the-deployment" {
		t.Errorf("recorded SA = %q, want from-the-deployment", got)
	}
}

// A Deployment that cannot be read right after the install fails the grant,
// and with it the run, which is retried: nothing is granted or recorded, and
// no companion is installed.
func TestHelmRun_GrantFailsWithoutTheDeployment(t *testing.T) {
	host := helmHost("my-host", "a")
	mock := NewMockHelmClient(nil)
	funcs := &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok {
				return apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	r, fc := newHelmReconciler(t, mock, funcs, host)
	r.runAsyncHelmReconcile(t.Context(), "ns", "my-host", "hash", nil, discardLogger())

	got := getHost(t, fc, "my-host")
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusFailed {
		t.Errorf("helm status = %q, want %q", s, HelmStatusFailed)
	}
	if subs := readClusterReaderSubjects(t, fc); len(subs) != 0 {
		t.Errorf("subjects = %v, want none", subs)
	}
	if sa := recordedHostManagerServiceAccount(got); sa != "" {
		t.Errorf("recorded SA = %q, want none", sa)
	}
	if slices.Contains(mock.Installed(), "a") {
		t.Error("a companion was installed after the grant failed")
	}
}

// A host name longer than the chart's 63-character fullname: the chart
// renders a shortened ServiceAccount name, and that -- read from the
// Deployment -- is what is granted.
func TestHelmRun_LongHostNameGrantsTheRenderedServiceAccount(t *testing.T) {
	name := strings.Repeat("h", 70)
	host := helmHost(name)
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	r.runAsyncHelmReconcile(t.Context(), "ns", name, "hash", nil, discardLogger())

	if subs := readClusterReaderSubjects(t, fc); len(subs) != 1 || subs[0].Name != name[:63] {
		t.Errorf("subjects = %v, want ns/%s", subs, name[:63])
	}
}

// A per-host serviceAccount override must reach the cluster-reader subject
// through the merged values.
func TestHelmRun_SubjectNamesTheRenderedServiceAccount(t *testing.T) {
	host := helmHost("my-host")
	host.Spec.Helm = &kdexv1alpha1.HelmConfig{HostManager: &kdexv1alpha1.HostManagerHelmConfig{
		Values: `{"serviceAccount":{"create":false,"name":"custom"}}`,
	}}
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	r.runAsyncHelmReconcile(t.Context(), "ns", "my-host", "hash", nil, discardLogger())

	got := readClusterReaderSubjects(t, fc)
	if len(got) != 1 || got[0].Name != "custom" || got[0].Namespace != "ns" {
		t.Errorf("subjects = %v, want ns/custom", got)
	}
}

func TestHelmRun_UnsupportedChartVersionRefused(t *testing.T) {
	mock := NewMockHelmClient(nil)
	host := helmHost("my-host", "x1")
	host.Spec.Helm = &kdexv1alpha1.HelmConfig{
		HostManager:     &kdexv1alpha1.HostManagerHelmConfig{Version: "0.32.0"},
		CompanionCharts: []kdexv1alpha1.CompanionChart{{Name: "x1", Chart: "oci://r/x1"}},
	}
	r, fc := newHelmReconciler(t, mock, nil, host)
	r.runAsyncHelmReconcile(t.Context(), "ns", "my-host", "hash", nil, discardLogger())
	got := getHost(t, fc, "my-host")

	if inst := mock.Installed(); len(inst) != 0 {
		t.Errorf("installed %v, want nothing", inst)
	}
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusFailed {
		t.Errorf("helm status = %q, want %q", s, HelmStatusFailed)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, string(kdexv1alpha1.ConditionTypeDegraded))
	if cond == nil || cond.Reason != string(ConditionReasonHostManagerChartUnsupported) {
		t.Errorf("degraded condition = %+v, want reason %q", cond, ConditionReasonHostManagerChartUnsupported)
	}
	if subs := readClusterReaderSubjects(t, fc); len(subs) != 0 {
		t.Errorf("subjects = %v, want none", subs)
	}
}

func hostWithRecordedSA(sa, status string) *kdexv1alpha1.KDexHost {
	host := helmHost("my-host")
	host.Finalizers = []string{hostFinalizerName}
	host.Status.Attributes = map[string]string{}
	if sa != "" {
		host.Status.Attributes[AttributeHostManagerServiceAccount] = sa
	}
	if status != "" {
		host.Status.Attributes[AttributeHelmReleaseStatus] = status
	}
	return host
}

// The heal keys on the recorded grant, not on the run's status: an installed
// host-manager keeps running while its run is Failed or InProgress.
func TestHealClusterReaderSubject(t *testing.T) {
	for _, status := range []string{HelmStatusCompleted, HelmStatusFailed, HelmStatusInProgress} {
		t.Run(status, func(t *testing.T) {
			r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
			host := hostWithRecordedSA("my-host", status)
			host.Namespace = "ns"
			for range 2 {
				if err := r.healClusterReaderSubject(t.Context(), host); err != nil {
					t.Fatal(err)
				}
			}
			if subs := readClusterReaderSubjects(t, fc); len(subs) != 1 || subs[0].Name != "my-host" {
				t.Errorf("subjects = %v, want ns/my-host once", subs)
			}
		})
	}
	t.Run("no record", func(t *testing.T) {
		r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
		host := hostWithRecordedSA("", HelmStatusCompleted)
		host.Namespace = "ns"
		if err := r.healClusterReaderSubject(t.Context(), host); err != nil {
			t.Fatal(err)
		}
		if subs := readClusterReaderSubjects(t, fc); len(subs) != 0 {
			t.Errorf("host with no recorded grant got subjects %v", subs)
		}
	})
}

// Renaming the ServiceAccount swaps the subject only once the install that
// renders the new one has succeeded.
func TestHelmRun_ServiceAccountRenameSwapsTheSubjectAfterInstall(t *testing.T) {
	host := helmHost("my-host")
	host.Status.Attributes = map[string]string{AttributeHostManagerServiceAccount: "old"}
	host.Spec.Helm = &kdexv1alpha1.HelmConfig{HostManager: &kdexv1alpha1.HostManagerHelmConfig{
		Values: `{"serviceAccount":{"create":false,"name":"new"}}`,
	}}
	mock := NewMockHelmClient(nil)
	mock.failInstall, mock.failMessage = true, "install failed"
	r, fc := newHelmReconciler(t, mock, nil, host)
	if err := r.ensureClusterReaderSubject(t.Context(), "ns", "old"); err != nil {
		t.Fatal(err)
	}

	r.runAsyncHelmReconcile(t.Context(), "ns", "my-host", "hash", nil, discardLogger())
	if subs := readClusterReaderSubjects(t, fc); len(subs) != 1 || subs[0].Name != "old" {
		t.Fatalf("after a failed install subjects = %v, want only old", subs)
	}

	mock.failInstall = false
	r.runAsyncHelmReconcile(t.Context(), "ns", "my-host", "hash2", nil, discardLogger())
	subs := readClusterReaderSubjects(t, fc)
	if len(subs) != 1 || subs[0].Name != "new" {
		t.Fatalf("after the install subjects = %v, want only new", subs)
	}
	if got := recordedHostManagerServiceAccount(getHost(t, fc, "my-host")); got != "new" {
		t.Errorf("recorded SA = %q, want new", got)
	}
}

// Reconcile heals the subject even when the helm hash matches and no run
// starts.
func TestReconcile_HealsTheSubjectWithoutAHelmRun(t *testing.T) {
	host := hostWithRecordedSA("my-host", HelmStatusCompleted)
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
	hash, err := r.computeHelmReleaseHash(host)
	if err != nil {
		t.Fatal(err)
	}
	host.Status.Attributes[AttributeHelmReleaseHash] = hash
	if err := fc.Status().Update(t.Context(), host); err != nil {
		t.Fatal(err)
	}

	_, _ = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "my-host"}})

	if subs := readClusterReaderSubjects(t, fc); len(subs) != 1 || subs[0].Name != "my-host" {
		t.Errorf("subjects = %v, want ns/my-host restored by Reconcile", subs)
	}
}

// A refused run's typed reason is what the host shows: the reconcile's status
// patch carries the helm run's failure forward without rewriting its reason.
func TestReconcile_KeepsTheHelmRunsRefusalReason(t *testing.T) {
	for _, tt := range []struct {
		name   string
		helm   *kdexv1alpha1.HelmConfig
		reason kdexv1alpha1.ConditionReason
	}{
		{"unsupported host-manager chart",
			&kdexv1alpha1.HelmConfig{HostManager: &kdexv1alpha1.HostManagerHelmConfig{Version: "0.32.0"}},
			ConditionReasonHostManagerChartUnsupported},
		{"refused companion",
			&kdexv1alpha1.HelmConfig{CompanionCharts: []kdexv1alpha1.CompanionChart{{Name: "bad", Chart: "file:///x"}}},
			ConditionReasonCompanionChartRefused},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := helmHost("my-host")
			host.Finalizers = []string{hostFinalizerName}
			host.Spec.Helm = tt.helm
			r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil, host)
			// Long enough that the second pass finds the failed run in backoff.
			r.RequeueDelay = time.Hour
			key := types.NamespacedName{Namespace: "ns", Name: "my-host"}
			tr := &kdexv1alpha1.KDexClusterTranslation{ObjectMeta: metav1.ObjectMeta{Name: "kdex-default-translation"}}
			kdexv1alpha1.SetConditions(&tr.Status.Conditions, kdexv1alpha1.ConditionStatuses{Ready: metav1.ConditionTrue},
				kdexv1alpha1.ConditionReasonReconcileSuccess, "ready")
			if err := fc.Create(t.Context(), tr); err != nil {
				t.Fatal(err)
			}

			// The first pass starts the run, which is refused.
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			awaitNoHelmRun(t, r, key)
			if s := getHost(t, fc, "my-host").Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusFailed {
				t.Fatalf("helm status = %q, want %q", s, HelmStatusFailed)
			}
			// The second finds it failed and in backoff, and patches the
			// status around it.
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if r.helmRunInFlight(key) {
				t.Fatal("the second pass started a run; want it in backoff")
			}

			got := getHost(t, fc, "my-host")
			for _, c := range []kdexv1alpha1.ConditionType{kdexv1alpha1.ConditionTypeDegraded, kdexv1alpha1.ConditionTypeReady} {
				cond := meta.FindStatusCondition(got.Status.Conditions, string(c))
				if cond == nil || cond.Reason != string(tt.reason) {
					t.Errorf("%s condition = %+v, want reason %q", c, cond, tt.reason)
				}
			}
		})
	}
}
