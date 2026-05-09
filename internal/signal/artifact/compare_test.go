package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
)

// TestCompare_XZShapedFixture is the canonical case the whole
// package was built for. It reproduces the load-bearing fact about
// CVE-2024-3094 (xz-utils, March 2024):
//
//   - The release tarball contained m4/build-to-host.m4
//   - The git tree at the corresponding tag did NOT
//
// A header-only walk + set-difference + path-shape classifier is
// enough to surface that fact as a single signal payload that the
// synthesist can pivot on. Per the threat-landscape doc, this is
// the highest-leverage signal v0.1 lacks; making this test pass
// is what closes that gap for the artifact-vs-repo dimension.
//
// The assertions cover the three things the synthesist needs to
// reason about the case:
//
//  1. The malicious file is present in extras_in_tarball_sample.
//  2. It carries category="build_glue", which is the human-
//     reviewer bucket Russ Cox / the Tukaani statement put it in
//     ("the trigger code was ... build-to-host.m4").
//  3. The pair_confidence is propagated verbatim from the caller
//     — the collector knows whether the tarball↔commit pairing
//     was an exact gitHead match (npm v≥5) or a tag-name match
//     (everywhere else), and that confidence travels with the
//     signal so the synthesist can weight evidence appropriately.
func TestCompare_XZShapedFixture(t *testing.T) {
	// Build a synthetic tarball mirroring xz-5.6.1's relevant
	// shape: a few benign source files PLUS the build-to-host.m4
	// payload that wasn't in the git tree at the v5.6.1 tag.
	tarball := buildTarGz(t, []tarEntry{
		{path: "src/lzma_decoder.c", body: []byte("// real source")},
		{path: "src/lzma_encoder.c", body: []byte("// real source")},
		{path: "configure.ac", body: []byte("AC_INIT([xz], [5.6.1])")},
		{path: "m4/build-to-host.m4", body: []byte("# attacker payload")},
	})

	// Git tree at the v5.6.1 tag — what `git ls-tree -r --name-only
	// v5.6.1` would emit. Crucially, m4/build-to-host.m4 is NOT in
	// this list. That asymmetry is the signal.
	gitPaths := []string{
		"src/lzma_decoder.c",
		"src/lzma_encoder.c",
		"configure.ac",
	}

	// Walk the tarball into a manifest, then build the comparison
	// against the supplied gitPaths. Two-step shape mirrors the
	// collector's flow: stream.Walk owns format dispatch + caps;
	// Compare owns the trust-model layer (pairing metadata, diff
	// classification) on top of the manifest.
	manifest, err := stream.Walk(t.Context(), bytes.NewReader(tarball),
		stream.FormatTarGzip, nil, stream.Limits{MaxTotalBytes: 1 << 20})
	require.NoError(t, err)

	cmp := Compare(manifest, gitPaths, CompareOptions{
		ArtifactURL:    "https://example.invalid/xz-5.6.1.tar.gz",
		GitRef:         "v5.6.1",
		GitCommit:      "0000000000000000000000000000000000000000", // synthetic
		PairConfidence: PairConfidenceTagMatch,
		SampleCap:      50,
	})

	// 1. Headline assertion: the malicious file appears in extras.
	var found *ClassifiedEntry
	for i, e := range cmp.ExtrasInTarballSample {
		if e.Path == "m4/build-to-host.m4" {
			found = &cmp.ExtrasInTarballSample[i]
			break
		}
	}
	require.NotNil(t, found,
		"m4/build-to-host.m4 must appear in extras_in_tarball_sample — "+
			"this is the load-bearing fact about CVE-2024-3094 the signal "+
			"is designed to surface")

	// 2. Category is build_glue. This is the bucket the human
	// reviewer would naturally put it in (m4 macro, autoconf
	// territory) — and the bucket the synthesist's prompt should
	// know to escalate on.
	assert.Equal(t, CategoryBuildGlue, found.Category,
		"m4/build-to-host.m4 must classify as build_glue; an *.m4 "+
			"file appearing only in the tarball is exactly the signature "+
			"shape of the xz attack")

	// 3. Pair confidence travels through verbatim. Without this,
	// the synthesist can't tell a high-confidence "definitely from
	// this commit" pairing from a tag-name guess.
	assert.Equal(t, PairConfidenceTagMatch, cmp.PairConfidence)
	assert.Equal(t, "v5.6.1", cmp.GitRef)
	assert.Equal(t, "https://example.invalid/xz-5.6.1.tar.gz", cmp.ArtifactURL)

	// Sanity: the SHA256 of the tarball bytes is computed and
	// non-empty. The exact value is opaque to this test — we
	// just need to know it's being recorded so the comparison is
	// reproducible from the signal payload alone.
	assert.NotEmpty(t, cmp.ArtifactSHA256,
		"artifact_sha256 must be populated so a reviewer can "+
			"reproduce the diff from the signal payload")
	assert.Len(t, cmp.ArtifactSHA256, 64,
		"artifact_sha256 must be the hex-encoded sha256 (64 chars)")

	// Counts agree with the sample.
	assert.Equal(t, 1, cmp.ExtrasInTarballCount,
		"exactly one tarball-only file (the m4 payload) in this fixture")
	assert.Equal(t, 1, cmp.Categories[CategoryBuildGlue],
		"category histogram must reflect the one build_glue extra")
}

// TestCompare_PyPISdistShape_PublisherInjectedFilesSuppressed pins
// the noise-floor claim for PyPI sdists: a clean package run through
// the diff produces zero extras-in-tarball once the publisher-
// injected files are accounted for.
//
// Diagnostic run (no suppression) emitted exactly four extras on
// this fixture: PKG-INFO and three files under <name>.egg-info/.
// Both classes are sdist build outputs that are never committed
// to the source repo. Without suppression, every healthy pypi
// package surfaces them as false-positive divergence — same noise
// pattern Cargo.lock produced for cargo before its suppression
// landed.
//
// PKG-INFO is a literal path. <name>.egg-info/* is a prefix
// pattern: the directory's name embeds the package name, so the
// suppression list can't be a fixed string slice the way cargo's
// is. The pattern is recognised dynamically by walking the manifest
// for entries whose first path component ends with ".egg-info".
//
// The test calls publisherMetadataPaths and the dynamic egg-info
// helper directly, mirroring the merge the collector will do at
// production-call time. Keeps Compare ecosystem-agnostic.
func TestCompare_PyPISdistShape_PublisherInjectedFilesSuppressed(t *testing.T) {
	tarball := buildTarGz(t, []tarEntry{
		{path: "hatch-1.2.3/hatch/__init__.py", body: []byte("# real source")},
		{path: "hatch-1.2.3/PKG-INFO", body: []byte("Metadata-Version: 2.1\nName: hatch\n")},
		{path: "hatch-1.2.3/hatch.egg-info/PKG-INFO", body: []byte("Metadata-Version: 2.1\n")},
		{path: "hatch-1.2.3/hatch.egg-info/SOURCES.txt", body: []byte("hatch/__init__.py\n")},
		{path: "hatch-1.2.3/hatch.egg-info/top_level.txt", body: []byte("hatch\n")},
	})

	// What `git ls-tree -r --name-only v1.2.3` would emit on a clean repo.
	// PKG-INFO and *.egg-info/* are never committed; they're sdist build outputs.
	gitPaths := []string{
		"hatch/__init__.py",
	}

	manifest, err := stream.Walk(t.Context(), bytes.NewReader(tarball),
		stream.FormatTarGzip, nil, stream.Limits{MaxTotalBytes: 1 << 20})
	require.NoError(t, err)

	// Production merge: collector appends static publisher metadata
	// paths (literal) and dynamic egg-info paths (manifest-derived)
	// to gitPaths so Compare's set-difference treats them as
	// already-present-in-git.
	gitPaths = append(gitPaths, publisherMetadataPaths("pypi")...)
	gitPaths = append(gitPaths, eggInfoPaths(manifest)...)

	cmp := Compare(manifest, gitPaths, CompareOptions{
		ArtifactURL:    "https://files.pythonhosted.org/packages/aa/bb/hatch-1.2.3.tar.gz",
		GitRef:         "v1.2.3",
		PairConfidence: PairConfidenceTagMatch,
		SampleCap:      50,
	})

	extras := make([]string, 0, len(cmp.ExtrasInTarballSample))
	for _, e := range cmp.ExtrasInTarballSample {
		extras = append(extras, e.Path)
	}

	assert.Empty(t, extras,
		"on a clean pypi sdist, all four publisher-injected files "+
			"(PKG-INFO + three under <name>.egg-info/) must be suppressed; "+
			"any leftover entries here are real divergence the diff should surface")
	assert.Equal(t, 0, cmp.ExtrasInTarballCount,
		"count must agree with the empty sample")
}

// tarEntry is a tiny test-only constructor type — only what's
// needed to express "regular file with these bytes at this path".
type tarEntry struct {
	path string
	body []byte
}

// buildTarGz writes the entries as a gzip-compressed tar archive
// in memory and returns the bytes. Test-only — production code
// only ever READS tarballs.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     e.path,
			Size:     int64(len(e.body)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}))
		_, err := io.Copy(tw, bytes.NewReader(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}
