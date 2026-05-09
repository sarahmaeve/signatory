package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWalk_DecompressionBombCap verifies that Walk refuses to keep
// reading once the gunzipped stream crosses MaxBytes.
//
// The threat: a tar.gz that's tiny on disk (a few KB) can decompress
// to gigabytes of output. Even though Walk never WRITES the body
// bytes to disk, it still streams through them to find the next
// header — the tar format interleaves headers with content. Without
// a cap, an attacker-controlled archive could pin the analyze
// process at 100% CPU and exhaust process memory chasing headers.
//
// The construction: a single tar entry whose declared size is large,
// padded with zero bytes that gzip compresses ~1000:1. The on-disk
// archive is a few KB; the decompressed stream is 10 MB. Cap is set
// to 1 MB. Walk must stop with ErrArchiveTooLarge before the entry
// finishes streaming — neither returning success nor consuming the
// full payload silently.
func TestWalk_DecompressionBombCap(t *testing.T) {
	// 10 MB of zeros — gzip's deflate compresses this to a few KB.
	const payloadSize int64 = 10 * 1024 * 1024
	const capBytes int64 = 1 * 1024 * 1024

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     "bomb.bin",
		Size:     payloadSize,
		Mode:     0o644,
		Typeflag: tar.TypeReg,
	}))
	// Stream the zero payload in chunks so we don't hold 10 MB in
	// one allocation. The bytes themselves are uninteresting; gzip
	// will collapse them.
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

	// Sanity-check the construction: on-disk archive is well under
	// the cap, but the decompressed stream is well over it. If gzip
	// ever stops collapsing zero runs this aggressively, this assert
	// surfaces the fixture problem before the cap test does.
	require.Less(t, int64(buf.Len()), capBytes,
		"fixture invariant: compressed bomb must fit under the cap on disk; "+
			"otherwise the test isn't exercising the decompression path")

	entries, err := Walk(&buf, WalkOptions{MaxBytes: capBytes})

	require.Error(t, err, "Walk must refuse a bomb that exceeds MaxBytes")
	assert.True(t, errors.Is(err, ErrArchiveTooLarge),
		"error must wrap ErrArchiveTooLarge sentinel so callers can "+
			"distinguish bomb-cap from generic IO failures; got: %v", err)
	assert.Contains(t, strings.ToLower(err.Error()), "exceeded",
		"error message should mention the cap was exceeded for operator diagnostics")

	// The header for "bomb.bin" was readable BEFORE the body cap
	// fired, so it's reasonable for Walk to have recorded that one
	// entry. What's NOT reasonable is to claim success — the error
	// is what guarantees the caller can't act on a partial walk.
	// We don't pin the entries-length here; the contract is "error
	// returned, no silent success."
	_ = entries
}

// TestWalk_PathTraversalEntryRecordedNotExtracted verifies that a
// tarball containing a "../etc/passwd"-style entry is RECORDED in
// Walk's output as data — path preserved verbatim, including the
// leading ".." components — and that no filesystem write is
// attempted (proven structurally: walker.go does not call
// os.OpenFile / os.Create / io.Copy on entry bodies).
//
// The point of header-only walking, per the package doc, is exactly
// this: tar-slip / zip-slip / symlink-escape become RECORDS rather
// than filesystem operations. An attacker who ships a malicious path
// in a tarball gets that path surfaced in the divergence signal as
// evidence; they don't get to write outside the (non-existent)
// extraction root.
//
// A walker that crashed, errored, or silently rewrote the path on
// encountering ".." would defeat both purposes: defenders need to
// SEE the suspicious path in the signal, not have it sanitized
// away upstream.
func TestWalk_PathTraversalEntryRecordedNotExtracted(t *testing.T) {
	const traversalPath = "../../etc/passwd"
	const benignPath = "src/main.go"

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	// One benign entry, one traversal entry, one symlink pointing
	// outside the (non-existent) root. All three should land in the
	// returned slice with their fields preserved verbatim.
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: benignPath, Size: 0, Mode: 0o644, Typeflag: tar.TypeReg,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: traversalPath, Size: 0, Mode: 0o644, Typeflag: tar.TypeReg,
	}))
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "link-to-passwd", Size: 0, Mode: 0o777,
		Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd",
	}))

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	entries, err := Walk(&buf, WalkOptions{MaxBytes: 1 << 20})

	require.NoError(t, err,
		"a tarball containing path-traversal entries is malformed FROM "+
			"a security stance, not from a tar-format stance — the walker "+
			"must surface them as data, not abort the walk")
	require.Len(t, entries, 3,
		"all three headers must be preserved; suppressing the dangerous "+
			"ones would hide the very evidence the divergence signal needs")

	// Index by path for assertion clarity.
	byPath := map[string]Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}

	assert.Contains(t, byPath, benignPath,
		"benign entry must be preserved verbatim")
	assert.Contains(t, byPath, traversalPath,
		"traversal entry's leading '..' must NOT be cleaned away — "+
			"the suspicious path is the signal")

	link, ok := byPath["link-to-passwd"]
	require.True(t, ok, "symlink entry must be recorded")
	assert.Equal(t, "../../etc/passwd", link.LinkTarget,
		"symlink target must be preserved verbatim; the walker is "+
			"recording evidence, not performing link resolution")
	assert.Equal(t, byte(tar.TypeSymlink), link.Type,
		"symlink typeflag must be preserved so downstream classifier "+
			"can flag this as a non-regular entry")
}
