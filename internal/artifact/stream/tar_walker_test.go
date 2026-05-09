package stream

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
// These protect process resources against adversarial archives.
// ---------------------------------------------------------------------

// TestTar_DecompressionBomb verifies the per-walk total uncompressed
// cap fires when the gunzipped stream exceeds it.
//
// Threat: a tarball that's small on disk (a few KB) can decompress
// to gigabytes. Even with header-only iteration, the tar format
// interleaves headers with content — finding header N+1 means
// streaming through entry N's body. Without a cap, an attacker-
// controlled archive pins analyze at 100% CPU and exhausts memory
// chasing headers in a stream of attacker-controlled length.
//
// The construction: one tar entry whose declared size is large,
// padded with zero bytes that gzip compresses ~1000:1. On-disk
// archive is a few KB; decompressed stream is 10 MB. Cap is 1 MB.
// Walk must stop with ErrLimitExceeded before the entry finishes.
func TestTar_DecompressionBomb(t *testing.T) {
	t.Parallel()
	const payloadSize int64 = 10 * 1024 * 1024
	const totalCap int64 = 1 * 1024 * 1024

	bb := &bytes.Buffer{}
	gz := gzip.NewWriter(bb)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "bomb.bin", Size: payloadSize, Mode: 0o644, Typeflag: tar.TypeReg,
	}))
	zeros := make([]byte, 64*1024)
	remaining := payloadSize
	for remaining > 0 {
		n := min(int64(len(zeros)), remaining)
		_, err := tw.Write(zeros[:n])
		require.NoError(t, err)
		remaining -= n
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	// Sanity-check the fixture before exercising the cap: if gzip
	// ever stops collapsing zero runs this aggressively, this assert
	// surfaces the fixture problem before the cap test does.
	require.Less(t, int64(bb.Len()), totalCap,
		"fixture invariant: compressed bomb must fit under the cap on disk; "+
			"otherwise the test isn't exercising the decompression path")

	manifest, err := Walk(t.Context(), bytes.NewReader(bb.Bytes()),
		FormatTarGzip, nil, Limits{MaxTotalBytes: totalCap})

	require.Error(t, err, "Walk must refuse a bomb that exceeds MaxTotalBytes")
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"error must wrap ErrLimitExceeded so callers can distinguish "+
			"a cap-triggered failure from a malformed-archive failure; got: %v", err)
	assert.Nil(t, manifest,
		"a partial manifest hides the entry that caused the abort and "+
			"makes downstream code falsely confident the walk completed")
}

// TestTar_PerEntryCapEnforced verifies a single oversized entry
// fails the walk even when the total stream is within MaxTotalBytes.
//
// Threat: a per-entry cap protects against archives that ship one
// huge file (say a 200 MiB blob) under a generous total cap. Without
// per-entry, an attacker could ship one massive entry that single-
// handedly consumes the analyzer's memory or processing budget.
func TestTar_PerEntryCapEnforced(t *testing.T) {
	t.Parallel()
	const entrySize int64 = 5 * 1024 * 1024
	const entryCap int64 = 1 * 1024 * 1024

	bb := &bytes.Buffer{}
	gz := gzip.NewWriter(bb)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "huge.bin", Size: entrySize, Mode: 0o644, Typeflag: tar.TypeReg,
	}))
	zeros := make([]byte, 64*1024)
	remaining := entrySize
	for remaining > 0 {
		n := min(int64(len(zeros)), remaining)
		_, err := tw.Write(zeros[:n])
		require.NoError(t, err)
		remaining -= n
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	manifest, err := Walk(t.Context(), bytes.NewReader(bb.Bytes()), FormatTarGzip, nil,
		Limits{MaxEntryBytes: entryCap, MaxTotalBytes: 100 << 20})

	require.Error(t, err, "Walk must refuse a single entry that exceeds MaxEntryBytes")
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"per-entry cap and total cap should both surface as ErrLimitExceeded "+
			"so callers can branch on cap-vs-malformed without disambiguating which cap; got: %v", err)
	assert.Nil(t, manifest)
}

// TestTar_EntryCountFlood verifies the entry-count cap fires.
//
// Threat: many-tiny-files DoS. An attacker could ship a tarball of
// 10 million empty entries, each a few hundred header bytes. Total
// bytes well under any size cap; the cost is in the iteration
// itself. MaxEntries caps the structural cost.
func TestTar_EntryCountFlood(t *testing.T) {
	t.Parallel()
	const entries = 50
	const cap_ = 10

	b := newTarGz(t)
	for i := range entries {
		b.addFile("e/"+strings.Repeat("x", i+1), nil)
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil,
		Limits{MaxEntries: cap_, MaxTotalBytes: 10 << 20, MaxEntryBytes: 1 << 20})

	require.Error(t, err, "Walk must refuse archives exceeding MaxEntries")
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"entry-count cap should surface as ErrLimitExceeded; got: %v", err)
	assert.Nil(t, manifest)
}

// TestTar_CompressionRatioCap verifies the compression-ratio cap
// fires for small-input-huge-output archives even when the absolute
// total is under MaxTotalBytes.
//
// Threat: an archive whose expansion ratio is suspicious (1000:1)
// can be a zip-bomb signature even at low absolute volume. Catching
// the ratio early is cheaper than waiting for MaxTotalBytes when a
// caller has loosened that cap for legitimate large artifacts.
//
// MaxTotalBytes is set generously here (100 MiB) so the test
// isolates the ratio defense; the bomb decompresses to ~10 MiB
// (well under) but the ratio is well over 100:1.
func TestTar_CompressionRatioCap(t *testing.T) {
	t.Parallel()
	const payloadSize int64 = 10 * 1024 * 1024 // 10 MiB
	const ratioCap = 50.0                      // permit 50:1, reject above

	bb := &bytes.Buffer{}
	gz := gzip.NewWriter(bb)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "ratio.bin", Size: payloadSize, Mode: 0o644, Typeflag: tar.TypeReg,
	}))
	zeros := make([]byte, 64*1024)
	remaining := payloadSize
	for remaining > 0 {
		n := min(int64(len(zeros)), remaining)
		_, err := tw.Write(zeros[:n])
		require.NoError(t, err)
		remaining -= n
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	// Confirm the actual ratio for this fixture exceeds the cap.
	actualRatio := float64(payloadSize) / float64(bb.Len())
	require.Greater(t, actualRatio, ratioCap,
		"fixture invariant: compressed/decompressed ratio must exceed "+
			"the cap, otherwise the test isn't exercising the ratio defense; "+
			"actual=%.0f cap=%.0f", actualRatio, ratioCap)

	manifest, err := Walk(t.Context(), bytes.NewReader(bb.Bytes()), FormatTarGzip, nil,
		Limits{
			MaxTotalBytes:       100 << 20, // generous, so MaxTotalBytes doesn't fire
			MaxEntryBytes:       100 << 20,
			MaxEntries:          1000,
			MaxCompressionRatio: ratioCap,
		})

	require.Error(t, err, "Walk must refuse an archive whose expansion ratio exceeds the cap")
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"ratio cap should surface as ErrLimitExceeded so callers can branch "+
			"on cap-vs-malformed without disambiguating which cap; got: %v", err)
	assert.Nil(t, manifest)
}

// TestTar_MalformedGzip verifies a non-gzip input fails cleanly with
// a wrapped error rather than panicking or returning a partial
// manifest.
func TestTar_MalformedGzip(t *testing.T) {
	t.Parallel()
	junk := []byte("this is not a gzip stream — not even close")

	manifest, err := Walk(t.Context(), bytes.NewReader(junk), FormatTarGzip, nil, Limits{})
	require.Error(t, err, "Walk must reject non-gzip input")
	assert.False(t, errors.Is(err, ErrLimitExceeded),
		"a malformed-archive error must NOT be conflated with a cap error")
	assert.Nil(t, manifest)
}

// ---------------------------------------------------------------------
// Suspicious paths are evidence, not failures: walk SUCCEEDS and the
// dangerous patterns are recorded verbatim. See doc.go.
// ---------------------------------------------------------------------

// TestTar_PathTraversalRecordedVerbatim verifies the walker records
// "../../etc/passwd" entries with paths intact rather than rejecting
// or sanitizing.
//
// The divergence collector NEEDS to see suspicious paths verbatim:
// "this tarball contained ../../etc/passwd, which the source repo
// does not have" IS the signal. A walker that sanitized away the
// leading ".." components would hide the most diagnostic evidence
// the consumer has. Defense is structural (no disk writes), not
// path-based.
func TestTar_PathTraversalRecordedVerbatim(t *testing.T) {
	t.Parallel()
	const traversal = "../../etc/passwd"
	const benign = "src/main.go"

	b := newTarGz(t).
		addFile(benign, []byte("package main\n")).
		addFile(traversal, []byte("root:x:0:0::/:/bin/sh\n"))

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})

	require.NoError(t, err,
		"a tarball containing path-traversal entries is malformed from a "+
			"security stance, not from a tar-format stance — the walker "+
			"must surface them as data, not abort the walk")
	require.Len(t, manifest.Entries, 2,
		"both entries must be preserved; suppressing the dangerous one "+
			"would hide the very evidence the divergence signal needs")

	byPath := indexByPath(manifest.Entries)
	assert.Contains(t, byPath, traversal,
		"traversal entry's leading '..' must NOT be cleaned away")
	assert.Contains(t, byPath, benign)
}

// TestTar_AbsolutePathRecordedVerbatim verifies an entry like
// "/etc/passwd" lands in the manifest with its leading slash intact.
func TestTar_AbsolutePathRecordedVerbatim(t *testing.T) {
	t.Parallel()
	const absPath = "/etc/passwd"

	b := newTarGz(t).addFile(absPath, []byte("data"))
	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})

	require.NoError(t, err)
	require.Len(t, manifest.Entries, 1)
	assert.Equal(t, absPath, manifest.Entries[0].Path,
		"absolute path must be preserved verbatim; the leading '/' is "+
			"the divergence signal a defender needs to see")
}

// TestTar_BackslashAndDriveLetterRecordedVerbatim verifies the
// walker doesn't normalize Windows-style separators or drive
// letters. These shouldn't appear in legitimate Unix-format release
// tarballs; their presence is itself diagnostic.
func TestTar_BackslashAndDriveLetterRecordedVerbatim(t *testing.T) {
	t.Parallel()
	const winStyle = `C:\Windows\System32\config\sam`

	b := newTarGz(t).addFile(winStyle, []byte("data"))
	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})

	require.NoError(t, err)
	require.Len(t, manifest.Entries, 1)
	assert.Equal(t, winStyle, manifest.Entries[0].Path,
		"Windows-style path must survive unmodified — its anomaly is "+
			"the signal")
}

// TestTar_SymlinkEscapingTargetRecordedVerbatim verifies a symlink
// pointing outside the (non-existent) archive root preserves its
// LinkTarget verbatim, classifies the entry as EntrySymlink, and
// the walker never follows it.
//
// Following the symlink would re-introduce the entire class of
// symlink-escape vulnerabilities that header-only walking exists to
// eliminate. The only correct behavior is to record the link as
// data and let downstream classification flag it.
func TestTar_SymlinkEscapingTargetRecordedVerbatim(t *testing.T) {
	t.Parallel()
	const linkPath = "package/innocuous.txt"
	const target = "../../../../etc/passwd"

	b := newTarGz(t).
		addFile("package/main.go", []byte("// real source")).
		addSymlink(linkPath, target)

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})

	require.NoError(t, err)
	require.Len(t, manifest.Entries, 2)

	byPath := indexByPath(manifest.Entries)
	link := byPath[linkPath]
	assert.Equal(t, target, link.LinkTarget,
		"symlink target must be preserved verbatim; the walker is "+
			"recording evidence, not performing link resolution")
	assert.Equal(t, EntrySymlink, link.Type,
		"symlink classification must be preserved so consumers can "+
			"flag non-regular entries as part of the divergence surface")
}

// TestTar_HardlinkRecordedAsHardlink verifies hardlink entries are
// classified as EntryHardlink and their target is preserved.
func TestTar_HardlinkRecordedAsHardlink(t *testing.T) {
	t.Parallel()
	const linkPath = "duplicate"
	const target = "../escape/route"

	b := newTarGz(t).
		addFile("real.txt", []byte("data")).
		addHardlink(linkPath, target)

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})

	require.NoError(t, err)
	byPath := indexByPath(manifest.Entries)
	link, ok := byPath[linkPath]
	require.True(t, ok)
	assert.Equal(t, EntryHardlink, link.Type)
	assert.Equal(t, target, link.LinkTarget)
}

// TestTar_DeviceNodeClassifiedAsOther verifies that entries with
// non-standard typeflags (fifo, char device, block device) land in
// the manifest classified as EntryOther rather than panicking,
// being dropped, or being mis-classified as files.
//
// Their presence in a release tarball is itself anomalous; the
// walker preserves them so consumers can surface "this tarball
// shipped a fifo, which is bizarre" as evidence.
func TestTar_DeviceNodeClassifiedAsOther(t *testing.T) {
	t.Parallel()
	b := newTarGz(t).
		addFile("real.go", nil).
		addOther("strange.fifo", tar.TypeFifo).
		addOther("char.dev", tar.TypeChar)

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})

	require.NoError(t, err)
	require.Len(t, manifest.Entries, 3)

	byPath := indexByPath(manifest.Entries)
	assert.Equal(t, EntryOther, byPath["strange.fifo"].Type,
		"fifo entries must classify as EntryOther so consumers can "+
			"flag them; misclassifying as EntryFile would hide them in "+
			"normal-looking divergence output")
	assert.Equal(t, EntryOther, byPath["char.dev"].Type)
}

// ---------------------------------------------------------------------
// Capture intents: targeted in-memory byte capture under hard caps.
// This is the ONLY way to obtain entry contents from the walker.
// ---------------------------------------------------------------------

// TestTar_CaptureIntent_BasicMatch verifies an intent matching one
// entry captures its bytes verbatim into Manifest.Captured.
//
// Use case anchor: cargo divergence registers a capture intent for
// .cargo_vcs_info.json so it can recover the publish-time SHA from
// the tarball post-fetch. The walker is the security-checked
// substrate that capture rides on.
func TestTar_CaptureIntent_BasicMatch(t *testing.T) {
	t.Parallel()
	const targetPath = "package/.cargo_vcs_info.json"
	const targetBody = `{"git":{"sha1":"deadbeef"}}`

	b := newTarGz(t).
		addFile("package/Cargo.toml", []byte("[package]\nname=\"x\"")).
		addFile(targetPath, []byte(targetBody)).
		addFile("package/src/lib.rs", []byte("// code"))

	intent := CaptureIntent{
		Name:    "cargo-vcs-info",
		Match:   func(e Entry) bool { return e.Path == targetPath },
		MaxSize: 64 * 1024,
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip,
		[]CaptureIntent{intent}, Limits{})

	require.NoError(t, err)
	captured, ok := manifest.Captured[intent.Name]
	require.True(t, ok, "matched-and-under-cap intent must produce a Captured entry")
	assert.Equal(t, targetBody, string(captured),
		"captured bytes must equal the entry body byte-for-byte; "+
			"corruption would silently change the SHA the consumer parses out")
	assert.NotContains(t, manifest.SkippedIntents, intent.Name,
		"a successful capture must NOT also appear in SkippedIntents")
}

// TestTar_CaptureIntent_OversizeSkipped verifies that an intent
// matching an entry larger than its MaxSize is recorded in
// SkippedIntents and the bytes are NOT captured.
//
// This is the load-bearing cap that lets the walker remain safe
// when an attacker controls a file the consumer would otherwise
// pull into memory. Without the cap, capture intents would
// re-introduce the very memory-exhaustion risk header-only walking
// exists to eliminate.
func TestTar_CaptureIntent_OversizeSkipped(t *testing.T) {
	t.Parallel()
	const targetPath = "huge.json"
	const intentCap int64 = 1024
	bigBody := bytes.Repeat([]byte("A"), int(intentCap)*10) // 10x the cap

	b := newTarGz(t).
		addFile("small.txt", []byte("ok")).
		addFile(targetPath, bigBody)

	intent := CaptureIntent{
		Name:    "huge-attempt",
		Match:   func(e Entry) bool { return e.Path == targetPath },
		MaxSize: intentCap,
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip,
		[]CaptureIntent{intent}, Limits{})

	require.NoError(t, err, "walk must succeed; oversize is per-intent, not per-walk")
	assert.NotContains(t, manifest.Captured, intent.Name,
		"oversize matches must NOT land in Captured — the cap exists "+
			"precisely to prevent attacker-controlled bytes from entering memory")
	reason, recorded := manifest.SkippedIntents[intent.Name]
	require.True(t, recorded,
		"oversize intent matches must surface in SkippedIntents so the "+
			"consumer knows the intent fired but didn't capture")
	assert.Contains(t, strings.ToLower(reason), "oversize",
		"reason must indicate WHY capture was skipped; got: %q", reason)
}

// TestTar_CaptureIntent_DuplicateMatch verifies that when an intent
// matches multiple entries, only the FIRST is captured and
// subsequent matches are recorded as duplicates.
//
// Why first-wins: a tarball with two .cargo_vcs_info.json files
// (one at root, one squatting under src/) is itself suspicious. The
// first-seen-wins policy is documented; consumers needing more
// sophisticated tie-breaking can match more narrowly.
func TestTar_CaptureIntent_DuplicateMatch(t *testing.T) {
	t.Parallel()
	b := newTarGz(t).
		addFile("first.txt", []byte("FIRST")).
		addFile("second.txt", []byte("SECOND"))

	intent := CaptureIntent{
		Name:    "any-txt",
		Match:   func(e Entry) bool { return strings.HasSuffix(e.Path, ".txt") },
		MaxSize: 1024,
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip,
		[]CaptureIntent{intent}, Limits{})

	require.NoError(t, err)
	assert.Equal(t, "FIRST", string(manifest.Captured[intent.Name]),
		"first match must win; subsequent matches must NOT overwrite")
	reason, ok := manifest.SkippedIntents[intent.Name]
	require.True(t, ok,
		"the second match must be recorded so consumers know the intent "+
			"matched ambiguously rather than silently losing the second match")
	assert.Contains(t, strings.ToLower(reason), "duplicate",
		"reason must indicate the skip was due to duplication; got: %q", reason)
}

// TestTar_CaptureIntent_NoMatch verifies that an intent matching
// nothing produces neither a Captured entry nor a SkippedIntents
// entry — silence is the unambiguous "this intent didn't fire."
func TestTar_CaptureIntent_NoMatch(t *testing.T) {
	t.Parallel()
	b := newTarGz(t).addFile("a.txt", []byte("a")).addFile("b.txt", []byte("b"))

	intent := CaptureIntent{
		Name:    "nope",
		Match:   func(e Entry) bool { return false },
		MaxSize: 1024,
	}

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip,
		[]CaptureIntent{intent}, Limits{})

	require.NoError(t, err)
	assert.NotContains(t, manifest.Captured, intent.Name)
	assert.NotContains(t, manifest.SkippedIntents, intent.Name,
		"a never-matched intent must produce no SkippedIntents entry; "+
			"otherwise consumers can't tell 'matched-but-skipped' from 'never-matched'")
}

// ---------------------------------------------------------------------
// Manifest correctness: top-dir detection, sha256, totals.
// ---------------------------------------------------------------------

// TestTar_StripTopDirDetected verifies the auto-detected wrapping
// directory is reported, without rewriting Entry.Path values.
func TestTar_StripTopDirDetected(t *testing.T) {
	t.Parallel()
	b := newTarGz(t).
		addFile("package/index.js", []byte("a")).
		addFile("package/README.md", []byte("b")).
		addFile("package/src/lib.js", []byte("c"))

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})

	require.NoError(t, err)
	assert.Equal(t, "package/", manifest.StrippedTopDir,
		"common 'package/' prefix across all entries should be detected")
	for _, e := range manifest.Entries {
		assert.True(t, strings.HasPrefix(e.Path, "package/"),
			"Entry.Path must remain verbatim — only the StrippedTopDir "+
				"metadata reflects the detected prefix; got %q", e.Path)
	}
}

// TestTar_StripTopDir_NoCommonPrefix verifies StrippedTopDir is
// empty when entries don't share a single top-level directory.
func TestTar_StripTopDir_NoCommonPrefix(t *testing.T) {
	t.Parallel()
	b := newTarGz(t).
		addFile("a/file.txt", nil).
		addFile("b/file.txt", nil).
		addFile("README.md", nil)

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})

	require.NoError(t, err)
	assert.Empty(t, manifest.StrippedTopDir,
		"with no common single top-level directory, StrippedTopDir "+
			"must be empty rather than guessing")
}

// TestTar_ArchiveSHA256 verifies the manifest's ArchiveSHA256
// matches the sha256 of the bytes fed to Walk.
func TestTar_ArchiveSHA256(t *testing.T) {
	t.Parallel()
	b := newTarGz(t).addFile("a.txt", []byte("hello"))
	raw := b.bytes()

	want := sha256.Sum256(raw)
	wantHex := hex.EncodeToString(want[:])

	manifest, err := Walk(t.Context(), bytes.NewReader(raw), FormatTarGzip, nil, Limits{})
	require.NoError(t, err)
	assert.Equal(t, wantHex, manifest.ArchiveSHA256,
		"ArchiveSHA256 must equal the hex sha256 of the raw input bytes; "+
			"this is the hash consumers report alongside the divergence signal")
}

// TestTar_TotalUncompressedBytes verifies the manifest reports the
// sum of accepted entry sizes, useful for operator visibility into
// how close to MaxTotalBytes a walk got.
func TestTar_TotalUncompressedBytes(t *testing.T) {
	t.Parallel()
	body1 := bytes.Repeat([]byte("a"), 100)
	body2 := bytes.Repeat([]byte("b"), 200)

	b := newTarGz(t).
		addFile("a", body1).
		addFile("b", body2)

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})
	require.NoError(t, err)
	assert.EqualValues(t, 300, manifest.TotalUncompressedBytes,
		"TotalUncompressedBytes must sum entry body sizes")
}

// TestTar_DefaultLimitsApply verifies a Limits{} (zero value)
// resolves to DefaultLimits inside Walk so callers don't accidentally
// walk with a zero-cap and get either ErrLimitExceeded immediately
// or no protection at all.
func TestTar_DefaultLimitsApply(t *testing.T) {
	t.Parallel()
	b := newTarGz(t).addFile("a.txt", []byte("small"))

	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})
	require.NoError(t, err,
		"a small archive must succeed with zero-value Limits — defaults "+
			"must fall back to DefaultLimits, not 0/0/0/0")
	require.NotNil(t, manifest)
	assert.Len(t, manifest.Entries, 1)
}

// TestTar_EmptyArchive verifies a tarball with no entries returns a
// non-nil manifest with empty Entries.
func TestTar_EmptyArchive(t *testing.T) {
	t.Parallel()
	b := newTarGz(t)
	manifest, err := Walk(t.Context(), b.reader(), FormatTarGzip, nil, Limits{})
	require.NoError(t, err, "an empty archive is well-formed; walk must succeed")
	require.NotNil(t, manifest)
	assert.Empty(t, manifest.Entries)
}

// TestTar_ContextCancellation verifies that cancelling the context
// during a walk surfaces the cancellation error and the walk stops.
//
// Walks of legitimate archives are short, but malformed-but-passes-
// caps inputs could in principle stall the gunzip/tar libraries;
// honouring ctx.Done is the operator's escape hatch.
func TestTar_ContextCancellation(t *testing.T) {
	t.Parallel()
	b := newTarGz(t)
	for i := range 1000 {
		b.addFile("e/"+strings.Repeat("x", i+1), bytes.Repeat([]byte("y"), 1024))
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // pre-cancel; Walk must observe this immediately

	_, err := Walk(ctx, b.reader(), FormatTarGzip, nil,
		Limits{MaxTotalBytes: 100 << 20, MaxEntries: 10000, MaxEntryBytes: 1 << 20})

	require.Error(t, err, "Walk must honour context cancellation")
	assert.True(t, errors.Is(err, context.Canceled),
		"cancellation error must wrap context.Canceled so callers can "+
			"distinguish operator-initiated abort from cap or malformed errors")
}

// indexByPath is a small test helper that returns the manifest's
// entries keyed by Path. Asserts that every entry has a unique path
// — a duplicate would mask data and the test should fail loudly.
func indexByPath(entries []Entry) map[string]Entry {
	out := map[string]Entry{}
	for _, e := range entries {
		out[e.Path] = e
	}
	return out
}

// TestTar_FormatTarHeaderWalk pins the smallest claim of the
// gem-outer-wrapper model: stream.Walk with FormatTar reads tar
// headers from a byte stream that is NOT wrapped in gzip and
// produces the same Manifest shape FormatTarGzip produces.
//
// Header-only, no extraction. Same loop as FormatTarGzip, just
// without the gunzip step. The compression-ratio cap is meaningless
// for an uncompressed stream (1:1 by definition); MaxTotalBytes
// still applies as the resource ceiling.
//
// Why FormatTar exists: a `.gem` file's outer container is a plain
// tar (no gzip) holding `data.tar.gz`, `metadata.gz`, and
// `checksums.yaml.gz` as siblings. Walking the outer to identify
// data.tar.gz is the format dispatch this test verifies. Inner
// data.tar.gz then walks via FormatTarGzip in a second pass.
func TestTar_FormatTarHeaderWalk(t *testing.T) {
	t.Parallel()

	// Build a plain (uncompressed) tar with two entries.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "data.tar.gz",
		Size:     5,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
	}))
	_, err := tw.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "metadata.gz",
		Size:     5,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
	}))
	_, err = tw.Write([]byte("world"))
	require.NoError(t, err)
	require.NoError(t, tw.Close())

	manifest, err := Walk(t.Context(), bytes.NewReader(buf.Bytes()),
		FormatTar, nil, Limits{MaxTotalBytes: 1 << 20})
	require.NoError(t, err,
		"FormatTar must walk a plain (non-gzipped) tar without error — "+
			"this is the outer-wrapper format `.gem` files use")

	require.Len(t, manifest.Entries, 2)

	idx := indexByPath(manifest.Entries)
	assert.Contains(t, idx, "data.tar.gz",
		"the data.tar.gz sibling is the gem payload that the second-pass "+
			"walk will descend into")
	assert.Contains(t, idx, "metadata.gz")
	assert.Equal(t, EntryFile, idx["data.tar.gz"].Type)
	assert.Equal(t, int64(5), idx["data.tar.gz"].Size)

	// ArchiveSHA256 must still be populated. The whole-archive hash
	// is independent of compression — the stream walker tees through
	// sha256 in both Format paths.
	assert.Len(t, manifest.ArchiveSHA256, 64,
		"ArchiveSHA256 must be the hex-encoded sha256 of the input bytes "+
			"regardless of format")
}
