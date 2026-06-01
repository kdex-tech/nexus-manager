package webhook

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"kdex.dev/crds/render"
	"sigs.k8s.io/yaml"
)

// bundledFooterPath is the repo-relative path to the bundled cluster-default
// page footer, read from the test's working directory (internal/webhook).
const bundledFooterPath = "../../config/bundled/kdex-default-page-footer.yaml"

func readBundledFooterContent(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(bundledFooterPath)
	require.NoError(t, err)

	var footer kdexv1alpha1.KDexClusterPageFooter
	require.NoError(t, yaml.Unmarshal(raw, &footer))
	require.NotEmpty(t, footer.Spec.Content)
	return footer.Spec.Content
}

// TestBundledFooterUsesDateHelper guards the migration described in issue #3:
// the bundled footer should render its copyright year via the `date` template
// helper rather than the bare `.LastModified.Year` field accessor.
func TestBundledFooterUsesDateHelper(t *testing.T) {
	content := readBundledFooterContent(t)

	assert.Contains(t, content, `[[ date .LastModified "year" ]]`,
		"bundled footer should use the date helper for the copyright year")
	assert.NotContains(t, content, `.LastModified.Year`,
		"bundled footer should no longer use the bare .LastModified.Year accessor")
}

// TestBundledFooterRendersIdenticalYear asserts the `date` helper form is
// byte-identical to the old `.LastModified.Year` form, so the migration is
// purely cosmetic and produces no observable change in rendered output.
func TestBundledFooterRendersIdenticalYear(t *testing.T) {
	content := readBundledFooterContent(t)

	renderer := render.Renderer{}
	data := render.DefaultTemplateData()

	got, err := renderer.RenderOne("kdex-default-page-footer", content, data)
	require.NoError(t, err)

	// Control: the legacy field-accessor form the bundled footer is migrating from.
	legacy := strings.ReplaceAll(content, `[[ date .LastModified "year" ]]`, `[[ .LastModified.Year ]]`)
	want, err := renderer.RenderOne("kdex-default-page-footer-legacy", legacy, data)
	require.NoError(t, err)

	assert.Equal(t, want, got, "date helper output must be byte-identical to .LastModified.Year")
}
