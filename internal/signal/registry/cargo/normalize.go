package cargo

import (
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// NormalizeDeclaredRepoURL converts a free-form repository URL — as
// publishers type it into Cargo.toml's `repository` field or as
// crates.io stores it — into a github-cloneable https URL. Returns
// empty string when the input doesn't resolve to a github repo.
//
// The logic mirrors pypi.NormalizeDeclaredRepoURL (and npm's equivalent
// via resolve.go) — the three copies are a candidate for extraction
// into a shared utility (see design/rust.md open question #1).
//
// Accepted shapes:
//
//	"https://github.com/serde-rs/serde"
//	"https://github.com/serde-rs/serde.git"
//	"https://github.com/serde-rs/serde/"
//	"http://github.com/serde-rs/serde"          (upgraded → https)
//	"git+https://github.com/serde-rs/serde.git"
//	"git+ssh://git@github.com/serde-rs/serde"
//	"ssh://git@github.com/serde-rs/serde.git"
//
// Rejected → empty string:
//
//	""
//	"git://github.com/serde-rs/serde"   (insecure plaintext)
//	"https://gitlab.com/foo/bar"        (non-github, v0.1)
//	"https://crates.io/crates/serde"    (self-link)
func NormalizeDeclaredRepoURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// Strip "git+" prefix (common in Rust ecosystem URLs).
	s = strings.TrimPrefix(s, "git+")

	// git:// is unauthenticated plaintext — refuse.
	if strings.HasPrefix(s, "git://") {
		return ""
	}

	// Drop URL fragments.
	if before, _, ok := strings.Cut(s, "#"); ok {
		s = before
	}

	// Convert ssh:// forms to https.
	if rest, ok := strings.CutPrefix(s, "ssh://git@github.com"); ok {
		s = "https://github.com" + rest
	}

	// Delegate to ResolveTarget for github-URL grammar validation.
	resolved, err := profile.ResolveTarget(s)
	if err != nil {
		return ""
	}
	if resolved.CloneURL == "" || resolved.Platform != profile.PlatformGitHub {
		return ""
	}
	return resolved.CloneURL
}
