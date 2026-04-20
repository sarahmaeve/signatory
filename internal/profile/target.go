package profile

import (
	"fmt"
	"strings"
)

// ResolvedTarget is the canonical form of a user-supplied target
// argument plus enough metadata for downstream commands to act on
// it without re-parsing.
//
// Every CLI command that accepts a <target> argument should run
// it through ResolveTarget once at the top of its Run method and
// use the fields of this struct thereafter. Mixing raw target
// strings with canonical URIs in the same code path is the source
// of the v0.1 dogfood's target-form inconsistency (see commit
// history and design/v0.1-invariants.md §"Invariant 4" on
// consistent transport forms).
type ResolvedTarget struct {
	// CanonicalURI is the scheme-prefixed identifier signatory
	// uses internally — the exact value stored as
	// entities.canonical_uri in SQLite. Examples:
	//   repo:github/nvbn/thefuck
	//   pkg:cargo/atuin
	//   identity:github/alecthomas
	CanonicalURI string

	// ShortName is the human-readable name derived from the URI's
	// last path segment. Used as TARGET_NAME in handoff template
	// substitution and as the filesystem-safe clone-directory
	// component.
	ShortName string

	// Scheme is the URI's scheme prefix without the trailing
	// colon ("repo", "pkg", "identity", "org", "patch").
	Scheme string

	// Platform is populated for repo:, identity:, org:, and
	// patch: URIs ("github", "gitlab", ...). Empty for pkg:.
	Platform string

	// Owner is populated for repo: and patch: URIs. Empty for
	// other schemes where the URI doesn't have an owner
	// component.
	Owner string

	// CloneURL is a https git-cloneable URL for repo: URIs on
	// supported platforms (currently GitHub). Empty for other
	// schemes or platforms. Callers that need to `git clone`
	// the target use this field without reconstructing the URL.
	CloneURL string

	// Ecosystem is populated for pkg: URIs ("npm", "pypi", "cargo",
	// "golang", ...). Empty for other schemes. Downstream dispatch
	// (ecosystem-specific collector routing, provider resolution)
	// reads this field instead of re-parsing the URI.
	Ecosystem string
}

// ResolveTarget normalizes a user-supplied target argument to its
// canonical form, accepting every form signatory's CLI surface
// encounters.
//
// Accepts:
//
//   - GitHub shorthand:   owner/repo
//   - GitHub hostname:    github.com/owner/repo
//   - GitHub URL:         https://github.com/owner/repo
//   - GitHub .git suffix: https://github.com/owner/repo.git
//   - SSH form:           git@github.com:owner/repo.git
//   - Canonical repo:     repo:github/owner/repo
//   - Canonical pkg:      pkg:cargo/atuin (and other ecosystems)
//   - Canonical identity: identity:github/alecthomas
//   - Canonical org:      org:github/stretchr
//   - Canonical patch:    patch:github/owner/repo/42
//
// Returns an error for empty input, malformed canonical URIs, or
// non-URI strings that don't parse as a GitHub shorthand.
//
// Design principle: this helper is the single source of truth for
// CLI target acceptance. If it accepts a form, every signatory
// command accepts that form; if it rejects, every command rejects
// with the same message. Drift between commands is the bug this
// function exists to prevent.
func ResolveTarget(raw string) (*ResolvedTarget, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil, fmt.Errorf("empty target")
	}

	// Already-canonical case: the input starts with one of our
	// known schemes. Validate the whole URI and extract the
	// per-scheme metadata.
	for _, prefix := range validURISchemes {
		if strings.HasPrefix(s, prefix) {
			return resolveCanonicalURI(s)
		}
	}

	// npmjs.com package URLs: convenience form for the
	// copy-paste-from-browser workflow. A user hitting
	// https://www.npmjs.com/package/<name> and running
	// `signatory analyze <url>` should not have to know about
	// purl syntax. Recognize the npmjs.com form explicitly and
	// convert to pkg:npm/<name>.
	if npmName, ok := parseNpmjsURL(s); ok {
		return resolveCanonicalURI("pkg:npm/" + npmName)
	}

	// Guard against non-github URLs sneaking through
	// NormalizeGitHubRepoInput's prefix-strip pipeline.
	//
	// NormalizeGitHubRepoInput trims `https://`/`http://`
	// unconditionally and only recognizes `github.com/` as a host
	// strip; `https://gitlab.com/foo/bar` would otherwise split to
	// owner=`gitlab.com`, name=`foo` and produce the misleading
	// canonical URI `repo:github/gitlab.com/foo`. Reject URL-scheme
	// inputs that aren't github (or a known-ecosystem host handled
	// above) so callers with gitlab / bitbucket / self-hosted URLs
	// get a clear "not yet supported" error instead of a silently-
	// wrong canonical form.
	if strings.Contains(s, "://") && !isGitHubURL(s) {
		return nil, fmt.Errorf(
			"target %q is a URL but not a github.com or npmjs.com URL; other hosting platforms are not yet supported by signatory",
			raw)
	}
	// SCP-form (git@host:owner/repo.git) — NormalizeGitHubRepoInput
	// strips `git@` unconditionally, which would misclassify
	// `git@gitlab.com:foo/bar.git`. Gate the same way.
	if strings.HasPrefix(s, "git@") && !strings.HasPrefix(s, "git@github.com:") {
		return nil, fmt.Errorf(
			"target %q is an SCP-form URL but not a github.com host; other hosting platforms are not yet supported by signatory",
			raw)
	}

	// Fall through to GitHub-shorthand parsing. Any form
	// NormalizeGitHubRepoInput accepts gets promoted to a
	// canonical repo: URI.
	canonicalURI, owner, name, err := NormalizeGitHubRepoInput(s)
	if err != nil {
		return nil, fmt.Errorf(
			"target %q is not a canonical URI (repo:/pkg:/identity:/org:/patch:) and does not parse as a github repo reference: %w",
			raw, err)
	}
	return &ResolvedTarget{
		CanonicalURI: canonicalURI,
		ShortName:    name,
		Scheme:       "repo",
		Platform:     PlatformGitHub,
		Owner:        owner,
		CloneURL:     "https://github.com/" + owner + "/" + name,
	}, nil
}

// parseNpmjsURL recognizes npmjs.com package URLs and extracts the
// package name. Returns (name, true) on a match; (_, false) on
// anything that isn't a well-formed npmjs.com/package/<name> URL.
//
// Accepted shapes:
//
//	https://www.npmjs.com/package/express
//	https://npmjs.com/package/express
//	http://www.npmjs.com/package/express
//	https://www.npmjs.com/package/@types/node        (scoped)
//	https://www.npmjs.com/package/express/v/4.18.2   (version page — strip /v/)
//	https://www.npmjs.com/package/express?activeTab=versions (query — strip)
//	https://www.npmjs.com/package/express#readme     (fragment — strip)
//
// Host-anchoring: the check rejects lookalike hosts like
// `npmjs.com.attacker.com/package/x` by requiring an exact match on
// "npmjs.com/" after the optional www./scheme strip. Same trick
// isGitHubURL uses for github.com.
//
// Does NOT validate the extracted name against npm's grammar —
// that's the caller's job via ValidateCanonicalURI (for URI shape)
// and, downstream, the npm client's ValidatePackageName (for
// HTTP-URL safety).
func parseNpmjsURL(input string) (string, bool) {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")

	// Host anchoring: must be EXACTLY "npmjs.com/" at this point.
	rest, ok := strings.CutPrefix(s, "npmjs.com/")
	if !ok {
		return "", false
	}

	// Path must start with "package/".
	rest, ok = strings.CutPrefix(rest, "package/")
	if !ok || rest == "" {
		return "", false
	}

	// Drop fragment and query — the npmjs.com UI adds these for
	// tabs, version pickers, etc., and they're not part of the
	// package identifier.
	if before, _, ok := strings.Cut(rest, "#"); ok {
		rest = before
	}
	if before, _, ok := strings.Cut(rest, "?"); ok {
		rest = before
	}

	// Scoped packages: "@scope/name" takes TWO path segments.
	// Everything after is version-page or other subpath noise.
	if strings.HasPrefix(rest, "@") {
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 || parts[0] == "@" || parts[1] == "" {
			return "", false
		}
		return parts[0] + "/" + parts[1], true
	}

	// Unscoped: one path segment is the name; trailing slash or
	// /v/<version> or /README etc. gets stripped.
	if idx := strings.Index(rest, "/"); idx >= 0 {
		rest = rest[:idx]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

// isGitHubURL returns true when input is an http(s) URL whose host
// (after scheme strip) starts with "github.com". Accepts forms like
// `https://github.com/owner/repo`, `http://github.com/...`, and the
// trailing-path variants NormalizeGitHubRepoInput handles. Does NOT
// do full URL parsing — that's heavier than this check needs — just
// scheme strip + host-segment prefix check.
func isGitHubURL(input string) bool {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	return strings.HasPrefix(s, "github.com/") || s == "github.com"
}

// resolveCanonicalURI handles an input already in canonical form
// (matched by one of the known scheme prefixes). Validates,
// decomposes into per-scheme fields, and populates CloneURL for
// supported repo: platforms.
func resolveCanonicalURI(uri string) (*ResolvedTarget, error) {
	if err := ValidateCanonicalURI(uri); err != nil {
		return nil, err
	}

	// Split on the first ':' to separate scheme from body. We
	// know the URI starts with one of validURISchemes, so the
	// colon exists and the scheme is well-known.
	colon := strings.IndexByte(uri, ':')
	scheme := uri[:colon]
	body := uri[colon+1:]
	parts := strings.Split(body, "/")

	// Every scheme needs at least one body segment. Empty body
	// would have been caught by ValidateCanonicalURI, but an
	// explicit check here makes the field-access below safe.
	if len(parts) == 0 || parts[0] == "" {
		return nil, fmt.Errorf("canonical URI %q has empty body after scheme", uri)
	}

	out := &ResolvedTarget{
		CanonicalURI: uri,
		Scheme:       scheme,
	}

	switch scheme {
	case "repo":
		// repo:<platform>/<owner>/<name>
		if len(parts) < 3 {
			return nil, fmt.Errorf("repo URI %q: expected platform/owner/name, got %d segment(s)", uri, len(parts))
		}
		out.Platform = parts[0]
		out.Owner = parts[1]
		out.ShortName = parts[len(parts)-1]
		if out.Platform == PlatformGitHub {
			out.CloneURL = "https://github.com/" + out.Owner + "/" + out.ShortName
		}
		// Other platforms (gitlab, bitbucket) intentionally leave
		// CloneURL empty until each is first-classed. Callers that
		// check for non-empty CloneURL before clone-dir actions
		// won't silently invoke `git clone` against a URL shape
		// the CLI hasn't validated.

	case "pkg":
		// pkg:<ecosystem>/<name...> where name may contain further
		// slashes (npm scoped packages: pkg:npm/@types/node).
		// ShortName is the final segment; Ecosystem is the first.
		if len(parts) < 2 {
			return nil, fmt.Errorf("pkg URI %q: expected ecosystem/name, got %d segment(s)", uri, len(parts))
		}
		out.Ecosystem = parts[0]
		out.ShortName = parts[len(parts)-1]

	case "identity", "org":
		// identity:<platform>/<user> or org:<platform>/<org>
		if len(parts) < 2 {
			return nil, fmt.Errorf("%s URI %q: expected platform/name, got %d segment(s)", scheme, uri, len(parts))
		}
		out.Platform = parts[0]
		out.ShortName = parts[len(parts)-1]

	case "patch":
		// patch:<platform>/<owner>/<repo>/<id>
		if len(parts) < 4 {
			return nil, fmt.Errorf("patch URI %q: expected platform/owner/repo/id, got %d segment(s)", uri, len(parts))
		}
		out.Platform = parts[0]
		out.Owner = parts[1]
		// ShortName renders as "repo#id" for human display —
		// preserves both the repo and the patch number in one
		// token without requiring callers to hand-compose.
		out.ShortName = parts[2] + "#" + parts[3]

	default:
		// Scheme is in validURISchemes but this switch doesn't
		// know it. That means validURISchemes grew without a
		// matching case here — loud failure is better than a
		// silent degradation to a default ShortName.
		return nil, fmt.Errorf("canonical URI %q uses scheme %q which ResolveTarget does not yet handle", uri, scheme)
	}

	return out, nil
}
