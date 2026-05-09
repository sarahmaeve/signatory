package github

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz target for ParseRepoURL ---
//
// ParseRepoURL normalizes various GitHub URL formats into (owner, repo).
// It receives input from registry-declared repository URLs (npm, cargo,
// gem, PyPI project_urls, Maven SCM URLs) — all publisher-supplied and
// effectively untrusted. An attacker controlling a package's metadata
// can set the repository URL to anything.
//
// The function already validates via validGitHubName regex. The fuzz
// test proves the validation holds: no path traversal, no control chars,
// no NUL bytes, no unicode normalization confusion in the output.

func FuzzParseRepoURL(f *testing.F) {
	// Happy-path formats
	f.Add("owner/repo")
	f.Add("github.com/owner/repo")
	f.Add("https://github.com/owner/repo")
	f.Add("http://github.com/owner/repo")
	f.Add("https://github.com/owner/repo.git")
	f.Add("github.com/owner/repo/tree/main/subdir")
	f.Add("  owner/repo  ") // whitespace

	// Legitimate edge cases
	f.Add("a/b")              // shortest valid
	f.Add("org-name/repo.js") // dots and hyphens
	f.Add("O123/R456")        // numeric
	f.Add("a-b-c/d_e_f")      // mixed separators
	f.Add("UPPER/lower")      // case

	// Should-reject inputs
	f.Add("")
	f.Add("/")
	f.Add("owner")
	f.Add("owner/")
	f.Add("/repo")
	f.Add("../../../etc/passwd")
	f.Add("owner/../other")
	f.Add(".hidden/.secret")

	// Adversarial: injection attempts
	f.Add("owner/repo?query=1")
	f.Add("owner/repo#fragment")
	f.Add("owner/repo\x00injected")
	f.Add("owner\nrepo")
	f.Add("owner\x01/repo\x02")
	f.Add("https://github.com/owner/repo%2F..%2F..%2Fetc%2Fpasswd")
	f.Add("https://evil.com/github.com/owner/repo")
	f.Add("git@github.com:owner/repo.git")
	f.Add("ssh://git@github.com/owner/repo")

	// Adversarial: unicode normalization
	f.Add("öwner/repö")             // non-ASCII
	f.Add("ow\u200bner/re\u200bpo") // zero-width space
	f.Add("owner̀/repo")            // combining diacritical

	// Adversarial: very long inputs
	f.Add(strings.Repeat("a", 500) + "/" + strings.Repeat("b", 500))

	f.Fuzz(func(t *testing.T, input string) {
		owner, repo, err := ParseRepoURL(input)

		if err != nil {
			// Rejected — owner and repo should be empty.
			if owner != "" || repo != "" {
				t.Errorf("ParseRepoURL(%q) returned error but non-empty results: owner=%q repo=%q", input, owner, repo)
			}
			return
		}

		// Successful parse — verify invariants:

		// Invariant 1: owner and repo must be non-empty.
		if owner == "" || repo == "" {
			t.Errorf("ParseRepoURL(%q) returned nil error but empty owner=%q or repo=%q", input, owner, repo)
		}

		// Invariant 2: must be valid UTF-8.
		if !utf8.ValidString(owner) {
			t.Errorf("ParseRepoURL(%q): owner is invalid UTF-8: %q", input, owner)
		}
		if !utf8.ValidString(repo) {
			t.Errorf("ParseRepoURL(%q): repo is invalid UTF-8: %q", input, repo)
		}

		// Invariant 3: no control characters (NUL, newline, etc.)
		assertNoControlChars(t, "owner", owner)
		assertNoControlChars(t, "repo", repo)

		// Invariant 4: no path separators in owner or repo.
		if strings.ContainsAny(owner, "/\\") {
			t.Errorf("ParseRepoURL(%q): owner contains path separator: %q", input, owner)
		}
		if strings.ContainsAny(repo, "/\\") {
			t.Errorf("ParseRepoURL(%q): repo contains path separator: %q", input, repo)
		}

		// Invariant 5: no path traversal components.
		if owner == "." || owner == ".." || repo == "." || repo == ".." {
			t.Errorf("ParseRepoURL(%q): path traversal in output: owner=%q repo=%q", input, owner, repo)
		}

		// Invariant 6: no URL metacharacters that could enable
		// injection when the owner/repo are substituted into API URLs.
		if strings.ContainsAny(owner, "?#%@:") {
			t.Errorf("ParseRepoURL(%q): owner contains URL metachar: %q", input, owner)
		}
		if strings.ContainsAny(repo, "?#%@:") {
			t.Errorf("ParseRepoURL(%q): repo contains URL metachar: %q", input, repo)
		}

		// Invariant 7: only ASCII allowed (no unicode normalization
		// confusion). GitHub owner/repo names are ASCII-only.
		for _, r := range owner {
			if r > 127 {
				t.Errorf("ParseRepoURL(%q): owner contains non-ASCII rune U+%04X", input, r)
				break
			}
		}
		for _, r := range repo {
			if r > 127 {
				t.Errorf("ParseRepoURL(%q): repo contains non-ASCII rune U+%04X", input, r)
				break
			}
		}

		// Invariant 8: owner must start with alphanumeric (per GitHub rules).
		if len(owner) > 0 && !isAlphanumeric(rune(owner[0])) {
			t.Errorf("ParseRepoURL(%q): owner starts with non-alphanumeric: %q", input, owner)
		}
		if len(repo) > 0 && !isAlphanumeric(rune(repo[0])) {
			t.Errorf("ParseRepoURL(%q): repo starts with non-alphanumeric: %q", input, repo)
		}
	})
}

// --- Helpers ---

func isAlphanumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

func assertNoControlChars(t *testing.T, field, s string) {
	t.Helper()
	for i, r := range s {
		if r < 0x20 {
			t.Errorf("%s: control char U+%04X at byte %d in %q", field, r, i, s)
			return
		}
	}
}
