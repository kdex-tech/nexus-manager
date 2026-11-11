package controller

import (
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

func hostWithSources(name string, companions ...kdexv1alpha1.CompanionChart) *kdexv1alpha1.KDexHost {
	h := helmHost(name)
	h.Spec.Helm = &kdexv1alpha1.HelmConfig{CompanionCharts: companions}
	return h
}

func TestCheckCompanionSources(t *testing.T) {
	tests := []struct {
		name      string
		companion kdexv1alpha1.CompanionChart
		wantErr   string
	}{
		{"oci", kdexv1alpha1.CompanionChart{Name: "y", Chart: "oci://registry.gitlab.com/x/charts/y"}, ""},
		{"local path", kdexv1alpha1.CompanionChart{Name: "y", Chart: "/var/cache/helm/x.tgz"}, "oci://"},
		{"file url", kdexv1alpha1.CompanionChart{Name: "y", Chart: "file:///x"}, "oci://"},
		{"https url", kdexv1alpha1.CompanionChart{Name: "y", Chart: "https://h/x.tgz"}, "oci://"},
		{"repo/chart", kdexv1alpha1.CompanionChart{Name: "y", Chart: "repo/chart"}, "oci://"},
		{"repository set", kdexv1alpha1.CompanionChart{Name: "y", Chart: "oci://r/y", Repository: "https://h"}, "repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkCompanionSources(hostWithSources("h", tt.companion))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantErr)
			}
			var refusal *refusalError
			if !errors.As(err, &refusal) || refusal.reason != ConditionReasonCompanionChartRefused {
				t.Errorf("error %#v is not a refusal under %s", err, ConditionReasonCompanionChartRefused)
			}
		})
	}
	if err := checkCompanionSources(helmHost("h")); err != nil {
		t.Errorf("host without helm block: %v", err)
	}
}

// A refused companion installs nothing at all -- the host-manager chart
// included -- and the host says why, under its own reason.
func TestHelmRun_RefusedCompanionInstallsNothing(t *testing.T) {
	mock := NewMockHelmClient(nil)
	host := hostWithSources("my-host", kdexv1alpha1.CompanionChart{Name: "bad", Chart: "file:///x"})

	got := helmReconcile(t, mock, "my-host", host)

	if inst := mock.Installed(); len(inst) != 0 {
		t.Errorf("installed %v, want nothing", inst)
	}
	if s := got.Status.Attributes[AttributeHelmReleaseStatus]; s != HelmStatusFailed {
		t.Errorf("helm status = %q, want %q", s, HelmStatusFailed)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, string(kdexv1alpha1.ConditionTypeDegraded))
	if cond == nil || cond.Reason != string(ConditionReasonCompanionChartRefused) {
		t.Errorf("degraded condition = %+v, want reason %q", cond, ConditionReasonCompanionChartRefused)
	}
}
