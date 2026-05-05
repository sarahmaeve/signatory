package gem

import (
	"strings"
	"testing"
)

// --- Fuzz target for ValidateGemName ---
//
// ValidateGemName gates URL construction for rubygems.org API calls.
// Any input passing validation is interpolated into
// https://rubygems.org/api/v1/gems/<name>.json URLs. A bypass means
// path injection against the rubygems.org API.
//
// rubygems.org grammar: starts with alphanumeric, body is [a-zA-Z0-9._-].
// Max 255 characters.

func FuzzValidateGemName(f *testing.F) {
	// Valid
	f.Add("rails")
	f.Add("nokogiri")
	f.Add("my-gem")
	f.Add("my_gem")
	f.Add("my.gem")
	f.Add("a")
	f.Add("A1")
	f.Add("rack-test")
	f.Add("activerecord")
	// Invalid: should reject
	f.Add("")
	f.Add("-starts-with-dash")
	f.Add(".starts-with-dot")
	f.Add("_starts-with-underscore")
	f.Add("has space")
	f.Add("has/slash")
	f.Add("has?query")
	f.Add("has#fragment")
	f.Add("has%encoded")
	f.Add("has\x00null")
	f.Add("has\nnewline")
	f.Add("has@at")
	// Adversarial: at 255-byte boundary
	f.Add("a" + strings.Repeat("b", 254))
	// Adversarial: one byte over
	f.Add("a" + strings.Repeat("b", 255))
	// Adversarial: path traversal
	f.Add("..")
	f.Add(".")
	// Adversarial: unicode
	f.Add("gëm")

	f.Fuzz(func(t *testing.T, name string) {
		err := ValidateGemName(name)

		if err != nil {
			return
		}

		// --- Passed validation: verify URL-safety invariants ---

		// Invariant 1: no URL metacharacters.
		for _, r := range name {
			if strings.ContainsRune("?#%@:/\\ \t\n\r", r) {
				t.Errorf("ValidateGemName accepted name with URL metachar %q: %q", r, name)
				return
			}
		}

		// Invariant 2: no ASCII control characters.
		for _, r := range name {
			if r < 0x20 || r == 0x7f {
				t.Errorf("ValidateGemName accepted name with control char U+%04X: %q", r, name)
				return
			}
		}

		// Invariant 3: starts with alphanumeric.
		if len(name) > 0 {
			first := rune(name[0])
			if !isAlphanumericASCII(first) {
				t.Errorf("ValidateGemName accepted name starting with %q: %q", first, name)
			}
		}

		// Invariant 4: non-empty.
		if name == "" {
			t.Error("ValidateGemName accepted empty string")
		}

		// Invariant 5: within length cap.
		if len(name) > 255 {
			t.Errorf("ValidateGemName accepted name over 255 bytes: len=%d", len(name))
		}

		// Invariant 6: only ASCII characters.
		for _, r := range name {
			if r > 127 {
				t.Errorf("ValidateGemName accepted non-ASCII rune U+%04X in %q", r, name)
				return
			}
		}
	})
}

func isAlphanumericASCII(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}
