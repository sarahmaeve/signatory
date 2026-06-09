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
// as the production assembly wires them. Returns the decoded
// wheel_pth_executable payload, or (nil,false) when no wheel was opened
// (sdist-only / 404 / oversized registry response).
func scanWheelLive(t *testing.T, name string) (map[string]any, bool) {
	t.Helper()
	ent := liveEntity(name)

	regRes, err := pypicollector.NewCollector().Collect(t.Context(), ent)
	require.NoError(t, err, "registry collect for %q", name)

	fetcher := artifactcollector.NewStreamArtifactFetcher(
		artifactcollector.StreamFetcherOptions{MaxBytes: 256 << 20, Timeout: 60 * time.Second})

	res, err := NewCollector(CollectorConfig{InRun: regRes, Fetcher: fetcher}).
		Collect(t.Context(), ent)
	require.NoError(t, err, "wheel collect for %q", name)
	return signalValue(t, res, "wheel_pth_executable")
}

func logWheelPth(t *testing.T, name string, v map[string]any, opened bool) {
	t.Helper()
	if !opened {
		t.Logf("%-20s no wheel opened (sdist-only / 404 / >10MiB registry cap)", name)
		return
	}
	t.Logf("%-20s pth_scanned=%v findings=%v files=%v",
		name, v["pth_files_scanned"], v["total_finding_count"], v["files_with_findings"])
}

func TestLive_WheelPth_Curated(t *testing.T) {
	// Clean packages: a pure-Python wheel with no .pth (requests) and
	// namespace packages that ship legitimate setuptools *-nspkg.pth
	// shims (zope.interface, google-api-core) — all must scan to ZERO
	// findings, proving the benign-twin discrimination holds on live
	// wheels, not just synthetic fixtures.
	clean := []string{"requests", "zope.interface", "google-api-core"}

	for _, name := range clean {
		t.Run(name, func(t *testing.T) {
			v, opened := scanWheelLive(t, name)
			logWheelPth(t, name, v, opened)
			if !opened {
				t.Skipf("%s: no wheel opened — nothing to assert", name)
			}
			assert.EqualValues(t, 0, v["total_finding_count"],
				"%s wheel .pth surface must be clean (no dangerous primitives)", name)
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
			v, opened := scanWheelLive(t, name)
			logWheelPth(t, name, v, opened)
		})
	}
}
