package profile

import "strings"

// NormalizeCrateName applies crates.io's name equivalence rule:
// hyphens and underscores are interchangeable for lookup purposes.
// The registry-canonical form uses hyphens (e.g., `serde-json`),
// so normalization replaces underscores with hyphens.
//
// Unlike PyPI's PEP 503, cargo normalization does NOT fold case and
// does NOT handle dots or repeated separators. It's the simplest of
// the three ecosystem normalizers.
//
// Idempotent: NormalizeCrateName(NormalizeCrateName(x)) == NormalizeCrateName(x).
func NormalizeCrateName(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}
