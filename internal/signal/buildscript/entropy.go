package buildscript

import "math"

// High-entropy literal detection. Tuned conservatively so it fires on
// embedded payloads but not on the long-ish strings legitimately found
// in build scripts:
//
//   - minEntropyRunLen 200: ~150 bytes of payload. Checksums (sha256 =
//     64 hex) and ordinary identifiers/URLs fall well short.
//   - entropyThreshold 4.0 bits/char: filters repetitive/padded runs
//     (which have low entropy) while random-looking base64 (~6) passes.
//
// Only the base64 charset is considered — NOT bare hex — because long
// hex strings (checksums, hashes) are common and benign in build inputs.
const (
	minEntropyRunLen = 200
	entropyThreshold = 4.0
)

// isB64Char reports membership in the base64 charset (standard +
// url-safe). '=' is padding; '-'/'_' are url-safe variants.
func isB64Char(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '+', b == '/', b == '=', b == '-', b == '_':
		return true
	default:
		return false
	}
}

// hasHighEntropyRun reports whether line contains a contiguous
// base64-charset run of at least minEntropyRunLen chars whose Shannon
// entropy is at least entropyThreshold bits/char — the embedded-payload
// signature.
func hasHighEntropyRun(line string) bool {
	b := []byte(line)
	for i := 0; i < len(b); {
		if !isB64Char(b[i]) {
			i++
			continue
		}
		j := i
		for j < len(b) && isB64Char(b[j]) {
			j++
		}
		if j-i >= minEntropyRunLen && shannonEntropy(b[i:j]) >= entropyThreshold {
			return true
		}
		i = j
	}
	return false
}

// shannonEntropy returns the per-byte Shannon entropy of b in bits.
func shannonEntropy(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var counts [256]int
	for _, c := range b {
		counts[c]++
	}
	n := float64(len(b))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
