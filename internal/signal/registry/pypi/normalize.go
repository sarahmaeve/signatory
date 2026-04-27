package pypi

import (
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// NormalizeDeclaredRepoURL converts a free-form repository URL — as
// publishers type it into pyproject.toml's [project.urls] map (or
// the equivalent Poetry table, or the legacy info.home_page field) —
// into a github-cloneable https URL. Returns empty string when the
// input doesn't resolve to a github repo, matching the
// "self-reported but not resolvable" signal the caller passes
// upstream as DeclaredSource{} (see internal/ecosystem/resolver).
//
// Accepted shapes (the union of "what humans paste" and "what
// PyPI publishers actually emit"):
//
//	"https://github.com/psf/requests"
//	"https://github.com/psf/requests.git"
//	"https://github.com/psf/requests/"
//	"http://github.com/psf/requests"          (downgraded → https)
//	"https://www.github.com/psf/requests"     (www stripped)
//	"https://github.com/psf/requests#main"    (fragment stripped)
//	"git+https://github.com/psf/requests.git"
//	"git+ssh://git@github.com/psf/requests.git"
//	"ssh://git@github.com/psf/requests.git"
//
// Rejected shapes → empty string:
//
//	""                                         (no declaration)
//	"git://github.com/psf/requests"            (insecure plaintext)
//	"https://gitlab.com/foo/bar"               (non-github, v0.1)
//	"https://requests.readthedocs.io/"         (Homepage that isn't a repo)
//	"https://pypi.org/project/requests/"       (self-link)
//	"not a url"                                (garbage)
//	"https://github.com/psf"                   (owner without repo)
//
// PyPI differs from npm in two ways the function reflects:
//
//   - No "github:owner/repo" shorthand exists in the PyPI ecosystem;
//     pyproject.toml URLs are full https/ssh strings.
//   - The "git@github.com:owner/repo.git" SCP form is rare in
//     project_urls (humans paste the https URL from the browser
//     address bar). It still parses correctly via profile.ResolveTarget,
//     so accept it implicitly without a dedicated branch.
//
// git:// is explicitly refused: an unauthenticated plaintext
// protocol that a downstream git-clone would have to upgrade or
// reject anyway. Returning empty keeps the protocol choice explicit
// at this layer.
//
// Delegates the github URL grammar (host + owner/repo split + .git
// stripping + http→https + www stripping) to profile.ResolveTarget,
// which is the single source of truth for "is this a valid github
// reference?" across signatory.
func NormalizeDeclaredRepoURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// Strip the "git+" prefix the python packaging ecosystem
	// inherits from pip's URL specifier grammar (PEP 440 direct
	// references). Strip first so the inner scheme check below
	// catches "git+git://" too.
	s = strings.TrimPrefix(s, "git+")

	// git:// is unauthenticated plaintext — refuse explicitly.
	// Preempts ResolveTarget seeing the git:// and misparsing it
	// as a hostname + path.
	if strings.HasPrefix(s, "git://") {
		return ""
	}

	// Drop URL fragments — typically "#main" or a commit hash for
	// a pinned ref, neither part of the repo identity.
	if before, _, ok := strings.Cut(s, "#"); ok {
		s = before
	}

	// Convert "ssh://git@github.com" forms to https — signatory
	// only clones over https. ResolveTarget doesn't recognize the
	// ssh scheme, so the rewrite has to happen here.
	if rest, ok := strings.CutPrefix(s, "ssh://git@github.com"); ok {
		s = "https://github.com" + rest
	}

	// Delegate to ResolveTarget for the actual github-URL grammar.
	// Non-github hosts and malformed inputs surface as errors that
	// we map to "return empty string."
	resolved, err := profile.ResolveTarget(s)
	if err != nil {
		return ""
	}
	if resolved.CloneURL == "" || resolved.Platform != profile.PlatformGitHub {
		return ""
	}
	return resolved.CloneURL
}
