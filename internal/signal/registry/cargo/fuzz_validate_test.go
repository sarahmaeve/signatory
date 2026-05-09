package cargo

import (
	"strings"
	"testing"
)

// --- Fuzz target for ValidateCrateName ---
//
// ValidateCrateName gates URL construction for crates.io API calls.
// Any input passing validation is interpolated into
// https://crates.io/api/v1/crates/<name> URLs. A bypass means path
// injection against the crates.io API.
//
// crates.io grammar: starts with a letter, body is [a-zA-Z0-9_-].
// Max 64 bytes.

func FuzzValidateCrateName(f *testing.F) {
	// Valid
	f.Add("serde")
	f.Add("tokio")
	f.Add("my-crate")
	f.Add("my_crate")
	f.Add("A")
	f.Add("a")
	f.Add("hyper-util")
	f.Add("serde_json")
	// Invalid: should reject
	f.Add("")
	f.Add("1starts-with-digit")
	f.Add("-starts-with-dash")
	f.Add("_starts-with-underscore")
	f.Add("has space")
	f.Add("has/slash")
	f.Add("has.dot")
	f.Add("has?query")
	f.Add("has#fragment")
	f.Add("has%encoded")
	f.Add("has\x00null")
	f.Add("has\nnewline")
	// Adversarial: at 64-byte boundary
	f.Add("a" + strings.Repeat("b", 63))
	// Adversarial: one byte over
	f.Add("a" + strings.Repeat("b", 64))
	// Adversarial: path traversal
	f.Add("..")
	f.Add(".")
	// Adversarial: unicode
	f.Add("cräte")

	f.Fuzz(func(t *testing.T, name string) {
		err := ValidateCrateName(name)

		if err != nil {
			return
		}

		// --- Passed validation: verify URL-safety invariants ---

		// Invariant 1: no URL metacharacters.
		for _, r := range name {
			if strings.ContainsRune("?#%@:/\\.+ \t\n\r", r) {
				t.Errorf("ValidateCrateName accepted name with URL metachar %q: %q", r, name)
				return
			}
		}

		// Invariant 2: no ASCII control characters.
		for _, r := range name {
			if r < 0x20 || r == 0x7f {
				t.Errorf("ValidateCrateName accepted name with control char U+%04X: %q", r, name)
				return
			}
		}

		// Invariant 3: starts with a letter.
		if len(name) > 0 {
			first := rune(name[0])
			isLower := first >= 'a' && first <= 'z'
			isUpper := first >= 'A' && first <= 'Z'
			if !isLower && !isUpper {
				t.Errorf("ValidateCrateName accepted name starting with non-letter %q: %q", first, name)
			}
		}

		// Invariant 4: non-empty.
		if name == "" {
			t.Error("ValidateCrateName accepted empty string")
		}

		// Invariant 5: within length cap.
		if len(name) > 64 {
			t.Errorf("ValidateCrateName accepted name over 64 bytes: len=%d", len(name))
		}

		// Invariant 6: only ASCII characters.
		for _, r := range name {
			if r > 127 {
				t.Errorf("ValidateCrateName accepted non-ASCII rune U+%04X in %q", r, name)
				return
			}
		}

		// Invariant 7: no dots (crates.io doesn't allow them).
		if strings.ContainsRune(name, '.') {
			t.Errorf("ValidateCrateName accepted name with dot: %q", name)
		}
	})
}
