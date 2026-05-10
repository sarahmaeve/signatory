package cargo

import (
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// NormalizeDeclaredRepoURL converts a free-form repository URL — as
// publishers type it into Cargo.toml's `repository` field or as
// crates.io stores it — into a cloneable https URL. Returns empty
// string when the input doesn't resolve to a first-classed forge
// (github, codeberg, gitlab) or fails URL grammar validation.
//
// The logic mirrors pypi.NormalizeDeclaredRepoURL (and npm's equivalent
// via resolve.go) — the three copies are a candidate for extraction
// into a shared utility.
//
// Accepted shapes (github example; codeberg.org and gitlab.com
// resolve through the same shapes via profile.NormalizeForgeRepoInput):
//
//	"https://github.com/serde-rs/serde"
//	"https://github.com/serde-rs/serde.git"
//	"https://github.com/serde-rs/serde/"
//	"http://github.com/serde-rs/serde"          (upgraded → https)
//	"git+https://github.com/serde-rs/serde.git"
//	"git+ssh://git@github.com/serde-rs/serde"
//	"ssh://git@github.com/serde-rs/serde.git"
//	"https://codeberg.org/forgejo/forgejo"
//	"https://gitlab.com/gitlab-org/gitlab"
//
// Rejected → empty string:
//
//	""
//	"git://github.com/serde-rs/serde"          (insecure plaintext)
//	"https://bitbucket.org/team/repo"          (forge not first-classed)
//	"https://crates.io/crates/serde"           (self-link)
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

	// Delegate to ResolveTarget for forge-URL grammar validation.
	// Empty CloneURL is the canonical "platform not first-classed"
	// signal — profile.CloneURLForRepoPlatform stamps a non-empty
	// URL only for github / codeberg / gitlab.
	resolved, err := profile.ResolveTarget(s)
	if err != nil {
		return ""
	}
	if resolved.CloneURL == "" {
		return ""
	}
	return resolved.CloneURL
}
