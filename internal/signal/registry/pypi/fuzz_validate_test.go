package pypi

import (
	"strings"
	"testing"
)

// --- Fuzz target for ValidatePackageName ---
//
// ValidatePackageName gates URL construction for PyPI's JSON API.
// Any input passing validation is interpolated into
// https://pypi.org/pypi/<name>/json URLs. A bypass means path
// injection against pypi.org.
//
// PEP 508 names: start+end alphanumeric, body is [A-Za-z0-9._-].
// Case-insensitive (PyPI normalizes), but we validate the raw form.

func FuzzValidatePackageName(f *testing.F) {
	// Valid
	f.Add("requests")
	f.Add("Flask")
	f.Add("django-rest-framework")
	f.Add("numpy")
	f.Add("my.package")
	f.Add("my_package")
	f.Add("A")
	f.Add("a1")
	f.Add("Zope.Interface")
	// Invalid: should reject
	f.Add("")
	f.Add("-starts-with-dash")
	f.Add(".starts-with-dot")
	f.Add("_starts-with-underscore")
	f.Add("ends-with-dash-")
	f.Add("ends-with-dot.")
	f.Add("ends-with-underscore_")
	f.Add("has space")
	f.Add("has/slash")
	f.Add("has?query")
	f.Add("has#fragment")
	f.Add("has%encoded")
	f.Add("has\x00null")
	f.Add("has\nnewline")
	f.Add("has@at")
	// Adversarial: at 100-byte boundary
	f.Add("a" + strings.Repeat("b", 98) + "c")
	// Adversarial: one byte over
	f.Add("a" + strings.Repeat("b", 99) + "c")
	// Adversarial: path traversal
	f.Add("../../../etc/passwd")
	f.Add("..") // exactly ".."
	f.Add(".")
	// Adversarial: unicode
	f.Add("päckage")
	f.Add("pαckage") // Greek alpha

	f.Fuzz(func(t *testing.T, name string) {
		err := ValidatePackageName(name)

		if err != nil {
			return
		}

		// --- Passed validation: verify URL-safety invariants ---

		// Invariant 1: no URL metacharacters.
		for _, r := range name {
			if strings.ContainsRune("?#%@:/\\ \t\n\r", r) {
				t.Errorf("ValidatePackageName accepted name with URL metachar %q: %q", r, name)
				return
			}
		}

		// Invariant 2: no ASCII control characters.
		for _, r := range name {
			if r < 0x20 || r == 0x7f {
				t.Errorf("ValidatePackageName accepted name with control char U+%04X: %q", r, name)
				return
			}
		}

		// Invariant 3: starts and ends with alphanumeric.
		if len(name) > 0 {
			first := rune(name[0])
			last := rune(name[len(name)-1])
			if !isAlphanumericASCII(first) {
				t.Errorf("ValidatePackageName accepted name starting with %q: %q", first, name)
			}
			if !isAlphanumericASCII(last) {
				t.Errorf("ValidatePackageName accepted name ending with %q: %q", last, name)
			}
		}

		// Invariant 4: non-empty.
		if name == "" {
			t.Error("ValidatePackageName accepted empty string")
		}

		// Invariant 5: within length cap.
		if len(name) > 100 {
			t.Errorf("ValidatePackageName accepted name over 100 bytes: len=%d", len(name))
		}

		// Invariant 6: only ASCII characters.
		for _, r := range name {
			if r > 127 {
				t.Errorf("ValidatePackageName accepted non-ASCII rune U+%04X in %q", r, name)
				return
			}
		}

		// Invariant 7: not a traversal pattern.
		if name == "." || name == ".." {
			t.Errorf("ValidatePackageName accepted traversal %q", name)
		}
	})
}

func isAlphanumericASCII(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}
