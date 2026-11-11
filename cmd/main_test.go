package main

import (
	"testing"

	"helm.sh/helm/v4/pkg/chart/loader/archive"
)

func TestChartSizeFlagsApplied(t *testing.T) {
	oldC, oldF := archive.MaxDecompressedChartSize, archive.MaxDecompressedFileSize
	t.Cleanup(func() { archive.MaxDecompressedChartSize, archive.MaxDecompressedFileSize = oldC, oldF })

	applyChartSizeLimits(16<<20, 4<<20)

	if archive.MaxDecompressedChartSize != 16<<20 {
		t.Errorf("MaxDecompressedChartSize = %d", archive.MaxDecompressedChartSize)
	}
	if archive.MaxDecompressedFileSize != 4<<20 {
		t.Errorf("MaxDecompressedFileSize = %d", archive.MaxDecompressedFileSize)
	}
}

// Helm decodes every retained revision (10) at once on an upgrade or
// uninstall, so the per-chart caps are budgeted against that, not one render.
func TestChartSizeDefaults(t *testing.T) {
	if defaultChartMaxDecompressedBytes != 4<<20 {
		t.Errorf("default --chart-max-decompressed-bytes = %d, want 4MiB", defaultChartMaxDecompressedBytes)
	}
	if defaultChartMaxFileBytes != 1<<20 {
		t.Errorf("default --chart-max-file-bytes = %d, want 1MiB", defaultChartMaxFileBytes)
	}
}
