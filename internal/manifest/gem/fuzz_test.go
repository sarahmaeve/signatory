package gem

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for Gemfile.lock parsing ---
//
// The Gemfile.lock parser uses an indent-based state machine to
// reconstruct the dependency graph. Indent levels (4-space = package,
// 6-space = sub-dependency) drive parent→child edge extraction.
// The input is an untrusted text file from a potentially attacker-
// controlled repository (typosquat, dependency confusion).
//
// A bug in the indent/state logic means:
//   - Wrong parent→child edges → wrong transitive deps analyzed
//   - Missed direct-dep markers → wrong classification
//   - Panics on adversarial indent patterns

// --- FuzzParseLockfileContent ---
//
// Tests the full lockfile parser that extracts specs, directs, and
// non-gem markers from the indent-structured text format.

func FuzzParseLockfileContent(f *testing.F) {
	// Minimal valid lockfile
	f.Add("GEM\n  remote: https://rubygems.org/\n  specs:\n    rails (7.1.3)\n\nDEPENDENCIES\n  rails\n")
	// Multiple gems with sub-deps
	f.Add("GEM\n  remote: https://rubygems.org/\n  specs:\n    actionpack (7.1.3)\n      activesupport (= 7.1.3)\n    activesupport (7.1.3)\n\nDEPENDENCIES\n  actionpack\n")
	// GIT section (non-gem source)
	f.Add("GIT\n  remote: https://github.com/user/repo.git\n  revision: abc123\n  specs:\n    my-gem (1.0.0)\n\nGEM\n  remote: https://rubygems.org/\n  specs:\n    rails (7.1.3)\n\nDEPENDENCIES\n  my-gem!\n  rails\n")
	// PATH section
	f.Add("PATH\n  remote: .\n  specs:\n    my-app (0.1.0)\n\nGEM\n  remote: https://rubygems.org/\n  specs:\n    rack (3.0.0)\n\nDEPENDENCIES\n  my-app!\n  rack\n")
	// PLATFORMS and BUNDLED WITH sections
	f.Add("GEM\n  remote: https://rubygems.org/\n  specs:\n    rake (13.0.0)\n\nPLATFORMS\n  ruby\n  x86_64-linux\n\nDEPENDENCIES\n  rake\n\nBUNDLED WITH\n   2.4.0\n")
	// Gem with version constraint in DEPENDENCIES
	f.Add("GEM\n  remote: https://rubygems.org/\n  specs:\n    devise (4.9.0)\n\nDEPENDENCIES\n  devise (~> 4.9)\n")
	// Empty
	f.Add("")
	// Only section headers
	f.Add("GEM\nDEPENDENCIES\n")
	// Adversarial: wrong indent levels
	f.Add("GEM\n  specs:\n  wrong_indent (1.0)\n")
	f.Add("GEM\n  specs:\n        deep_indent (1.0)\n")
	// Adversarial: no version parens
	f.Add("GEM\n  specs:\n    noversion\n")
	// Adversarial: many gems
	f.Add("GEM\n  specs:\n" + strings.Repeat("    gem-x (1.0)\n", 100) + "\nDEPENDENCIES\n" + strings.Repeat("  gem-x\n", 100))
	// Adversarial: control chars in names
	f.Add("GEM\n  specs:\n    evil\x00gem (1.0)\n\nDEPENDENCIES\n  evil\x00gem\n")
	// Adversarial: very long lines
	f.Add("GEM\n  specs:\n    " + strings.Repeat("a", 1000) + " (1.0)\n")
	// Adversarial: blank section with specs:
	f.Add("UNKNOWN\n  specs:\n    fake (1.0)\n")

	f.Fuzz(func(t *testing.T, content string) {
		specs, directs, nonGem := parseLockfileContent(content)

		// Invariant 1: all spec names must be valid UTF-8.
		for i, s := range specs {
			if !utf8.ValidString(s.name) {
				t.Errorf("specs[%d].name is invalid UTF-8: %q", i, s.name)
			}
			if !utf8.ValidString(s.version) {
				t.Errorf("specs[%d].version is invalid UTF-8: %q", i, s.version)
			}
		}

		// Invariant 2: all direct dep names must be valid UTF-8.
		for name := range directs {
			if !utf8.ValidString(name) {
				t.Errorf("directs key is invalid UTF-8: %q", name)
			}
		}

		// Invariant 3: nonGem names must be valid UTF-8.
		for name := range nonGem {
			if !utf8.ValidString(name) {
				t.Errorf("nonGem key is invalid UTF-8: %q", name)
			}
		}

		// Invariant 4: nonGem names must be a subset of spec names.
		specNames := make(map[string]bool, len(specs))
		for _, s := range specs {
			specNames[s.name] = true
		}
		for name := range nonGem {
			if !specNames[name] {
				t.Errorf("nonGem contains %q which is not in specs", name)
			}
		}

		// Invariant 5: spec names should not be empty.
		for i, s := range specs {
			if s.name == "" {
				t.Errorf("specs[%d] has empty name", i)
			}
		}
	})
}

// --- FuzzParseLockfileEdges ---
//
// Tests the edge-extraction state machine that builds the dependency
// graph from indent structure: 4-space = parent, 6-space = child.

func FuzzParseLockfileEdges(f *testing.F) {
	// Simple parent→child
	f.Add("GEM\n  specs:\n    rails (7.1.3)\n      actionpack (= 7.1.3)\n      activesupport (= 7.1.3)\n")
	// Multiple parents
	f.Add("GEM\n  specs:\n    actionpack (7.1.3)\n      rack (~> 3.0)\n    activesupport (7.1.3)\n      concurrent-ruby (~> 1.0)\n")
	// Parent with no children
	f.Add("GEM\n  specs:\n    rake (13.0.0)\n")
	// Empty
	f.Add("")
	// GIT section
	f.Add("GIT\n  remote: https://github.com/a/b.git\n  specs:\n    my-gem (1.0)\n      dep-a (~> 2.0)\n")
	// Adversarial: inconsistent indents
	f.Add("GEM\n  specs:\n    parent (1.0)\n     five_spaces (2.0)\n")
	f.Add("GEM\n  specs:\n    parent (1.0)\n       seven_spaces (2.0)\n")
	// Adversarial: child before any parent
	f.Add("GEM\n  specs:\n      orphan-child (~> 1.0)\n    parent (1.0)\n")
	// Adversarial: many edges
	f.Add("GEM\n  specs:\n    parent (1.0)\n" + strings.Repeat("      child (>= 1.0)\n", 100))

	f.Fuzz(func(t *testing.T, content string) {
		edges := parseLockfileEdges(content)

		// Invariant 1: all edge URIs must be valid UTF-8.
		for i, e := range edges {
			if !utf8.ValidString(e.Parent) {
				t.Errorf("edges[%d].Parent is invalid UTF-8: %q", i, e.Parent)
			}
			if !utf8.ValidString(e.Child) {
				t.Errorf("edges[%d].Child is invalid UTF-8: %q", i, e.Child)
			}
		}

		// Invariant 2: edge URIs should be non-empty.
		for i, e := range edges {
			if e.Parent == "" {
				t.Errorf("edges[%d].Parent is empty", i)
			}
			if e.Child == "" {
				t.Errorf("edges[%d].Child is empty", i)
			}
		}

		// Invariant 3: edge URIs should start with "pkg:gem/" (the
		// canonical form produced by profile.CanonicalPackageURI).
		for i, e := range edges {
			if e.Parent != "" && !strings.HasPrefix(e.Parent, "pkg:gem/") {
				t.Errorf("edges[%d].Parent %q doesn't start with pkg:gem/", i, e.Parent)
			}
			if e.Child != "" && !strings.HasPrefix(e.Child, "pkg:gem/") {
				t.Errorf("edges[%d].Child %q doesn't start with pkg:gem/", i, e.Child)
			}
		}

		// Invariant 4: no self-edges (a gem depending on itself is
		// nonsensical and would indicate a parser bug).
		for i, e := range edges {
			if e.Parent == e.Child {
				t.Errorf("edges[%d] is a self-edge: %q", i, e.Parent)
			}
		}
	})
}

// --- FuzzParseSpecLine ---
//
// parseSpecLine splits "rails (7.1.3)" → name="rails", version="7.1.3".
// Simple but sits on the hot path of the lockfile parser.

func FuzzParseSpecLine(f *testing.F) {
	f.Add("rails (7.1.3)")
	f.Add("actionpack (7.1.3)")
	f.Add("noversion")
	f.Add("gem-name (1.0.0.pre.beta)")
	f.Add("()")
	f.Add("name (")
	f.Add("name (1.0) extra stuff")
	f.Add("")
	f.Add(strings.Repeat("x", 500) + " (1.0)")
	f.Add("evil\x00name (1.0)")

	f.Fuzz(func(t *testing.T, s string) {
		name, version := parseSpecLine(s)

		// Invariant 1: outputs must be valid UTF-8.
		if !utf8.ValidString(name) {
			t.Errorf("parseSpecLine(%q): name is invalid UTF-8: %q", s, name)
		}
		if !utf8.ValidString(version) {
			t.Errorf("parseSpecLine(%q): version is invalid UTF-8: %q", s, version)
		}

		// Invariant 2: if input is non-empty and not just whitespace,
		// name should be non-empty — except when the name part before
		// "(" is itself empty (inputs like "()" or "  (1.0)"). We
		// do not assert on this case because the parser is permitted
		// to return ("", version) for such inputs; downstream callers
		// treat the empty name as "skip this entry".

		// Invariant 3: version should not contain unbalanced parens
		// from the parsing logic (the closing ")" should be stripped).
		if strings.Contains(version, ")") {
			t.Errorf("parseSpecLine(%q): version %q contains closing paren", s, version)
		}
	})
}
