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
