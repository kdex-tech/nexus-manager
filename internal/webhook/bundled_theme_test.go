package webhook

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kdexv1alpha1 "kdex.dev/crds/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

const bundledThemePath = "../../config/bundled/kdex-default-theme.yaml"

// jsDelivrVersionPin matches an exact x.y.z version in a jsDelivr path
// (".../cdn-release@11.12.0/..."); "@latest", "@11" and no "@" all fail.
var jsDelivrVersionPin = regexp.MustCompile(`@\d+\.\d+\.\d+/`)

// The default theme is on every branded consent page, whose CSP admits any
// https: stylesheet, so a third-party stylesheet it loads must be pinned to a
// version and carry SRI: a CDN that serves other bytes is then refused by the
// browser. (Google Fonts CSS varies by user agent and cannot take SRI.)
func TestBundledThemeCDNStylesheetsArePinnedWithSRI(t *testing.T) {
	raw, err := os.ReadFile(bundledThemePath)
	require.NoError(t, err)

	var theme kdexv1alpha1.KDexClusterTheme
	require.NoError(t, yaml.Unmarshal(raw, &theme))

	seen := 0
	for _, a := range theme.Spec.Assets {
		if a.LinkHref == "" {
			continue
		}
		u, err := url.Parse(a.LinkHref)
		require.NoError(t, err, a.LinkHref)
		if u.Host != "cdn.jsdelivr.net" {
			continue
		}
		seen++
		assert.NotContains(t, a.LinkHref, "@latest", a.LinkHref)
		assert.Regexp(t, jsDelivrVersionPin, a.LinkHref, "pinned to an exact version")
		assert.True(t, strings.HasPrefix(a.Attributes["integrity"], "sha384-"), "integrity on %s", a.LinkHref)
		assert.Equal(t, "anonymous", a.Attributes["crossorigin"], "crossorigin on %s", a.LinkHref)
	}
	assert.NotZero(t, seen, "the default theme loads at least one cdn.jsdelivr.net stylesheet")
}
