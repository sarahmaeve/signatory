package contentinjection

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
