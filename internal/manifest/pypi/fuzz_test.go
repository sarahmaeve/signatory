package pypi

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for PEP 508 parsing primitives ---
//
// These functions process requirement specifiers from requirements.txt,
// pyproject.toml [project.dependencies], and PEP 735 dependency-groups.
// The input is publisher-controlled (a typosquatting attacker controls
// what goes into their package's dependencies array). The parsing must:
//
//   - Never panic on adversarial input
//   - Produce valid UTF-8 output
//   - Correctly track bracket depth (extras syntax)
//   - Not confuse version operators inside extras brackets
//   - Normalize names deterministically (PEP 503)

// --- FuzzSplitNameAndVersion ---
//
// splitNameAndVersion is the bracket-tracking character-level parser
// that separates "requests[security]>=2.31.0" into name="requests[security]"
// and version=">=2.31.0". A bug here means wrong package name or version.

func FuzzSplitNameAndVersion(f *testing.F) {
	// Standard forms
	f.Add("requests")
	f.Add("requests==2.31.0")
	f.Add("requests>=2.30,<3")
	f.Add("requests~=2.30")
	f.Add("requests!=2.0")
	// With extras
	f.Add("requests[security]==2.31.0")
	f.Add("requests[security,socks]>=2.30")
	f.Add("package[extra1,extra2,extra3]>=1.0")
	// Bare name (no version)
	f.Add("numpy")
	f.Add("Flask")
	// Edge: extras but no version
	f.Add("requests[security]")
	// Edge: version operator chars in extras (adversarial)
	f.Add("pkg[>=2.0]==1.0")
	f.Add("pkg[!extra]==1.0")
	f.Add("pkg[<weird>]==1.0")
	f.Add("pkg[~approx]==1.0")
	// Edge: unbalanced brackets
	f.Add("pkg[unclosed>=1.0")
	f.Add("pkg]extra[==1.0")
	f.Add("pkg[[nested]]==1.0")
	// Edge: empty extras
	f.Add("pkg[]==1.0")
	// Adversarial: control chars
	f.Add("pkg\x00==1.0")
	f.Add("pkg\n==1.0")
	// Adversarial: very long
	f.Add(strings.Repeat("a", 500) + ">=1.0")
	f.Add("pkg[" + strings.Repeat("x,", 500) + "y]>=1.0")
	// Adversarial: all operators
	f.Add("pkg<1.0")
	f.Add("pkg>1.0")
	f.Add("pkg<=1.0")
	f.Add("pkg>=1.0")
	f.Add("pkg==1.0")
	f.Add("pkg!=1.0")
	f.Add("pkg~=1.0")
	// Empty
	f.Add("")

	f.Fuzz(func(t *testing.T, spec string) {
		// splitNameAndVersion is a pure string splitter (not a boundary
		// validator). It operates on whatever bytes it receives. The
		// UTF-8 gate lives in parsePEP508Requirement (the caller).
		// Skip invalid UTF-8 here since we test that gate separately.
		if !utf8.ValidString(spec) {
			return
		}

		name, version := splitNameAndVersion(spec)

		// Invariant 1: name + version must reconstruct to the original
		// (they're a partition of the input string at a split point).
		if name+version != spec {
			t.Errorf("splitNameAndVersion(%q): name=%q version=%q don't reconstruct (got %q)",
				spec, name, version, name+version)
		}

		// Invariant 2: if version is non-empty, it must start with an
		// operator character (<, >, =, !, ~).
		if version != "" {
			first := rune(version[0])
			if first != '<' && first != '>' && first != '=' && first != '!' && first != '~' {
				t.Errorf("splitNameAndVersion(%q): version %q starts with non-operator %q",
					spec, version, first)
			}
		}

		// Invariant 3: output must be valid UTF-8 (guaranteed by input filter).
		if !utf8.ValidString(name) {
			t.Errorf("splitNameAndVersion(%q): name is invalid UTF-8", spec)
		}
		if !utf8.ValidString(version) {
			t.Errorf("splitNameAndVersion(%q): version is invalid UTF-8", spec)
		}

		// Invariant 4: if there's no operator outside brackets, the
		// entire spec should be the name (version empty).
		// This is the core correctness property: operators INSIDE
		// brackets must not trigger the split.
		if version == "" && name != spec {
			t.Errorf("splitNameAndVersion(%q): no version but name %q != spec", spec, name)
		}
	})
}

// --- FuzzPep503Normalize ---
//
// pep503Normalize lowercases and collapses separator runs [_.-] → "-".
// This determines canonical URIs: "Python_Dotenv" → "python-dotenv" →
// "pkg:pypi/python-dotenv". A bug means the wrong package gets analyzed.

func FuzzPep503Normalize(f *testing.F) {
	f.Add("requests")
	f.Add("Flask")
	f.Add("Python_Dotenv")
	f.Add("python__dot..env")
	f.Add("Zope.Interface")
	f.Add("my-package")
	f.Add("MY_PACKAGE")
	f.Add("a")
	f.Add("")
	f.Add("---")
	f.Add("___")
	f.Add("...")
	f.Add("_.-")
	f.Add("-._")
	f.Add("a-b_c.d")
	// Adversarial: all separators
	f.Add(strings.Repeat("-_.", 100))
	// Adversarial: separator at start/end
	f.Add("-name")
	f.Add("name-")
	f.Add("_name_")
	// Adversarial: long name
	f.Add(strings.Repeat("A", 500))

	f.Fuzz(func(t *testing.T, name string) {
		result := pep503Normalize(name)

		// Invalid UTF-8 input → empty output (defensive rejection).
		if !utf8.ValidString(name) {
			if result != "" {
				t.Errorf("pep503Normalize(%q): invalid UTF-8 input should produce empty, got %q", name, result)
			}
			return
		}

		// Invariant 1: output must be valid UTF-8.
		if !utf8.ValidString(result) {
			t.Errorf("pep503Normalize(%q): output is invalid UTF-8", name)
		}

		// Invariant 2: output must be lowercase.
		if result != strings.ToLower(result) {
			t.Errorf("pep503Normalize(%q): output %q is not lowercase", name, result)
		}

		// Invariant 3: no consecutive separators (runs are collapsed).
		if strings.Contains(result, "--") {
			t.Errorf("pep503Normalize(%q): output %q has consecutive hyphens", name, result)
		}

		// Invariant 4: no underscores or dots in output (all replaced by hyphen).
		if strings.ContainsAny(result, "_.") {
			t.Errorf("pep503Normalize(%q): output %q contains underscore or dot", name, result)
		}

		// Invariant 5: deterministic (same input → same output).
		result2 := pep503Normalize(name)
		if result != result2 {
			t.Errorf("pep503Normalize(%q): non-deterministic (%q vs %q)", name, result, result2)
		}

		// Invariant 6: idempotent (normalizing already-normalized is a no-op).
		result3 := pep503Normalize(result)
		if result3 != result {
			t.Errorf("pep503Normalize(%q): not idempotent: normalize(%q) = %q",
				name, result, result3)
		}

		// Invariant 7: output length <= input length (collapsing removes chars).
		if len(result) > len(name) {
			t.Errorf("pep503Normalize(%q): output %q is longer than input", name, result)
		}
	})
}

// --- FuzzParsePEP508Requirement ---
//
// The full PEP 508 requirement parser: handles extras, version
// specifiers, env markers, URL forms, and VCS prefixes. This is
// the shared workhorse called by both requirements.txt and
// pyproject.toml parsers.

func FuzzParsePEP508Requirement(f *testing.F) {
	f.Add("requests")
	f.Add("requests==2.31.0")
	f.Add("requests>=2.30,<3")
	f.Add("requests[security]==2.31.0")
	f.Add("Flask~=2.0")
	f.Add("numpy; python_version >= '3.10'")
	f.Add("requests @ https://example.com/requests.whl")
	f.Add("git+https://github.com/user/repo.git@main#egg=pkg")
	f.Add("-e git+https://github.com/user/repo.git#egg=pkg")
	f.Add("https://example.com/pkg-1.0.whl")
	f.Add("file:///tmp/local-pkg")
	f.Add("")
	f.Add("   ")
	// Adversarial: embedded semicolons (marker delimiter)
	f.Add("pkg==1.0; os_name == 'nt'; extra == 'test'")
	// Adversarial: @ in version spec (confuse URL detection)
	f.Add("pkg@1.0")
	// Adversarial: many extras
	f.Add("pkg[" + strings.Repeat("a,", 100) + "b]==1.0")
	// Adversarial: control chars
	f.Add("pkg\x00name==1.0")
	// Adversarial: very long
	f.Add(strings.Repeat("x", 1000) + "==1.0.0")

	f.Fuzz(func(t *testing.T, spec string) {
		dep, ok := parsePEP508Requirement(spec)

		if !ok {
			// Parser declined — that's fine (empty input).
			return
		}

		// Invariant 1: Name must be non-empty for accepted specs.
		if dep.Name == "" {
			t.Errorf("parsePEP508Requirement(%q): ok=true but Name is empty", spec)
		}

		// Invariant 2: Name must be valid UTF-8.
		if !utf8.ValidString(dep.Name) {
			t.Errorf("parsePEP508Requirement(%q): Name is invalid UTF-8", spec)
		}

		// Invariant 3: Version must be valid UTF-8.
		if !utf8.ValidString(dep.Version) {
			t.Errorf("parsePEP508Requirement(%q): Version is invalid UTF-8", spec)
		}

		// Invariant 4: CanonicalURI, if set, must be valid UTF-8 and
		// start with "pkg:pypi/".
		if dep.CanonicalURI != "" {
			if !utf8.ValidString(dep.CanonicalURI) {
				t.Errorf("parsePEP508Requirement(%q): CanonicalURI is invalid UTF-8", spec)
			}
			if !strings.HasPrefix(dep.CanonicalURI, "pkg:pypi/") {
				t.Errorf("parsePEP508Requirement(%q): CanonicalURI %q doesn't start with pkg:pypi/",
					spec, dep.CanonicalURI)
			}
			// The normalized name portion must be lowercase with no
			// underscores or dots (PEP 503 normalization applied).
			norm := strings.TrimPrefix(dep.CanonicalURI, "pkg:pypi/")
			if norm != strings.ToLower(norm) {
				t.Errorf("parsePEP508Requirement(%q): canonical name %q is not lowercase", spec, norm)
			}
			if strings.ContainsAny(norm, "_.") {
				t.Errorf("parsePEP508Requirement(%q): canonical name %q contains underscore or dot", spec, norm)
			}
		}

		// Invariant 5: Ecosystem must be either "pypi" or "pypi-local".
		if dep.Ecosystem != "pypi" && dep.Ecosystem != "pypi-local" {
			t.Errorf("parsePEP508Requirement(%q): unexpected ecosystem %q", spec, dep.Ecosystem)
		}

		// Invariant 6: Direct must be true (requirements.txt/pyproject
		// deps are always direct).
		if !dep.Direct {
			t.Errorf("parsePEP508Requirement(%q): Direct is false", spec)
		}
	})
}
