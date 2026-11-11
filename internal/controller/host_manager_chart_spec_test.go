package controller

import (
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

func TestCheckHostManagerChartVersion(t *testing.T) {
	for _, v := range []string{"0.33.0", "0.34.1", "1.0.0"} {
		if err := checkHostManagerChartVersion(v); err != nil {
			t.Errorf("%q: unexpected error %v", v, err)
		}
	}
	for _, v := range []string{"0.32.0", "0.32.9", "", "^0.33", "latest", "0.33"} {
		if err := checkHostManagerChartVersion(v); err == nil {
			t.Errorf("%q: want an error", v)
		}
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

func TestHostManagerServiceAccountName(t *testing.T) {
	tests := []struct {
		name string
		vals map[string]any
		want string
	}{
		{"absent", map[string]any{}, "my-host"},
		{"name", map[string]any{"serviceAccount": map[string]any{"name": "x"}}, "x"},
		{"create false", map[string]any{"serviceAccount": map[string]any{"create": false}}, "default"},
		{"create false with name", map[string]any{"serviceAccount": map[string]any{"create": false, "name": "y"}}, "y"},
		{"create true", map[string]any{"serviceAccount": map[string]any{"create": true}}, "my-host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostManagerServiceAccountName(tt.vals, "my-host"); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
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

// A host whose chart is installed gets its subject on every reconcile, even
// when the release-hash gate would skip the run; a host that never installed
// gets none.
func TestHealClusterReaderSubject(t *testing.T) {
	r, fc := newHelmReconciler(t, NewMockHelmClient(nil), nil)
	host := helmHost("my-host")
	host.Namespace = "ns"
	ctx := t.Context()

	if err := r.healClusterReaderSubject(ctx, host); err != nil {
		t.Fatal(err)
	}
	if subs := readClusterReaderSubjects(t, fc); len(subs) != 0 {
		t.Fatalf("never-installed host got subjects %v", subs)
	}

	host.Status.Attributes = map[string]string{AttributeHelmReleaseStatus: HelmStatusCompleted}
	for range 2 {
		if err := r.healClusterReaderSubject(ctx, host); err != nil {
			t.Fatal(err)
		}
	}
	if subs := readClusterReaderSubjects(t, fc); len(subs) != 1 || subs[0].Name != "my-host" {
		t.Errorf("subjects = %v, want ns/my-host once", subs)
	}
}
