package pypi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the producer side of the wheel_url handoff that the
// pypi-wheel content collector (internal/signal/pypiwheel) consumes to
// open the published wheel. recordWheelURL runs on every Collect, but
// before these tests no fixture set a bdist_wheel with a download URL,
// so the emission branch was wholly uncovered — only the absence
// fallback ran. A rename of any payload key (the "loose in-run handoff
// contract" the code comments flag) would silently degrade every wheel
// scan to permanent absence with a green suite.

func TestCollector_Collect_WheelURL_Emitted(t *testing.T) {
	t.Parallel()
	// Newest non-yanked version ships a bdist_wheel with a URL; the
	// handoff must carry url/version/filename/integrity for that wheel.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-1.0.0-py3-none-any.whl",
					URL:      "https://files.pythonhosted.org/packages/aa/pkg-1.0.0-py3-none-any.whl",
					Digests:  Digests{SHA256: "oldhash"}},
			},
			"2.0.0": {
				// sdist listed first to prove the loop selects the wheel,
				// not whichever distribution happens to come first.
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "sdist",
					Filename: "pkg-2.0.0.tar.gz",
					URL:      "https://files.pythonhosted.org/packages/cc/pkg-2.0.0.tar.gz",
					Digests:  Digests{SHA256: "sdisthash"}},
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-2.0.0-py3-none-any.whl",
					URL:      "https://files.pythonhosted.org/packages/bb/pkg-2.0.0-py3-none-any.whl",
					Digests:  Digests{SHA256: "newhash"}},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "wheel_url"),
		"a non-yanked bdist_wheel with a URL must emit wheel_url")
	v := getSignalValue(t, result, "wheel_url")
	assert.Equal(t, "https://files.pythonhosted.org/packages/bb/pkg-2.0.0-py3-none-any.whl", v["url"],
		"newest version's wheel URL, not the co-located sdist URL")
	assert.Equal(t, "2.0.0", v["version"])
	assert.Equal(t, "pkg-2.0.0-py3-none-any.whl", v["filename"])
	assert.Equal(t, "newhash", v["integrity"],
		"integrity is the wheel's sha256, not the co-located sdist's")
}

func TestCollector_Collect_WheelURL_SkipsYankedVersion(t *testing.T) {
	t.Parallel()
	// Newest version is yanked; the handoff falls back to the newest
	// NON-yanked version so the content scanner never opens a release
	// the maintainer already pulled. Pins the `if rec.yanked` skip.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-1.0.0-py3-none-any.whl",
					URL:      "https://files.pythonhosted.org/packages/aa/pkg-1.0.0-py3-none-any.whl",
					Digests:  Digests{SHA256: "goodhash"}},
			},
			"2.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Yanked: true,
					Filename: "pkg-2.0.0-py3-none-any.whl",
					URL:      "https://files.pythonhosted.org/packages/bb/pkg-2.0.0-py3-none-any.whl",
					Digests:  Digests{SHA256: "yankedhash"}},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "wheel_url"))
	v := getSignalValue(t, result, "wheel_url")
	assert.Equal(t, "1.0.0", v["version"],
		"yanked 2.0.0 is skipped; newest non-yanked version (1.0.0) is selected")
	assert.Equal(t, "goodhash", v["integrity"])
}

func TestCollector_Collect_WheelURL_SdistOnly_Absence(t *testing.T) {
	t.Parallel()
	// A package that publishes only sdists has no wheel to open. The
	// handoff records an absence, which pypiwheel.Collect reads to emit
	// its own no-wheel absence rather than a false "clean" positive.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "sdist",
					Filename: "pkg-1.0.0.tar.gz",
					URL:      "https://files.pythonhosted.org/packages/aa/pkg-1.0.0.tar.gz",
					Digests:  Digests{SHA256: "sdisthash"}},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("sdist-only"))
	require.NoError(t, err)
	result := wrap(t, raw)

	assert.False(t, hasSignal(result, "wheel_url"),
		"sdist-only package has no bdist_wheel → no wheel_url signal")
	assert.True(t, hasAbsence(result, "wheel_url"),
		"sdist-only package records a wheel_url absence (the no-wheel handoff pypiwheel consumes)")
}
