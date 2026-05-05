package gopublish

import (
	"strings"
	"testing"
)

// --- Fuzz targets for ValidateModulePath / encodeModulePath / validateVersion ---
//
// These are the URL-safety gatekeepers: any input that passes
// validation is directly substituted into proxy.golang.org URLs.
// A bypass means SSRF or path traversal against the module proxy.
//
// encodeModulePath is the bang-encoding rule (uppercase → !lower).
// It must produce output that:
//   - Contains no uppercase letters (proxy is case-sensitive over bang-encoded names)
//   - Contains only URL-path-safe characters
//   - Round-trips correctly: decode(encode(x)) == x for valid paths

// --- FuzzValidateModulePath ---
//
// Proves that no input passing validation contains URL metacharacters
// that would re-parse the constructed proxy URL.

func FuzzValidateModulePath(f *testing.F) {
	// Valid paths
	f.Add("github.com/owner/repo")
	f.Add("golang.org/x/tools")
	f.Add("github.com/Azure/azure-sdk-for-go")
	f.Add("github.com/a/b")
	f.Add("go.uber.org/zap")
	f.Add("gopkg.in/yaml.v3")
	f.Add("github.com/foo/bar-baz_qux.v2")
	// Boundary: exactly 2 segments
	f.Add("a.com/b")
	// Invalid: should reject
	f.Add("")
	f.Add("single-segment")
	f.Add("/leading/slash")
	f.Add("trailing/slash/")
	f.Add("double//slash")
	f.Add("github.com/./traversal")
	f.Add("github.com/../traversal")
	f.Add("github.com/owner/repo?query=1")
	f.Add("github.com/owner/repo#fragment")
	f.Add("github.com/owner/repo%2f..%2f..")
	f.Add("github.com/owner\x00/repo")
	f.Add("github.com/owner\n/repo")
	f.Add("github.com/owner /repo")
	// Adversarial: very long path
	f.Add(strings.Repeat("a", 500) + ".com/" + strings.Repeat("b", 500))
	// Adversarial: many segments
	f.Add("a.com/" + strings.Repeat("x/", 100) + "y")
	// Boundary characters
	f.Add("github.com/owner~tilde/repo_underscore")
	f.Add("github.com/owner/repo.git")

	f.Fuzz(func(t *testing.T, path string) {
		err := ValidateModulePath(path)

		if err != nil {
			// Rejected — good. No further checks needed.
			return
		}

		// --- Passed validation: verify URL-safety invariants ---

		// Invariant 1: no URL metacharacters that would re-parse the URL.
		urlMetachars := "?#%@: \t\n\r"
		for _, r := range path {
			if strings.ContainsRune(urlMetachars, r) {
				t.Errorf("ValidateModulePath accepted path with URL metachar %q: %q", r, path)
				return
			}
		}

		// Invariant 2: no ASCII control characters.
		for _, r := range path {
			if r < 0x20 || r == 0x7f {
				t.Errorf("ValidateModulePath accepted path with control char U+%04X: %q", r, path)
				return
			}
		}

		// Invariant 3: no path traversal segments (. or ..).
		for _, seg := range strings.Split(path, "/") {
			if seg == "." || seg == ".." {
				t.Errorf("ValidateModulePath accepted traversal segment %q in %q", seg, path)
				return
			}
		}

		// Invariant 4: at least two path elements.
		if strings.Count(path, "/") < 1 {
			t.Errorf("ValidateModulePath accepted single-segment path: %q", path)
		}

		// Invariant 5: no empty segments (which would double-encode as //).
		if strings.Contains(path, "//") {
			t.Errorf("ValidateModulePath accepted path with '//': %q", path)
		}

		// Invariant 6: no leading/trailing slashes.
		if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
			t.Errorf("ValidateModulePath accepted path with leading/trailing '/': %q", path)
		}
	})
}

// --- FuzzEncodeModulePath ---
//
// Proves that encodeModulePath produces valid proxy-compatible output:
// no uppercase letters in output, no characters outside the proxy's
// accepted set, and that the encoding is deterministic.

func FuzzEncodeModulePath(f *testing.F) {
	f.Add("github.com/owner/repo")
	f.Add("github.com/Azure/azure-sdk-for-go")
	f.Add("github.com/AzureAD/microsoft-authentication-library-for-go")
	f.Add("github.com/ALLCAPS/REPO")
	f.Add("golang.org/x/tools")
	f.Add("a.com/b")
	// Edge: all uppercase
	f.Add("GITHUB.COM/OWNER/REPO")
	// Edge: already lowercase
	f.Add("github.com/lowercase/only")
	// Edge: alternating case
	f.Add("github.com/AbCdEf/GhIjKl")

	f.Fuzz(func(t *testing.T, path string) {
		// Only fuzz encoding on valid paths (encoding garbage is meaningless).
		if err := ValidateModulePath(path); err != nil {
			return
		}

		encoded := encodeModulePath(path)

		// Invariant 1: output must not contain uppercase ASCII letters.
		// The proxy's bang-encoding is designed to eliminate them.
		for i, r := range encoded {
			if r >= 'A' && r <= 'Z' {
				t.Errorf("encodeModulePath(%q) output has uppercase %q at byte %d: %q", path, r, i, encoded)
				return
			}
		}

		// Invariant 2: encoding is deterministic.
		encoded2 := encodeModulePath(path)
		if encoded != encoded2 {
			t.Errorf("encodeModulePath(%q) is non-deterministic: %q vs %q", path, encoded, encoded2)
		}

		// Invariant 3: output contains only URL-safe characters for a
		// path component (lowercase letters, digits, dots, hyphens,
		// underscores, tildes, forward slashes, and the bang prefix).
		for _, r := range encoded {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= '0' && r <= '9':
			case r == '.' || r == '-' || r == '_' || r == '~' || r == '/' || r == '!':
			default:
				t.Errorf("encodeModulePath(%q) output has unexpected char %q: %q", path, r, encoded)
				return
			}
		}

		// Invariant 4: output preserves path structure (same number of slashes).
		if strings.Count(encoded, "/") != strings.Count(path, "/") {
			t.Errorf("encodeModulePath(%q) changed slash count: input %d, output %d",
				path, strings.Count(path, "/"), strings.Count(encoded, "/"))
		}

		// Invariant 5: output is no shorter than input (bang-encoding adds chars).
		if len(encoded) < len(path) {
			t.Errorf("encodeModulePath(%q) produced shorter output: %d < %d", path, len(encoded), len(path))
		}
	})
}

// --- FuzzValidateVersion ---
//
// Proves that no version passing validation contains URL metacharacters.

func FuzzValidateVersion(f *testing.F) {
	f.Add("v1.2.3")
	f.Add("v0.0.0-20240101000000-abcdef123456")
	f.Add("v1.0.0-beta.1")
	f.Add("v2.0.0+incompatible")
	f.Add("v1.0.0-rc.1+meta")
	f.Add("")
	// Boundary: 128 bytes
	f.Add("v" + strings.Repeat("1", 127))
	// Over boundary
	f.Add("v" + strings.Repeat("1", 128))
	// Invalid chars
	f.Add("v1.0.0?query")
	f.Add("v1.0.0#fragment")
	f.Add("v1.0.0/path")
	f.Add("v1.0.0 space")
	f.Add("v1.0.0\x00null")
	f.Add("v1.0.0\nnewline")

	f.Fuzz(func(t *testing.T, v string) {
		err := validateVersion(v)

		if err != nil {
			return
		}

		// Invariant 1: no URL metacharacters.
		urlMetachars := "?#%@:/ \t\n\r"
		for _, r := range v {
			if strings.ContainsRune(urlMetachars, r) {
				t.Errorf("validateVersion accepted version with URL metachar %q: %q", r, v)
				return
			}
		}

		// Invariant 2: no control characters.
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				t.Errorf("validateVersion accepted version with control char U+%04X: %q", r, v)
				return
			}
		}

		// Invariant 3: non-empty (validation rejects empty).
		if v == "" {
			t.Error("validateVersion accepted empty string")
		}

		// Invariant 4: within length cap.
		if len(v) > 128 {
			t.Errorf("validateVersion accepted version over 128 bytes: len=%d", len(v))
		}
	})
}
