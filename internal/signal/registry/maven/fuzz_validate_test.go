package maven

import (
	"strings"
	"testing"
)

// --- Fuzz target for ValidateCoordinate ---
//
// ValidateCoordinate gates URL construction for Maven Central.
// GroupId segments become path components via dot-to-slash conversion
// (e.g., "org.apache.commons" → "org/apache/commons/"). ArtifactId
// is used directly. Any input passing validation is interpolated into
// https://repo1.maven.org/maven2/... URLs. A bypass means path
// injection against Maven Central.
//
// Maven grammar: starts with alphanumeric, body is [a-zA-Z0-9._-].

func FuzzValidateCoordinate(f *testing.F) {
	// Valid groupId segments
	f.Add("org")
	f.Add("org.apache.commons")
	f.Add("com.google.guava")
	f.Add("io.netty")
	f.Add("a")
	f.Add("A1")
	f.Add("my-artifact")
	f.Add("my_artifact")
	f.Add("my.artifact")
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
	f.Add("has:colon")
	// Adversarial: path traversal
	f.Add("..")
	f.Add(".")
	f.Add("..foo") // starts with dot-dot but has more chars
	// Adversarial: long input
	f.Add(strings.Repeat("a", 1000))
	// Adversarial: unicode
	f.Add("artïfact")

	f.Fuzz(func(t *testing.T, segment string) {
		err := ValidateCoordinate(segment)

		if err != nil {
			return
		}

		// --- Passed validation: verify URL-safety invariants ---

		// Invariant 1: no URL metacharacters (dots are allowed in Maven
		// coordinates and converted to path separators by the client).
		for _, r := range segment {
			if strings.ContainsRune("?#%@:/\\ \t\n\r", r) {
				t.Errorf("ValidateCoordinate accepted segment with URL metachar %q: %q", r, segment)
				return
			}
		}

		// Invariant 2: no ASCII control characters.
		for _, r := range segment {
			if r < 0x20 || r == 0x7f {
				t.Errorf("ValidateCoordinate accepted segment with control char U+%04X: %q", r, segment)
				return
			}
		}

		// Invariant 3: starts with alphanumeric.
		if len(segment) > 0 {
			first := rune(segment[0])
			if !isAlphanumericASCII(first) {
				t.Errorf("ValidateCoordinate accepted segment starting with %q: %q", first, segment)
			}
		}

		// Invariant 4: non-empty.
		if segment == "" {
			t.Error("ValidateCoordinate accepted empty string")
		}

		// Invariant 5: only ASCII characters.
		for _, r := range segment {
			if r > 127 {
				t.Errorf("ValidateCoordinate accepted non-ASCII rune U+%04X in %q", r, segment)
				return
			}
		}

		// Invariant 6: when dots are split into path segments, none
		// are traversal patterns. This is critical because the client
		// converts "org.apache.commons" → "org/apache/commons/" for
		// the URL path. If a segment is ".." after dot-splitting, it
		// would traverse.
		for _, part := range strings.Split(segment, ".") {
			if part == ".." {
				t.Errorf("ValidateCoordinate accepted segment with '..' after dot-split: %q", segment)
				return
			}
			// Empty part between dots (e.g., "a..b") — not traversal
			// but still interesting for URL construction.
		}
	})
}

func isAlphanumericASCII(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}
