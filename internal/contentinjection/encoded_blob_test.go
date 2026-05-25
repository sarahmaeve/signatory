package contentinjection

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScanEncodedBlob_Benign verifies that ordinary content with
// hashes, short signatures, embedded keys, and normal prose does
// not fire. The thresholds are tuned to skip everything in this
// list per design doc §"Distinct from legitimate hashes and
// signatures by length distribution."
func TestScanEncodedBlob_Benign(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"sha256_hash", "Checksum: 8d969eef6ecad3c29a3a629280e686cf0c3f5d5a86aff3ca12020c923adc6c92"},
		{"sha512_hash", "Hash: " + strings.Repeat("af", 64)}, // 128 hex chars
		{"short_base64_ed25519", "Sig: " + strings.Repeat("A", 88)},
		{"jwt_signature", "Token: " + strings.Repeat("aB1_", 85)}, // 340 chars, base64url alphabet, mixed
		{"prose_only", "Run the cargo build command to compile this crate."},
		{"random_short_letters", strings.Repeat("abcdef", 30)}, // 180 chars hex-shaped
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := scanEncodedBlob([]byte(tc.body))
			assert.Equal(t, 0, res.Count, "benign %q must not fire", tc.name)
		})
	}
}

// TestScanEncodedBlob_LongBase64 models the CamoLeak exfil shape:
// a long single-line base64 run carrying an opaque payload.
func TestScanEncodedBlob_LongBase64(t *testing.T) {
	t.Parallel()

	body := []byte("Embedded payload: " +
		strings.Repeat("ABCDabcd1234+/", encodedBlobBase64Threshold/14+1) +
		"==")
	res := scanEncodedBlob(body)
	assert.GreaterOrEqual(t, res.Count, 1, "long base64 run must fire")
	assert.NotEmpty(t, res.Details)
}

// TestScanEncodedBlob_LongHexLower models a hex-encoded payload.
// Must clear encodedBlobHexThreshold to fire.
func TestScanEncodedBlob_LongHexLower(t *testing.T) {
	t.Parallel()

	body := []byte("Payload: " + strings.Repeat("af", encodedBlobHexThreshold/2+1))
	res := scanEncodedBlob(body)
	assert.GreaterOrEqual(t, res.Count, 1, "long hex run must fire")
}

// TestScanEncodedBlob_LongHexUpper covers the upper-case hex
// alphabet — separate from lower because mixed-case in a single
// run is rare in legitimate use.
func TestScanEncodedBlob_LongHexUpper(t *testing.T) {
	t.Parallel()

	body := []byte("Payload: " + strings.Repeat("AF", encodedBlobHexThreshold/2+1))
	res := scanEncodedBlob(body)
	assert.GreaterOrEqual(t, res.Count, 1, "long upper-case hex run must fire")
}

// TestScanEncodedBlob_LongBase32 covers the base32 alphabet.
// Base32 has lower threshold than base64 because legitimate base32
// uses are rarer.
func TestScanEncodedBlob_LongBase32(t *testing.T) {
	t.Parallel()

	body := []byte("Payload: " +
		strings.Repeat("ABCDEFGH", encodedBlobBase32Threshold/8+1))
	res := scanEncodedBlob(body)
	assert.GreaterOrEqual(t, res.Count, 1, "long base32 run must fire")
}

// TestScanEncodedBlob_WrappedBase64Exfil covers the line-wrapping
// evasion. The single-line regex requires 256+ contiguous chars of
// base64 alphabet; an attacker who wraps the payload at 64 or 76
// chars per line evades that detector. A wrapped block above the
// wrapped threshold must still fire as PrimitiveEncodedBlob —
// closing the gap an attacker who reads RFC 4880 can otherwise
// exploit to trivially obfuscate a payload.
func TestScanEncodedBlob_WrappedBase64Exfil(t *testing.T) {
	t.Parallel()

	// 40 lines × 64 base64-alphabet chars = 2560 chars total — well
	// above the wrapped threshold so the assertion isn't sensitive
	// to off-by-one fixture errors. Each line is a real-looking
	// PGP-shape width (64 chars) to verify the line-grouping path,
	// not the contiguous regex path, is what catches this.
	const linesInFixture = 40
	const lineWidth = 64
	lines := make([]string, 0, linesInFixture)
	for range linesInFixture {
		lines = append(lines, strings.Repeat("A", lineWidth))
	}
	require.GreaterOrEqual(t, linesInFixture*lineWidth, encodedBlobWrappedThreshold,
		"test premise: fixture total must exceed the wrapped threshold")
	body := []byte("Embedded payload:\n" + strings.Join(lines, "\n") + "\n")

	res := scanEncodedBlob(body)
	assert.GreaterOrEqual(t, res.Count, 1,
		"wrapped base64 block totaling 2048 chars must fire as PrimitiveEncodedBlob — "+
			"the single-line regex misses this by design, the wrapped detector must catch it")
}

// TestScanEncodedBlob_SingleLineNotDoubleCounted verifies the
// wrapped detector does NOT fire on a single-line blob even when
// the line clears the wrapped threshold. The single-line detector
// is the canonical home for contiguous-run cases; the wrapped
// detector is strictly for multi-line shapes. Without the
// lineCount>=2 guard, a single 2048-char base64 line would
// produce two findings (one from each detector) under the same
// PrimitiveEncodedBlob — over-counting the underlying payload.
func TestScanEncodedBlob_SingleLineNotDoubleCounted(t *testing.T) {
	t.Parallel()

	body := []byte(strings.Repeat("A", encodedBlobWrappedThreshold+1))
	res := scanEncodedBlob(body)

	// At least one finding: the single-line detector picks it up.
	require.GreaterOrEqual(t, res.Count, 1, "single-line detector must fire")
	// But no Details entry should carry the "-wrapped" suffix —
	// the wrapped detector must skip single-line runs even when
	// they clear the wrapped threshold.
	for _, d := range res.Details {
		assert.NotContains(t, d, "-wrapped",
			"single-line blob must not produce a -wrapped finding (would double-count)")
	}
}

// TestScanEncodedBlob_PGPSignatureBenign verifies the wrapped
// detector skips a typical PGP-armored signature shape. Real PGP
// signatures wrap at 64 chars/line and total ~400 chars
// (RSA-2048 ≈ 6 lines, RSA-4096 ≈ 11 lines). The wrapped threshold
// must sit above the longest legitimate signature shape so PGP
// blocks remain benign — the same false-positive policy the
// single-line thresholds apply to bare RSA / Ed25519 / JWT
// signatures.
func TestScanEncodedBlob_PGPSignatureBenign(t *testing.T) {
	t.Parallel()

	// 6 lines × 64 chars = 384 chars total — well below the
	// wrapped threshold and below the single-line threshold too.
	// Real PGP-signature shape with BEGIN/END markers around it.
	body := []byte(`-----BEGIN PGP SIGNATURE-----

iQFGBAEBCAAwFiEEA0FzmoO2vGdcGdYSrSlqyB5xnpAFAmZD2lkSHGFwaUBleGFt
cGxlLmNvbQAKCRAA0FzmoO2vGdcGd4QQAJAa1Av9RB6Z3eHmAaZMZmcCJSf7BMOu
8x5pXJEhsKMjBPdQVqcUNFRkLM4l5qf5gjGmYUg2FjPRzWPgF6PEy3rcDFEYwBJL
fLeYqQQ7HxmmM5IvT4UQK9HOZpvCEvL6e5Z9j7L0Q3VhwQQ7vPmcCWlxRY5xQOgC
=DGmh
-----END PGP SIGNATURE-----`)

	res := scanEncodedBlob(body)
	assert.Equal(t, 0, res.Count,
		"PGP-armored signature (~400 chars across 4 wrapped lines) must not fire — "+
			"the wrapped threshold must clear typical PGP shapes")
}

// TestScanEncodedBlob_DetailFormat verifies the Details entries
// carry encoding name + run length, not the blob content.
func TestScanEncodedBlob_DetailFormat(t *testing.T) {
	t.Parallel()

	body := []byte(strings.Repeat("ABCDabcd1234+/", encodedBlobBase64Threshold/14+1) + "==")
	res := scanEncodedBlob(body)
	assert.NotEmpty(t, res.Details)
	for _, d := range res.Details {
		assert.Regexp(t, `^[a-z0-9-]+\(\d+\)$`, d,
			"Details entry %q must have encoding(length) shape", d)
	}
}
