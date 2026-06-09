package pypi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the PyPI parity for the gem ecosystem's
// native_extension_present / native_extension_introduced signals
// (internal/signal/registry/gem/collector.go). PyPI has no single
// per-version "platform" field like gem; native-ness is derived from
// the wheel filename's PEP 425 platform tag: py3-none-any (and the
// py2.py3-none-any form) is pure-Python, while a concrete platform tag
// (manylinux / macosx / win / musllinux) means a compiled extension
// ships in the wheel — the surface the 2026-06 Miasma/Hades PyPI
// campaign abused with trojanized .abi3.so extensions.

// ----- wheel platform-tag parser -----

func TestNativeWheel_PlatformTagParsing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		filename string
		wantTag  string
		wantOK   bool
		native   bool
	}{
		{"pure py3", "pkg-1.0.0-py3-none-any.whl", "any", true, false},
		{"pure py2.py3", "pkg-1.0.0-py2.py3-none-any.whl", "any", true, false},
		{"manylinux", "pkg-1.0.0-cp311-cp311-manylinux_2_17_x86_64.whl", "manylinux_2_17_x86_64", true, true},
		{"abi3 macos", "pkg-2.0.0-cp39-abi3-macosx_11_0_arm64.whl", "macosx_11_0_arm64", true, true},
		{"win", "pkg-1.0.0-cp312-cp312-win_amd64.whl", "win_amd64", true, true},
		{"musllinux", "p-1.0-cp310-cp310-musllinux_1_1_x86_64.whl", "musllinux_1_1_x86_64", true, true},
		{"build tag present", "pkg-1.0.0-1-cp311-cp311-manylinux_2_17_x86_64.whl", "manylinux_2_17_x86_64", true, true},
		{"sdist tarball", "pkg-1.0.0.tar.gz", "", false, false},
		{"egg", "pkg-1.0.0-py3.6.egg", "", false, false},
		{"malformed too few parts", "pkg-1.0.0.whl", "", false, false},
		{"empty", "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tag, ok := wheelPlatformTag(tc.filename)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantTag, tag)
			assert.Equal(t, tc.native, isNativeWheelFilename(tc.filename))
		})
	}
}

func TestNativeWheelSummary_DedupesAndCounts(t *testing.T) {
	t.Parallel()
	// Three native wheels across two platforms (manylinux x2 for two
	// python ABIs, macos x1) plus an sdist and a pure-Python wheel.
	dists := []Distribution{
		{Filename: "p-1.0-cp310-cp310-manylinux_2_17_x86_64.whl"},
		{Filename: "p-1.0-cp311-cp311-manylinux_2_17_x86_64.whl"},
		{Filename: "p-1.0-cp311-abi3-macosx_11_0_arm64.whl"},
		{Filename: "p-1.0-py3-none-any.whl"},
		{Filename: "p-1.0.tar.gz"},
	}
	present, count, tags := nativeWheelSummary(dists)
	assert.True(t, present)
	assert.Equal(t, 3, count, "three native wheel files")
	assert.Equal(t, []string{"manylinux_2_17_x86_64", "macosx_11_0_arm64"}, tags,
		"deduped to two distinct platform tags, first-seen order")
}

// ----- native_extension_present -----

func TestCollector_Collect_NativeExtensionPresent_True(t *testing.T) {
	t.Parallel()
	// Latest version ships compiled wheels — native extension present.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-1.0.0-cp311-cp311-manylinux_2_17_x86_64.whl"},
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-1.0.0-cp39-abi3-macosx_11_0_arm64.whl"},
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "sdist",
					Filename: "pkg-1.0.0.tar.gz"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("native-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "native_extension_present"))
	v := getSignalValue(t, result, "native_extension_present")
	assert.Equal(t, true, v["present"],
		"latest version ships compiled wheels → present=true")
	assert.Equal(t, "1.0.0", v["version_checked"])
	assert.EqualValues(t, 2, v["native_wheel_count"],
		"two of three dists are non-any-platform wheels")

	// platform_tags JSON-decodes to []interface{}; Contains compares
	// each element by value, so a bare string target matches.
	assert.Contains(t, v["platform_tags"], "manylinux_2_17_x86_64")
	assert.Contains(t, v["platform_tags"], "macosx_11_0_arm64")
}

func TestCollector_Collect_NativeExtensionPresent_False_PureWheel(t *testing.T) {
	t.Parallel()
	// Latest version ships only a pure-Python wheel — no native code.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-1.0.0-py3-none-any.whl"},
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "sdist",
					Filename: "pkg-1.0.0.tar.gz"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pure-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "native_extension_present"))
	v := getSignalValue(t, result, "native_extension_present")
	assert.Equal(t, false, v["present"],
		"py3-none-any wheel is pure-Python → present=false")
	assert.EqualValues(t, 0, v["native_wheel_count"])
}

// ----- native_extension_introduced -----

func TestCollector_Collect_NativeExtensionIntroduced_DetectsTransition(t *testing.T) {
	t.Parallel()
	// A pure-Python project (the MCP-themed masquerade shape) that
	// suddenly ships a compiled extension in its latest version — the
	// .abi3.so injection vector. Versions 1.0/1.1 were py3-none-any;
	// 2.0 introduces a manylinux wheel.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "attacker"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-1.0.0-py3-none-any.whl"},
			},
			"1.1.0": {
				{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-1.1.0-py3-none-any.whl"},
			},
			"2.0.0": {
				{UploadTimeISO: "2026-04-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-2.0.0-cp312-cp312-manylinux_2_17_x86_64.whl"},
				{UploadTimeISO: "2026-04-01T00:00:00Z", PackageType: "sdist",
					Filename: "pkg-2.0.0.tar.gz"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("compromised"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "native_extension_introduced"))
	v := getSignalValue(t, result, "native_extension_introduced")
	assert.Equal(t, true, v["present_in_latest"],
		"latest ships a compiled wheel")
	assert.Equal(t, true, v["introduced_recently"],
		"pure-Python → native transition should flag")
	assert.Equal(t, "2.0.0", v["introduced_at_version"])
	assert.EqualValues(t, 2, v["prior_versions_without"],
		"both prior versions were pure-Python")
	assert.EqualValues(t, 3, v["versions_checked"])
}

func TestCollector_Collect_NativeExtensionIntroduced_AlwaysNative(t *testing.T) {
	t.Parallel()
	// A legitimately-native package (numpy/scipy shape, or the
	// embiggen/ensmallen bioinformatics cluster): compiled from its
	// first published version. Native presence is real, but there is
	// NO transition — this signal deliberately does not fire on the
	// already-native package, which is the honest limitation the
	// caveats document.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"0.1.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-0.1.0-cp310-cp310-manylinux_2_17_x86_64.whl"},
			},
			"0.2.0": {
				{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-0.2.0-cp311-cp311-manylinux_2_17_x86_64.whl"},
			},
			"0.3.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-0.3.0-cp312-cp312-macosx_11_0_arm64.whl"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("numpy-shape"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "native_extension_introduced"))
	v := getSignalValue(t, result, "native_extension_introduced")
	assert.Equal(t, true, v["present_in_latest"])
	assert.Equal(t, false, v["introduced_recently"],
		"native from the first version — not a transition")
	assert.EqualValues(t, 0, v["prior_versions_without"],
		"no prior version was pure-Python")
}

func TestCollector_Collect_NativeExtensionIntroduced_PureThroughout(t *testing.T) {
	t.Parallel()
	// Pure-Python throughout — never native, no transition.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-1.0.0-py3-none-any.whl"},
			},
			"2.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel",
					Filename: "pkg-2.0.0-py2.py3-none-any.whl"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pure-throughout"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "native_extension_introduced"))
	v := getSignalValue(t, result, "native_extension_introduced")
	assert.Equal(t, false, v["present_in_latest"],
		"py2.py3-none-any is still pure-Python")
	assert.Equal(t, false, v["introduced_recently"])
}
