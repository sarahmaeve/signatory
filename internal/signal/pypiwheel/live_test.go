//go:build network_access_ok

// These tests hit the real PyPI registry AND download real wheels.
// Not run by default — same network_access_ok gate as the cmd/signatory
// and registry/pypi live tests.
//
// Curated set:
//
//	go test -tags network_access_ok -run Live_WheelPth ./internal/signal/pypiwheel/ -v
//
// Ad-hoc target(s):
//
//	SIGNATORY_PYPI_TARGET=zope.interface,requests \
//	    go test -tags network_access_ok -run Live_WheelPth_Target \
//	    ./internal/signal/pypiwheel/ -v
//
// End-to-end: runs the real pypi registry collector to emit wheel_url,
// then this collector fetches and scans the real wheel. Validates the
// .pth scan against live wheels — including legitimate namespace
// packages that ship setuptools *-nspkg.pth shims (which must NOT flag).

package pypiwheel

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	artifactcollector "github.com/sarahmaeve/signatory/internal/signal/artifact"
	pypicollector "github.com/sarahmaeve/signatory/internal/signal/registry/pypi"
)

func liveEntity(name string) *profile.Entity {
	return &profile.Entity{
		ID:           "e-" + name,
		Ecosystem:    "pypi",
		CanonicalURI: "pkg:pypi/" + name,
		ShortName:    name,
		Type:         profile.EntityPackage,
	}
}

// scanWheelLive runs the real registry collector (to emit wheel_url)
// then this collector against the resulting in-run accumulator, exactly
// as the production assembly wires them. Returns the wheel_pth_executable
// and wheel_bundled_payload payloads, and whether a wheel was opened
// (false ⇒ sdist-only / 404 / oversized registry response).
func scanWheelLive(t *testing.T, name string) (pth, bundled map[string]any, opened bool) {
	t.Helper()
	ent := liveEntity(name)

	regRes, err := pypicollector.NewCollector().Collect(t.Context(), ent)
	require.NoError(t, err, "registry collect for %q", name)

	fetcher := artifactcollector.NewStreamArtifactFetcher(
		artifactcollector.StreamFetcherOptions{MaxBytes: 256 << 20, Timeout: 60 * time.Second})

	res, err := NewCollector(CollectorConfig{InRun: regRes, Fetcher: fetcher}).
		Collect(t.Context(), ent)
	require.NoError(t, err, "wheel collect for %q", name)

	pth, opened = signalValue(t, res, "wheel_pth_executable")
	bundled, _ = signalValue(t, res, "wheel_bundled_payload")
	return pth, bundled, opened
}

func logWheelScan(t *testing.T, name string, pth, bundled map[string]any, opened bool) {
	t.Helper()
	if !opened {
		t.Logf("%-18s no wheel opened (sdist-only / 404 / >10MiB registry cap)", name)
		return
	}
	t.Logf("%-18s pth: scanned=%v findings=%v | bundled: scripts=%v injection=%v native=%v co_located=%v",
		name,
		pth["pth_files_scanned"], pth["total_finding_count"],
		bundled["foreign_scripts_scanned"], bundled["injection_finding_count"],
		bundled["native_lib_count"], bundled["co_located_native_and_script"])

	// Detail any bundled-script injection findings (path → primitives) so
	// a real hit — or a false positive surfaced in dogfood — is legible.
	if scripts, _ := bundled["scripts_with_injection"].([]any); len(scripts) > 0 {
		for _, s := range scripts {
			m := s.(map[string]any)
			var prims []string
			for _, f := range m["findings"].([]any) {
				fm := f.(map[string]any)
				prims = append(prims, fmt.Sprintf("%v(%v)", fm["primitive"], fm["count"]))
			}
			t.Logf("    %s → %s", m["path"], strings.Join(prims, ","))
		}
	}
}

func TestLive_WheelPth_Curated(t *testing.T) {
	// Clean packages: a pure-Python wheel with no .pth (requests),
	// namespace packages that ship legitimate setuptools shims
	// (zope.interface, google-api-core), and — the load-bearing FP check
	// — packages that bundle LOTS of legitimate JS (plotly, streamlit).
	// All must scan to ZERO .pth findings AND zero bundled-script
	// injection findings, proving the discrimination holds on real wheels
	// at scale, not just synthetic fixtures.
	clean := []string{"requests", "zope.interface", "google-api-core", "plotly", "streamlit"}

	for _, name := range clean {
		t.Run(name, func(t *testing.T) {
			pth, bundled, opened := scanWheelLive(t, name)
			logWheelScan(t, name, pth, bundled, opened)
			if !opened {
				t.Skipf("%s: no wheel opened — nothing to assert", name)
			}
			assert.EqualValues(t, 0, pth["total_finding_count"],
				"%s wheel .pth surface must be clean", name)
			assert.EqualValues(t, 0, bundled["injection_finding_count"],
				"%s bundled scripts must produce no content-injection findings", name)
		})
	}
}

func TestLive_WheelPth_Target(t *testing.T) {
	raw := os.Getenv("SIGNATORY_PYPI_TARGET")
	if strings.TrimSpace(raw) == "" {
		t.Skip("set SIGNATORY_PYPI_TARGET=<name>[,<name>...] to scan specific wheels")
	}
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			pth, bundled, opened := scanWheelLive(t, name)
			logWheelScan(t, name, pth, bundled, opened)
		})
	}
}
