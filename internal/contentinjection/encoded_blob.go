package contentinjection

import (
	"fmt"
	"regexp"
)

// Encoded-blob thresholds. Per design doc the primitive must
// "distinct from legitimate hashes and signatures by length
// distribution" — the thresholds are calibrated above the common
// legitimate-encoded artifacts and below the CamoLeak exfil-frame
// size class.
//
// Reference points the thresholds skip:
//
//   - SHA-256 hash: 64 hex chars
//   - SHA-512 hash: 128 hex chars
//   - Ed25519 signature: ~88 base64 chars
//   - RSA-2048 signature: ~344 base64 chars
//   - JWT signature: ~340 base64 chars
//   - PGP armored signature: wrapped at 64 chars/line; single line
//     rarely exceeds 512 chars even in compact form
//
// Reference points the thresholds catch:
//
//   - CamoLeak-shape exfil frame: 1024+ base64 chars
//   - Source-map embedded base64: typically multi-KB
//   - Inline-image data URLs: typically multi-KB (caller is expected
//     to handle these by file role — base64 inside a markdown image
//     URL is a different primitive than base64 inside a comment).
const (
	encodedBlobHexThreshold    = 512
	encodedBlobBase64Threshold = 1024
	encodedBlobBase32Threshold = 256
)

// Per-encoding patterns. Hex is intentionally restricted to a single
// case (lower OR upper) per run; mixing is rare in legitimate use
// and the single-case requirement keeps the regex tight.
//
// Go's regexp engine rejects quantifier counts above 1000 at compile
// time. The patterns below use a permissive {256,} minimum so they
// always compile; the per-scanner threshold field above does the
// authoritative length check, so a 257-char base64 run found by the
// regex is rejected at the length filter and does not fire.
var (
	encodedBlobHexLowerPattern  = regexp.MustCompile(`[0-9a-f]{256,}`)
	encodedBlobHexUpperPattern  = regexp.MustCompile(`[0-9A-F]{256,}`)
	encodedBlobBase64Pattern    = regexp.MustCompile(`[A-Za-z0-9+/]{256,}={0,2}`)
	encodedBlobBase64URLPattern = regexp.MustCompile(`[A-Za-z0-9_\-]{256,}={0,2}`)
	encodedBlobBase32Pattern    = regexp.MustCompile(`[A-Z2-7]{256,}={0,8}`)
)

// blobScanner pairs a regex with its encoding label and a minimum-
// length threshold the matched run must clear. The regex's quantifier
// already encodes the threshold; the field keeps the threshold
// visible to readers and to the Details renderer.
type blobScanner struct {
	encoding  string
	threshold int
	pattern   *regexp.Regexp
}

var encodedBlobScanners = []blobScanner{
	{encoding: "base64", threshold: encodedBlobBase64Threshold, pattern: encodedBlobBase64Pattern},
	{encoding: "base64url", threshold: encodedBlobBase64Threshold, pattern: encodedBlobBase64URLPattern},
	{encoding: "hex-lower", threshold: encodedBlobHexThreshold, pattern: encodedBlobHexLowerPattern},
	{encoding: "hex-upper", threshold: encodedBlobHexThreshold, pattern: encodedBlobHexUpperPattern},
	{encoding: "base32", threshold: encodedBlobBase32Threshold, pattern: encodedBlobBase32Pattern},
}

// scanEncodedBlob fires PrimitiveEncodedBlob for each contiguous
// run of base-N alphabet content above its encoding's length
// threshold. Each run contributes a sample of shape
// "<encoding>(<length>)" to Details — the length itself is the
// load-bearing evidence, not the blob content (which is almost
// certainly an opaque payload not worth signal-payload bytes).
//
// Multi-encoding overlap is possible (a base64 run is also a
// base64url run when no `+`/`/` chars appear). The aggregator
// reports each encoding independently. The Count therefore
// over-counts the underlying byte runs slightly when a blob
// satisfies multiple alphabets simultaneously — preferable to the
// alternative of picking one encoding and missing the others.
func scanEncodedBlob(content []byte) Finding {
	out := Finding{Primitive: PrimitiveEncodedBlob}
	for _, s := range encodedBlobScanners {
		for _, m := range s.pattern.FindAll(content, -1) {
			if len(m) < s.threshold {
				continue
			}
			out.Count++
			if len(out.Details) < detailCap {
				out.Details = append(out.Details, fmt.Sprintf("%s(%d)", s.encoding, len(m)))
			}
		}
	}
	return out
}
