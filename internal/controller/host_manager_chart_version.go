package controller

import (
	"github.com/Masterminds/semver/v3"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
)

// MinHostManagerChartVersion is the oldest host-manager chart this operator
// installs. Older charts still ship a ClusterRole/roleRef of their own, which
// the installer identity can no longer create.
const MinHostManagerChartVersion = "0.33.0"

// ConditionReasonHostManagerChartUnsupported is the reason on a host whose
// host-manager chart version is below MinHostManagerChartVersion.
const ConditionReasonHostManagerChartUnsupported kdexv1alpha1.ConditionReason = "HostManagerChartUnsupported"

// checkHostManagerChartVersion refuses any explicit pin that is not strict
// semver at or above MinHostManagerChartVersion (ranges and tags such as
// "^0.33" or "latest" included: the floor cannot be verified for them). An
// empty version is accepted: the shipped default leaves it empty and Helm
// resolves the registry's latest chart.
func checkHostManagerChartVersion(version string) error {
	if version == "" {
		return nil
	}
	v, err := semver.StrictNewVersion(version)
	if err != nil {
		return refused(ConditionReasonHostManagerChartUnsupported, "host-manager chart version %q is not an exact semantic version; at least %s is required", version, MinHostManagerChartVersion)
	}
	if v.LessThan(semver.MustParse(MinHostManagerChartVersion)) {
		return refused(ConditionReasonHostManagerChartUnsupported, "host-manager chart version %q is older than the minimum supported %s", version, MinHostManagerChartVersion)
	}
	return nil
}
