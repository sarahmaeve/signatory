//go:build network_access_ok

// These tests hit the real PyPI registry (pypi.org). They are NOT run
// by default — they share the network_access_ok build tag with the
// cmd/signatory live tests.
//
// To run the curated dogfood set:
//
//	go test -tags network_access_ok -run Live_PyPINativeExtension \
//	    ./internal/signal/registry/pypi/ -v
//
// To dogfood an arbitrary module (or a comma-separated list), set the
// target env var — this is the reusable "feed a module name" fixture:
//
//	SIGNATORY_PYPI_TARGET=tiktoken,ensmallen,mistralai \
//	    go test -tags network_access_ok -run Live_PyPINativeExtension_Target \
//	    ./internal/signal/registry/pypi/ -v
//
// These validate the native_extension_present / native_extension_introduced
// signals end-to-end against live wheel metadata. Run manually before
// releases, not in CI.

package pypi

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// collectNativeExtensionLive runs the real collector against the live
// PyPI registry for one package and returns the decoded
// native_extension_present and native_extension_introduced payloads.
// Fails the test if either signal did not land (an absence means the
// package had no parseable releases — informative, not silent).
func collectNativeExtensionLive(t *testing.T, name string) (present, introduced map[string]any) {
	t.Helper()
	raw, err := NewCollector().Collect(t.Context(), pypiEntity(name))
	require.NoError(t, err, "live collect for %q", name)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "native_extension_present"),
		"native_extension_present should land for %q (absence ⇒ no parseable releases)", name)
	require.True(t, hasSignal(result, "native_extension_introduced"),
		"native_extension_introduced should land for %q", name)

	return getSignalValue(t, result, "native_extension_present"),
		getSignalValue(t, result, "native_extension_introduced")
}

// logNativeExtension prints the dogfood-relevant fields for one package
// so a manual run reads as a report, not just a pass/fail.
func logNativeExtension(t *testing.T, name string, present, introduced map[string]any) {
	t.Helper()
	t.Logf("%-22s present=%v native_wheels=%v platform_tags=%v | introduced=%v at=%v prior_pure=%v checked=%v",
		name,
		present["present"], present["native_wheel_count"], present["platform_tags"],
		introduced["introduced_recently"], introduced["introduced_at_version"],
		introduced["prior_versions_without"], introduced["versions_checked"])
}

// observeNativeExtensionLive is the failure-tolerant variant used for
// the observe and ad-hoc-target groups: it reports what it found
// without failing the test, so an arbitrary module name — including one
// that 404s (removed after a campaign), exceeds the 10 MiB registry
// response cap (e.g. pydantic-core ≈ 10.5 MiB → whole collection
// degrades to absence), or has no parseable releases — reads as an
// observation rather than a red test.
func observeNativeExtensionLive(t *testing.T, name string) {
	t.Helper()
	raw, err := NewCollector().Collect(t.Context(), pypiEntity(name))
	if err != nil {
		t.Logf("%-22s COLLECT ERROR: %v", name, err)
		return
	}
	result := wrap(t, raw)
	if !hasSignal(result, "native_extension_present") {
		t.Logf("%-22s no signal (collection degraded — 404 / >10MiB response cap / no parseable releases)", name)
		return
	}
	logNativeExtension(t, name,
		getSignalValue(t, result, "native_extension_present"),
		getSignalValue(t, result, "native_extension_introduced"))
}

// TestLive_PyPINativeExtension_Curated dogfoods the signal against a
// curated spectrum: reliably-pure-Python packages, reliably-native
// packages, and campaign-relevant / previously-recorded targets. Hard
// assertions are made only on the high-confidence packages; the rest
// are logged so packaging changes upstream cannot make the test flaky.
func TestLive_PyPINativeExtension_Curated(t *testing.T) {
	// Pure-Python — ship py3-none-any wheels. present must be false.
	pure := []string{"idna", "requests"}
	// Native — ship manylinux/macos/win wheels every version in the
	// window. present must be true, introduced must be false (no recent
	// pure→native transition).
	native := []string{"numpy", "cryptography"}
	// Log-only: campaign-named legit-native (ensmallen/embiggen
	// bioinformatics cluster), Rust-backed (tiktoken, pydantic-core),
	// and previously-recorded targets (mistralai, sigstore). Packaging
	// can vary, so observe rather than assert.
	observe := []string{"tiktoken", "pydantic-core", "ensmallen", "embiggen", "mistralai", "sigstore"}

	for _, name := range pure {
		t.Run("pure/"+name, func(t *testing.T) {
			present, introduced := collectNativeExtensionLive(t, name)
			logNativeExtension(t, name, present, introduced)
			assert.Equal(t, false, present["present"],
				"%s is pure-Python (py3-none-any) → present=false", name)
			assert.Equal(t, false, introduced["introduced_recently"])
		})
	}

	for _, name := range native {
		t.Run("native/"+name, func(t *testing.T) {
			present, introduced := collectNativeExtensionLive(t, name)
			logNativeExtension(t, name, present, introduced)
			assert.Equal(t, true, present["present"],
				"%s ships compiled wheels → present=true", name)
			assert.Greater(t, present["native_wheel_count"], float64(0),
				"native package should report ≥1 native wheel")
			assert.Equal(t, false, introduced["introduced_recently"],
				"%s has been native across the window → no transition", name)
		})
	}

	for _, name := range observe {
		t.Run("observe/"+name, func(t *testing.T) {
			observeNativeExtensionLive(t, name)
		})
	}
}

// TestLive_PyPINativeExtension_Target is the ad-hoc dogfood entry point:
// feed it one module name or a comma-separated list via
// SIGNATORY_PYPI_TARGET. Skips when unset so a tagged full run does not
// require the env var.
func TestLive_PyPINativeExtension_Target(t *testing.T) {
	raw := os.Getenv("SIGNATORY_PYPI_TARGET")
	if strings.TrimSpace(raw) == "" {
		t.Skip("set SIGNATORY_PYPI_TARGET=<name>[,<name>...] to dogfood specific modules")
	}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			observeNativeExtensionLive(t, name)
		})
	}
}
