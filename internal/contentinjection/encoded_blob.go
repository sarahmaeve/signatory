package contentinjection

import (
	"bytes"
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

	// encodedBlobWrappedThreshold caps the summed length across a
	// contiguous group of "pure base-N alphabet" lines for the
	// line-wrapped detector. Set above the largest legitimate
	// wrapped shape we expect to encounter:
	//
	//   - PGP signature (RSA-4096 worst case): ~688 base64 chars
	//     across ~11 lines of 64 chars each.
	//   - PGP-armored public key: typically ~700–1200 chars.
	//   - SSH armored key block: similar range.
	//
	// Set well above those (2048) to catch wrapped exfil — an
	// attacker who reads RFC 4880 and wraps their payload at 64
	// chars/line to slip past the single-line 1024-char threshold —
	// while keeping PGP false positives rare in practice. Wrapped
	// blobs smaller than 2048 chars (less than ~32 lines of 64) are
	// outside the detector's coverage by design; the analyst layer
	// is expected to handle smaller suspicious payloads via other
	// primitives or file-role weighting.
	encodedBlobWrappedThreshold = 2048

	// minWrappedLineLength is the per-line minimum for a line to
	// count as part of a wrapped run. Below this, a line is
	// classified as "not a wrapped-blob line" regardless of its
	// contents. Set at 40 chars — below standard PGP wrap (64) so
	// the last line of a block (which is often short) still
	// contributes, and above the length of a typical short hash
	// fragment in prose so a single "0123abcd" inline reference
	// doesn't seed a run.
	minWrappedLineLength = 40
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
//
// linePattern is the same alphabet without the length quantifier,
// anchored at line ends — used by the wrapped-blob detector to
// classify each line of the input. Trailing `=` padding is stripped
// by the caller before the match, so the pattern need not include
// it.
type blobScanner struct {
	encoding    string
	threshold   int
	pattern     *regexp.Regexp
	linePattern *regexp.Regexp
}

var encodedBlobScanners = []blobScanner{
	{encoding: "base64", threshold: encodedBlobBase64Threshold, pattern: encodedBlobBase64Pattern, linePattern: regexp.MustCompile(`^[A-Za-z0-9+/]+$`)},
	{encoding: "base64url", threshold: encodedBlobBase64Threshold, pattern: encodedBlobBase64URLPattern, linePattern: regexp.MustCompile(`^[A-Za-z0-9_\-]+$`)},
	{encoding: "hex-lower", threshold: encodedBlobHexThreshold, pattern: encodedBlobHexLowerPattern, linePattern: regexp.MustCompile(`^[0-9a-f]+$`)},
	{encoding: "hex-upper", threshold: encodedBlobHexThreshold, pattern: encodedBlobHexUpperPattern, linePattern: regexp.MustCompile(`^[0-9A-F]+$`)},
	{encoding: "base32", threshold: encodedBlobBase32Threshold, pattern: encodedBlobBase32Pattern, linePattern: regexp.MustCompile(`^[A-Z2-7]+$`)},
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
	scanWrappedEncodedBlob(content, &out)
	return out
}

// scanWrappedEncodedBlob extends scanEncodedBlob to detect base-N
// payloads that are line-wrapped — the evasion path against the
// single-line contiguous-run detector. PGP-armored output wraps at
// 64 chars/line by convention; an attacker who knows the
// single-line threshold (1024 chars) can wrap their payload at any
// width below that to slip past the existing regex.
//
// Algorithm: walk content line-by-line. For each line, after
// trimming trailing CR / horizontal whitespace / base64 padding,
// check it against each encoding's linePattern. Accumulate the
// per-encoding run length across consecutive matching lines; when
// a non-matching line breaks the run, flush — emit a finding if
// the accumulated total clears encodedBlobWrappedThreshold.
//
// Multiple encodings can have an open run in parallel for a given
// stream of pure-uppercase pure-digits lines (e.g. all-hex-upper
// lines are also valid base32 alphabet); each encoding that
// reaches the wrapped threshold contributes its own finding. The
// caller's Count therefore over-counts the underlying byte runs
// slightly when a wrapped block satisfies multiple alphabets, same
// as the single-line detector's documented behavior.
func scanWrappedEncodedBlob(content []byte, out *Finding) {
	runChars := make(map[string]int, len(encodedBlobScanners))
	runLines := make(map[string]int, len(encodedBlobScanners))

	flush := func(enc string) {
		// Require multi-line runs (lines >= 2). A single-line run
		// is the single-line detector's job; firing here too would
		// double-count the same payload under PrimitiveEncodedBlob.
		if runLines[enc] >= 2 && runChars[enc] >= encodedBlobWrappedThreshold {
			out.Count++
			if len(out.Details) < detailCap {
				out.Details = append(out.Details, fmt.Sprintf("%s-wrapped(%d)", enc, runChars[enc]))
			}
		}
		runChars[enc] = 0
		runLines[enc] = 0
	}

	for rawLine := range bytes.SplitSeq(content, []byte("\n")) {
		// Strip CR (Windows line endings), horizontal whitespace,
		// and trailing base64 `=` padding. The line pattern is the
		// alphabet without `=` so padding must come off before the
		// match. Leading whitespace is intentionally NOT stripped —
		// an indented or quoted block is more likely to be code or
		// quoted content than a raw exfil payload, and treating
		// indentation as a run-break biases toward fewer false
		// positives.
		line := bytes.TrimRight(rawLine, "\r \t=")
		for _, s := range encodedBlobScanners {
			if len(line) >= minWrappedLineLength && s.linePattern.Match(line) {
				runChars[s.encoding] += len(line)
				runLines[s.encoding]++
			} else {
				flush(s.encoding)
			}
		}
	}

	// Final flush — a run that extends to the last line of content
	// otherwise never gets evaluated.
	for _, s := range encodedBlobScanners {
		flush(s.encoding)
	}
}
