package stream

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// Cap / limit failures: walk must STOP and return an error.
// ---------------------------------------------------------------------

// TestZip_DecompressionBomb verifies the per-walk total uncompressed
// cap fires when entry uncompressed sizes sum past it.
//
// Zip's per-entry independent compression makes "decompression bomb"
// shape slightly different from tar.gz — each entry is its own
// deflate stream. The cap defends the same way: an entry (or set of
// entries) whose declared uncompressed size exceeds the cap fails
// the walk before any decompression CPU is spent.
func TestZip_DecompressionBomb(t *testing.T) {
	t.Parallel()
	const payloadSize = 10 * 1024 * 1024
	const totalCap int64 = 1 * 1024 * 1024

	b := newZip(t).addFile("bomb.bin", bytes.Repeat([]byte{0}, payloadSize))
	raw := b.bytes()

	require.Less(t, int64(len(raw)), totalCap,
		"fixture invariant: compressed bomb must fit on disk under the cap; "+
			"otherwise the test exercises MaxCompressedBytes instead of decompression")

	manifest, err := Walk(t.Context(), bytes.NewReader(raw), FormatZip, nil,
		Limits{MaxTotalBytes: totalCap, MaxCompressedBytes: 100 << 20})

	require.Error(t, err, "Walk must refuse a zip whose entries sum past MaxTotalBytes")
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"error must wrap ErrLimitExceeded; got: %v", err)
	assert.Nil(t, manifest)
}

// TestZip_PerEntryCapEnforced verifies a single oversized entry
// fails the walk even when the total is within MaxTotalBytes.
func TestZip_PerEntryCapEnforced(t *testing.T) {
	t.Parallel()
	const entrySize = 5 * 1024 * 1024
	const entryCap int64 = 1 * 1024 * 1024

	b := newZip(t).addFile("huge.bin", bytes.Repeat([]byte{0}, entrySize))
	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil,
		Limits{MaxEntryBytes: entryCap, MaxTotalBytes: 100 << 20, MaxCompressedBytes: 100 << 20})

	require.Error(t, err, "Walk must refuse a zip with an entry exceeding MaxEntryBytes")
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"per-entry cap should surface as ErrLimitExceeded; got: %v", err)
	assert.Nil(t, manifest)
}

// TestZip_EntryCountFlood verifies the entry-count cap fires.
func TestZip_EntryCountFlood(t *testing.T) {
	t.Parallel()
	const entries = 50
	const cap_ = 10

	b := newZip(t)
	for i := range entries {
		b.addFile("e/"+strings.Repeat("x", i+1), nil)
	}
	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil,
		Limits{MaxEntries: cap_, MaxTotalBytes: 10 << 20, MaxEntryBytes: 1 << 20, MaxCompressedBytes: 100 << 20})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"entry-count cap should surface as ErrLimitExceeded; got: %v", err)
	assert.Nil(t, manifest)
}

// TestZip_CompressionRatioCap verifies the ratio cap fires for
// small-input-huge-output zips even when MaxTotalBytes is generous.
func TestZip_CompressionRatioCap(t *testing.T) {
	t.Parallel()
	const payloadSize = 10 * 1024 * 1024
	const ratioCap = 50.0

	b := newZip(t).addFile("ratio.bin", bytes.Repeat([]byte{0}, payloadSize))
	raw := b.bytes()

	actualRatio := float64(payloadSize) / float64(len(raw))
	require.Greater(t, actualRatio, ratioCap,
		"fixture invariant: ratio must exceed cap; actual=%.0f cap=%.0f",
		actualRatio, ratioCap)

	manifest, err := Walk(t.Context(), bytes.NewReader(raw), FormatZip, nil,
		Limits{
			MaxTotalBytes:       100 << 20,
			MaxEntryBytes:       100 << 20,
			MaxEntries:          1000,
			MaxCompressionRatio: ratioCap,
			MaxCompressedBytes:  100 << 20,
		})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"ratio cap should surface as ErrLimitExceeded; got: %v", err)
	assert.Nil(t, manifest)
}

// TestZip_CompressedInputCap verifies the new MaxCompressedBytes cap
// fires when the raw input bytes (before any zip parsing) exceed
// the limit.
//
// Zip requires random access (central directory at end), so the
// walker buffers compressed bytes. MaxCompressedBytes bounds that
// buffer — without it, an attacker could feed the walker an
// arbitrary-large compressed input and exhaust process memory before
// any zip-format defenses fired.
func TestZip_CompressedInputCap(t *testing.T) {
	t.Parallel()
	const compressedCap int64 = 1024

	b := newZip(t)
	for i := range 100 {
		b.addFile(strings.Repeat("a", i+1), bytes.Repeat([]byte("X"), 100))
	}
	raw := b.bytes()
	require.Greater(t, int64(len(raw)), compressedCap,
		"fixture invariant: raw zip must exceed the compressed cap")

	manifest, err := Walk(t.Context(), bytes.NewReader(raw), FormatZip, nil,
		Limits{MaxCompressedBytes: compressedCap})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"compressed-input cap should surface as ErrLimitExceeded; got: %v", err)
	assert.Nil(t, manifest)
}

// TestZip_MalformedZip verifies a non-zip input fails cleanly.
func TestZip_MalformedZip(t *testing.T) {
	t.Parallel()
	junk := []byte("definitely not a zip archive")

	manifest, err := Walk(t.Context(), bytes.NewReader(junk), FormatZip, nil, Limits{})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrLimitExceeded),
		"malformed-zip error must NOT be conflated with a cap error")
	assert.Nil(t, manifest)
}

// ---------------------------------------------------------------------
// Suspicious paths are evidence, not failures: walk SUCCEEDS and the
// dangerous patterns are recorded verbatim. See doc.go.
// ---------------------------------------------------------------------

// TestZip_PathTraversalRecordedVerbatim verifies the walker records
// "../../etc/passwd"-style entries with paths intact, mirroring the
// tar walker's stance.
//
// "Zip slip" is the standard name for this attack class in the zip
// world (Snyk, 2018) — the same defense applies: header-only
// iteration + no extraction means the path is data, not a filesystem
// operation. Recording it preserves it as evidence for the divergence
// collector.
func TestZip_PathTraversalRecordedVerbatim(t *testing.T) {
	t.Parallel()
	const traversal = "../../etc/passwd"
	const benign = "src/main.go"

	b := newZip(t).
		addFile(benign, []byte("package main\n")).
		addFile(traversal, []byte("root:x:0:0::/:/bin/sh\n"))

	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil, Limits{})

	require.NoError(t, err,
		"a zip with zip-slip entries is malformed from a security stance, "+
			"not from a zip-format stance — surface them as data, not abort")
	require.Len(t, manifest.Entries, 2)

	byPath := indexByPath(manifest.Entries)
	assert.Contains(t, byPath, traversal,
		"zip-slip entry's leading '..' must NOT be cleaned away")
	assert.Contains(t, byPath, benign)
}

// TestZip_AbsolutePathRecordedVerbatim verifies "/etc/passwd"-style
// entries are preserved with leading slash intact.
func TestZip_AbsolutePathRecordedVerbatim(t *testing.T) {
	t.Parallel()
	const absPath = "/etc/passwd"

	b := newZip(t).addFile(absPath, []byte("data"))
	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil, Limits{})

	require.NoError(t, err)
	require.Len(t, manifest.Entries, 1)
	assert.Equal(t, absPath, manifest.Entries[0].Path,
		"absolute path must be preserved; the leading '/' is the signal")
}

// TestZip_SymlinkRecordedAsSymlink verifies symlinks (encoded via
// Unix mode bits in zip's external attributes) classify as
// EntrySymlink with the link target captured from the file body.
//
// Zip differs from tar here: tar carries the link target in the
// header's Linkname field, but zip stores it as the file's content
// bytes with the IFLNK mode bit set. The walker bridges this so
// downstream consumers see a uniform LinkTarget regardless of
// archive format.
func TestZip_SymlinkRecordedAsSymlink(t *testing.T) {
	t.Parallel()
	const linkPath = "package/innocuous.txt"
	const target = "../../../../etc/passwd"

	b := newZip(t).
		addFile("package/main.go", []byte("// real source")).
		addSymlink(linkPath, target)

	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil, Limits{})

	require.NoError(t, err)
	require.Len(t, manifest.Entries, 2)

	byPath := indexByPath(manifest.Entries)
	link, ok := byPath[linkPath]
	require.True(t, ok)
	assert.Equal(t, EntrySymlink, link.Type,
		"symlink mode bit must classify the entry as EntrySymlink — "+
			"misclassifying as EntryFile would hide the link in normal-looking output")
	assert.Equal(t, target, link.LinkTarget,
		"link target must come through verbatim; for zip, the target "+
			"lives in the file body and the walker must capture it")
}

// ---------------------------------------------------------------------
// Zip-specific threats: encryption rejection + parser confusion.
// ---------------------------------------------------------------------

// TestZip_EncryptedEntryRejected verifies that an entry with the
// general-purpose flag bit 0 (encrypted) set fails the walk.
//
// The walker can't decompress encrypted entries (no key), and
// encryption in a release artifact is itself anomalous — there's no
// legitimate reason for npm/pypi/cargo/JAR/wheel publishers to ship
// encrypted entries. Rejecting surfaces the anomaly instead of
// silently dropping the entry from the manifest.
//
// The fixture is a normal zip with bit 0 of the first local header's
// general-purpose flag flipped on. The actual content isn't
// encrypted (Go's stdlib doesn't have the API), but the bit IS the
// signal — by spec, the bit declares encryption regardless of
// whether decryption would succeed.
func TestZip_EncryptedEntryRejected(t *testing.T) {
	t.Parallel()
	b := newZip(t).addFile("secret.txt", []byte("supposedly encrypted"))
	raw := markFirstLocalHeaderEncrypted(t, b.bytes())

	manifest, err := Walk(t.Context(), bytes.NewReader(raw), FormatZip, nil, Limits{})

	require.Error(t, err, "zip with encrypted-bit entry must be rejected")
	assert.True(t, errors.Is(err, ErrEncryptedEntry),
		"encryption-bit detection must surface as ErrEncryptedEntry "+
			"so consumers can branch separately from cap or malformed errors; got: %v", err)
	assert.Nil(t, manifest)
}

// TestZip_LocalCentralCountMismatch verifies that a zip whose local
// file header count differs from its central directory count fails
// the walk with ErrParserConfusion.
//
// Real attack class: different zip parsers (Go's archive/zip, Java's
// java.util.zip, Python's zipfile, etc.) resolve local-vs-central
// disagreements differently. An attacker can ship one set of files
// to one parser and a different set to another. For divergence
// detection, accepting only ONE view would let an attacker bypass
// the diff by hiding files from whichever side we read.
//
// The fixture injects an extra local file header at the boundary
// before the central directory, then rewrites the EOCD's central-dir
// offset to skip the injection — stdlib's central-dir-only view sees
// N entries, our local-header scan sees N+1.
func TestZip_LocalCentralCountMismatch(t *testing.T) {
	t.Parallel()
	b := newZip(t).
		addFile("a.txt", []byte("a")).
		addFile("b.txt", []byte("b"))
	raw := injectFakeLocalHeader(t, b.bytes(), "smuggled.dll")

	manifest, err := Walk(t.Context(), bytes.NewReader(raw), FormatZip, nil, Limits{})

	require.Error(t, err, "local-vs-central count mismatch must fail the walk")
	assert.True(t, errors.Is(err, ErrParserConfusion),
		"mismatch detection must surface as ErrParserConfusion so consumers "+
			"branch on parser-confusion separately from malformed-zip; got: %v", err)
	assert.Nil(t, manifest)
}

// ---------------------------------------------------------------------
// Capture intents: same surface as tar walker.
// ---------------------------------------------------------------------

// TestZip_CaptureIntent_BasicMatch verifies an intent matching one
// entry captures its bytes verbatim.
func TestZip_CaptureIntent_BasicMatch(t *testing.T) {
	t.Parallel()
	const targetPath = "META-INF/MANIFEST.MF"
	const targetBody = "Manifest-Version: 1.0\n"

	b := newZip(t).
		addFile("com/example/Main.class", []byte{0xCA, 0xFE, 0xBA, 0xBE}).
		addFile(targetPath, []byte(targetBody))

	intent := CaptureIntent{
		Name:    "jar-manifest",
		Match:   func(e Entry) bool { return e.Path == targetPath },
		MaxSize: 64 * 1024,
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatZip,
		[]CaptureIntent{intent}, Limits{})

	require.NoError(t, err)
	captured, ok := manifest.Captured[intent.Name]
	require.True(t, ok)
	assert.Equal(t, targetBody, string(captured),
		"captured bytes must equal entry body byte-for-byte")
}

// TestZip_CaptureIntent_OversizeSkipped verifies oversize matches
// are recorded in SkippedIntents and bytes are NOT captured.
func TestZip_CaptureIntent_OversizeSkipped(t *testing.T) {
	t.Parallel()
	const targetPath = "huge.json"
	const intentCap int64 = 1024
	bigBody := bytes.Repeat([]byte("A"), int(intentCap)*10)

	b := newZip(t).
		addFile("small.txt", []byte("ok")).
		addFile(targetPath, bigBody)

	intent := CaptureIntent{
		Name:    "huge-attempt",
		Match:   func(e Entry) bool { return e.Path == targetPath },
		MaxSize: intentCap,
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatZip,
		[]CaptureIntent{intent}, Limits{})

	require.NoError(t, err)
	assert.NotContains(t, manifest.Captured, intent.Name,
		"oversize match must NOT land in Captured")
	reason, recorded := manifest.SkippedIntents[intent.Name]
	require.True(t, recorded)
	assert.Contains(t, strings.ToLower(reason), "oversize")
}

// TestZip_CaptureIntent_DuplicateMatch verifies first-match-wins.
func TestZip_CaptureIntent_DuplicateMatch(t *testing.T) {
	t.Parallel()
	b := newZip(t).
		addFile("first.txt", []byte("FIRST")).
		addFile("second.txt", []byte("SECOND"))

	intent := CaptureIntent{
		Name:    "any-txt",
		Match:   func(e Entry) bool { return strings.HasSuffix(e.Path, ".txt") },
		MaxSize: 1024,
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatZip,
		[]CaptureIntent{intent}, Limits{})

	require.NoError(t, err)
	assert.Equal(t, "FIRST", string(manifest.Captured[intent.Name]),
		"first match must win")
	reason, ok := manifest.SkippedIntents[intent.Name]
	require.True(t, ok)
	assert.Contains(t, strings.ToLower(reason), "duplicate")
}

// TestZip_CaptureIntent_NoMatch verifies non-matching intents
// produce neither a Captured nor a SkippedIntents entry.
func TestZip_CaptureIntent_NoMatch(t *testing.T) {
	t.Parallel()
	b := newZip(t).addFile("a.txt", []byte("a")).addFile("b.txt", []byte("b"))

	intent := CaptureIntent{
		Name:    "nope",
		Match:   func(e Entry) bool { return false },
		MaxSize: 1024,
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatZip,
		[]CaptureIntent{intent}, Limits{})

	require.NoError(t, err)
	assert.NotContains(t, manifest.Captured, intent.Name)
	assert.NotContains(t, manifest.SkippedIntents, intent.Name)
}

// ---------------------------------------------------------------------
// Manifest correctness: top-dir, sha256, totals.
// ---------------------------------------------------------------------

// TestZip_StripTopDirDetected verifies the auto-detected wrapping
// directory is reported without rewriting Entry.Path values.
func TestZip_StripTopDirDetected(t *testing.T) {
	t.Parallel()
	b := newZip(t).
		addFile("mypkg-1.0/setup.py", nil).
		addFile("mypkg-1.0/README.md", nil).
		addFile("mypkg-1.0/src/main.py", nil)

	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil, Limits{})

	require.NoError(t, err)
	assert.Equal(t, "mypkg-1.0/", manifest.StrippedTopDir,
		"common 'mypkg-1.0/' prefix should be detected (PyPI sdist convention)")
	for _, e := range manifest.Entries {
		assert.True(t, strings.HasPrefix(e.Path, "mypkg-1.0/"),
			"Entry.Path must remain verbatim; got %q", e.Path)
	}
}

// TestZip_StripTopDir_NoCommonPrefix verifies StrippedTopDir is
// empty when entries don't share a single top-level directory.
func TestZip_StripTopDir_NoCommonPrefix(t *testing.T) {
	t.Parallel()
	b := newZip(t).
		addFile("a/file.txt", nil).
		addFile("b/file.txt", nil).
		addFile("README.md", nil)

	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil, Limits{})
	require.NoError(t, err)
	assert.Empty(t, manifest.StrippedTopDir)
}

// TestZip_ArchiveSHA256 verifies the manifest's hash matches the
// raw input bytes.
func TestZip_ArchiveSHA256(t *testing.T) {
	t.Parallel()
	b := newZip(t).addFile("a.txt", []byte("hello"))
	raw := b.bytes()

	want := sha256.Sum256(raw)
	wantHex := hex.EncodeToString(want[:])

	manifest, err := Walk(t.Context(), bytes.NewReader(raw), FormatZip, nil, Limits{})
	require.NoError(t, err)
	assert.Equal(t, wantHex, manifest.ArchiveSHA256)
}

// TestZip_TotalUncompressedBytes verifies the manifest reports the
// sum of accepted entry uncompressed sizes.
func TestZip_TotalUncompressedBytes(t *testing.T) {
	t.Parallel()
	body1 := bytes.Repeat([]byte("a"), 100)
	body2 := bytes.Repeat([]byte("b"), 200)
	b := newZip(t).addFile("a", body1).addFile("b", body2)

	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil, Limits{})
	require.NoError(t, err)
	assert.EqualValues(t, 300, manifest.TotalUncompressedBytes)
}

// TestZip_DefaultLimitsApply verifies a Limits{} (zero value)
// resolves to DefaultLimits inside Walk.
func TestZip_DefaultLimitsApply(t *testing.T) {
	t.Parallel()
	b := newZip(t).addFile("a.txt", []byte("small"))

	manifest, err := Walk(t.Context(), b.reader(), FormatZip, nil, Limits{})
	require.NoError(t, err,
		"a small zip must succeed with zero-value Limits — defaults must apply")
	require.NotNil(t, manifest)
	assert.Len(t, manifest.Entries, 1)
}

// TestZip_EmptyArchive verifies a zip with no entries returns a
// non-nil manifest with empty Entries.
func TestZip_EmptyArchive(t *testing.T) {
	t.Parallel()
	bb := &bytes.Buffer{}
	zw := zip.NewWriter(bb)
	require.NoError(t, zw.Close())

	manifest, err := Walk(t.Context(), bytes.NewReader(bb.Bytes()), FormatZip, nil, Limits{})
	require.NoError(t, err, "an empty zip is well-formed; walk must succeed")
	require.NotNil(t, manifest)
	assert.Empty(t, manifest.Entries)
}

// TestZip_ContextCancellation verifies that cancellation is honored.
func TestZip_ContextCancellation(t *testing.T) {
	t.Parallel()
	b := newZip(t)
	for i := range 1000 {
		b.addFile("e/"+strings.Repeat("x", i+1), bytes.Repeat([]byte("y"), 1024))
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Walk(ctx, b.reader(), FormatZip, nil,
		Limits{MaxTotalBytes: 100 << 20, MaxEntries: 10000, MaxEntryBytes: 1 << 20, MaxCompressedBytes: 100 << 20})

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled),
		"cancellation must wrap context.Canceled; got: %v", err)
}
