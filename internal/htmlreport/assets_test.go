package htmlreport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStyleCSS_LoadsAndCarriesExpectedClasses asserts the embed is
// wired correctly and the stylesheet covers the classes the
// renderers actually emit. If a future refactor renames a class in
// the renderers, this test fails as a reminder to update the CSS
// (or vice versa).
func TestStyleCSS_LoadsAndCarriesExpectedClasses(t *testing.T) {
	b := StyleCSS()
	require.NotEmpty(t, b)

	css := string(b)

	for _, sev := range []string{
		".severity-critical",
		".severity-high",
		".severity-medium",
		".severity-low",
		".severity-informational",
		".severity-positive",
	} {
		assert.Contains(t, css, sev,
			"stylesheet should define %s for the renderer's severity badge", sev)
	}

	for _, posture := range []string{
		".posture-vetted-frozen",
		".posture-trusted-for-now",
		".posture-unexamined",
		".posture-unknown-provenance",
		".posture-rejected",
	} {
		assert.Contains(t, css, posture,
			"stylesheet should define %s for the posture call-out", posture)
	}

	assert.Contains(t, css, ".missing-reference-banner",
		"stylesheet should define the missing-reference banner class")
}

// TestAssetsFS_HasStyleCSSAtRoot guards the fs.Sub layout the
// directory writer relies on: "style.css" must sit at the root of
// the returned filesystem, not at "embedded_assets/style.css".
func TestAssetsFS_HasStyleCSSAtRoot(t *testing.T) {
	fsys := AssetsFS()
	f, err := fsys.Open("style.css")
	require.NoError(t, err)
	defer f.Close() //nolint:errcheck // test-only; close error not actionable

	// And the embedded_assets/ prefix should NOT be visible at the
	// subbed root.
	if _, err := fsys.Open("embedded_assets/style.css"); err == nil {
		t.Fatal("embedded_assets/ prefix should be stripped by fs.Sub")
	}
}
