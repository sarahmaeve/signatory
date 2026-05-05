package npm

import (
	"strings"
	"testing"
)

// --- Fuzz target for ValidatePackageName ---
//
// ValidatePackageName gates URL construction: any input passing
// validation is directly interpolated into registry.npmjs.org URLs.
// A bypass means path/query/fragment injection against the registry.
//
// npm names come in two shapes:
//   - Unscoped: starts with alphanumeric, body is [A-Za-z0-9._-]
//   - Scoped: @scope/name with both parts following unscoped rules
//
// The fuzz test proves the regex + length check is airtight.

func FuzzValidatePackageName(f *testing.F) {
	// Valid unscoped
	f.Add("express")
	f.Add("lodash")
	f.Add("is-odd")
	f.Add("left-pad")
	f.Add("a")
	f.Add("a123")
	f.Add("my.package")
	f.Add("my_package")
	f.Add("my-package")
	// Valid scoped
	f.Add("@angular/core")
	f.Add("@types/node")
	f.Add("@babel/preset-env")
	f.Add("@a/b")
	// Invalid: should reject
	f.Add("")
	f.Add("@")
	f.Add("@/")
	f.Add("@scope/")
	f.Add("@/name")
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
	// Adversarial: at npm's 214-byte boundary
	f.Add("a" + strings.Repeat("b", 213))
	// Adversarial: one byte over
	f.Add("a" + strings.Repeat("b", 214))
	// Adversarial: scoped with path traversal
	f.Add("@../../../etc")
	f.Add("@scope/../../../etc")
	// Adversarial: unicode
	f.Add("päckage")
	f.Add("pαckage") // Greek alpha

	f.Fuzz(func(t *testing.T, name string) {
		err := ValidatePackageName(name)

		if err != nil {
			return
		}

		// --- Passed validation: verify URL-safety invariants ---

		// Invariant 1: no URL metacharacters (except @ and / in scoped names).
		for _, r := range name {
			if r == '@' || r == '/' {
				continue // allowed in scoped names
			}
			if strings.ContainsRune("?#%: \t\n\r\\", r) {
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

		// Invariant 3: no path traversal.
		// For scoped names, split on / and check both parts.
		parts := strings.SplitN(name, "/", 2)
		for _, p := range parts {
			clean := strings.TrimPrefix(p, "@")
			if clean == "." || clean == ".." {
				t.Errorf("ValidatePackageName accepted traversal in %q", name)
				return
			}
		}

		// Invariant 4: non-empty.
		if name == "" {
			t.Error("ValidatePackageName accepted empty string")
		}

		// Invariant 5: within length cap.
		if len(name) > 214 {
			t.Errorf("ValidatePackageName accepted name over 214 bytes: len=%d", len(name))
		}

		// Invariant 6: only ASCII characters (npm names are ASCII-only).
		for _, r := range name {
			if r > 127 {
				t.Errorf("ValidatePackageName accepted non-ASCII rune U+%04X in %q", r, name)
				return
			}
		}

		// Invariant 7: first char of each segment is alphanumeric.
		if strings.HasPrefix(name, "@") {
			// Scoped: @scope/name — scope starts after @, name starts after /
			inner := strings.TrimPrefix(name, "@")
			slashIdx := strings.IndexByte(inner, '/')
			if slashIdx > 0 {
				scopeStart := rune(inner[0])
				nameStart := rune(inner[slashIdx+1])
				if !isAlphanumericASCII(scopeStart) {
					t.Errorf("ValidatePackageName accepted scoped name where scope starts with %q: %q", scopeStart, name)
				}
				if !isAlphanumericASCII(nameStart) {
					t.Errorf("ValidatePackageName accepted scoped name where name starts with %q: %q", nameStart, name)
				}
			}
		} else {
			if len(name) > 0 && !isAlphanumericASCII(rune(name[0])) {
				t.Errorf("ValidatePackageName accepted unscoped name starting with %q: %q", rune(name[0]), name)
			}
		}
	})
}

func isAlphanumericASCII(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}
