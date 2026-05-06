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

	// Version is populated when the input carried a package version
	// — either as a `pkg:<eco>/<name>@<version>` suffix on a
	// canonical URI, or as the `/v/<version>` segment on an
	// npmjs.com package URL. Empty when no version was specified.
	//
	// Only pkg: URIs carry versions in v0.1. repo:, identity:, org:,
	// and patch: URIs ignore any `@` in their segments — content-hash
	// / SHA pinning is a v0.2+ topic (see design/agent-facing-
	// contract.md D7).
	//
	// When Version is non-empty, CanonicalURI includes the `@V`
	// suffix — the URI is the identity, and the version is
	// load-bearing for that identity. `pkg:npm/X@2.2.4` is a
	// distinct canonical URI from `pkg:npm/X`; they index to
	// different entity rows and can hold different postures/burns.
	Version string
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
	if npmName, npmVersion, ok := parseNpmjsURL(s); ok {
		uri := "pkg:npm/" + npmName
		if npmVersion != "" {
			uri += "@" + npmVersion
		}
		return resolveCanonicalURI(uri)
	}

	// pypi.org project URLs: same copy-paste-from-browser workflow
	// for Python packages. Names are PEP 503-normalized inside
	// parsePyPIURL before URI construction, so `/project/Requests/`
	// and `/project/requests/` both produce `pkg:pypi/requests`.
	// Unlike npm, which preserves case because the npm registry is
	// case-sensitive, PyPI's registry canonicalizes on lookup and
	// our canonical URI must match.
	if pypiName, pypiVersion, ok := parsePyPIURL(s); ok {
		uri := "pkg:pypi/" + pypiName
		if pypiVersion != "" {
			uri += "@" + pypiVersion
		}
		return resolveCanonicalURI(uri)
	}

	// crates.io crate URLs: copy-paste-from-browser for Rust crates.
	// https://crates.io/crates/serde → pkg:cargo/serde
	// https://crates.io/crates/serde/1.0.219 → pkg:cargo/serde@1.0.219
	if crateName, crateVersion, ok := parseCratesIOURL(s); ok {
		uri := "pkg:cargo/" + crateName
		if crateVersion != "" {
			uri += "@" + crateVersion
		}
		return resolveCanonicalURI(uri)
	}

	// rubygems.org gem URLs: copy-paste-from-browser for Ruby gems.
	// https://rubygems.org/gems/rails → pkg:gem/rails
	// https://rubygems.org/gems/rails/versions/7.1.3 → pkg:gem/rails@7.1.3
	if gemName, gemVersion, ok := parseRubyGemsURL(s); ok {
		uri := "pkg:gem/" + gemName
		if gemVersion != "" {
			uri += "@" + gemVersion
		}
		return resolveCanonicalURI(uri)
	}

	// Maven Central URLs: copy-paste-from-browser for Maven packages.
	// Three hosts share the same /artifact/<groupId>/<artifactId> path
	// structure: central.sonatype.com (current portal),
	// search.maven.org (legacy search), and mvnrepository.com (popular
	// third-party index).
	// https://central.sonatype.com/artifact/com.google.guava/guava
	//   → pkg:maven/com.google.guava/guava
	if mvnGroup, mvnArtifact, mvnVersion, ok := parseMavenCentralURL(s); ok {
		uri := "pkg:maven/" + mvnGroup + "/" + mvnArtifact
		if mvnVersion != "" {
			uri += "@" + mvnVersion
		}
		return resolveCanonicalURI(uri)
	}

	// pkg.go.dev module/package URLs: copy-paste-from-browser
	// convenience for Go modules. Output is the pkg:golang/<import-path>
	// canonical form per the purl spec — the same form an analyst
	// referencing the module by purl would produce, and the same form
	// the gomod parser emits for non-github paths. Github-hosted
	// modules get owner/repo case-folded and any subpackage segments
	// stripped via canonicalGoModuleURI.
	if importPath, goVersion, ok := parsePkgGoDevURL(s); ok {
		uri := canonicalGoModuleURI(importPath)
		if goVersion != "" {
			uri += "@" + goVersion
		}
		return resolveCanonicalURI(uri)
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
			"target %q is a URL but not a github.com, npmjs.com, pypi.org, crates.io, rubygems.org, or Maven Central URL; other hosting platforms are not yet supported by signatory",
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

	// Pre-strip @<version> from github shorthand inputs before
	// delegating to NormalizeGitHubRepoInput. Without this,
	// NormalizeGitHubRepoInput's path-segment validator rejects
	// `Y@v1.0.0` because `@` isn't in its allowed character set.
	// Doing the split here keeps NormalizeGitHubRepoInput strict
	// on the bare name while extending the user-facing grammar.
	//
	// The canonical URI we synthesize on the way out preserves the
	// @V suffix — mirrors the pkg: shape and lets storage code
	// route both through SplitURIVersion uniformly.
	bareInput := s
	requestedVersion := ""
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		// Skip the SCP-form `git@github.com:...` case — the @ there
		// is part of the SSH user-host syntax, not a version
		// separator. SCP-form was already gated above; here we re-
		// guard so we don't misread that @ as a version split.
		if !strings.HasPrefix(s, "git@") {
			candidate := s[at+1:]
			before := s[:at]
			// The version separator @ must come AFTER any /
			// — otherwise it's part of the host portion of a URL
			// (theoretical; URLs at this point have https:// stripped
			// upstream of NormalizeGitHubRepoInput, so a / before @
			// is the path-vs-version boundary).
			if strings.ContainsRune(before, '/') {
				if candidate == "" {
					return nil, fmt.Errorf("target %q: empty version after '@'", raw)
				}
				// Reject nested @: the path portion (everything
				// before the version separator) must not itself
				// contain another @. Catches inputs like
				// "owner/repo@v1.0@extra" where LastIndexByte
				// would otherwise accept "extra" as the version
				// and silently drop the middle @.
				if strings.ContainsRune(before, '@') {
					return nil, fmt.Errorf("target %q: nested separators not allowed (multiple '@' in input)", raw)
				}
				bareInput = before
				requestedVersion = candidate
			}
		}
	}

	// Discriminate Go vanity paths from github shorthand BEFORE falling
	// through to NormalizeGitHubRepoInput. validPathSegment accepts dots
	// in owner names, so without this branch "modernc.org/sqlite" would
	// satisfy NormalizeGitHubRepoInput as owner=modernc.org, name=sqlite
	// and produce repo:github/modernc.org/sqlite — the misclassification
	// dogfood entry 1 surfaced.
	//
	// Discriminator: first path segment contains a `.`. GitHub
	// usernames/orgs cannot contain `.` (per github.com's name rules —
	// alphanumeric and hyphens only), so a dot in the first segment is
	// unambiguous evidence of a vanity host. The "github.com/" prefix
	// case is intentionally NOT excluded here — NormalizeGitHubRepoInput
	// strips that prefix above any segment scanning, so by the time we
	// reach this branch any github.com/ prefix has already been removed,
	// leaving owner/repo where owner is bare.
	//
	// Output mirrors what the gomod parser produces for the same import
	// path (pkg:go/<full-path>), keeping ResolveTarget and the parser in
	// agreement on canonical form. Vanity-resolution to a github form
	// (e.g. golang.org/x/Y → repo:github/golang/Y) is a lookup-side
	// alternate handled elsewhere — see LookupEntity / AlternateURIs.
	if firstSeg, _, ok := strings.Cut(bareInput, "/"); ok && firstSeg != "" &&
		!strings.HasPrefix(bareInput, "github.com/") &&
		!strings.HasPrefix(bareInput, "git@") &&
		strings.ContainsRune(firstSeg, '.') {
		canonicalURI := canonicalGoModuleURI(bareInput)
		if requestedVersion != "" {
			canonicalURI += "@" + requestedVersion
		}
		// Delegate to resolveCanonicalURI so the ShortName / Ecosystem /
		// Version fields are set the same way they are for direct
		// pkg:golang/ inputs. Drift between the two paths is the bug class
		// this consolidation prevents.
		return resolveCanonicalURI(canonicalURI)
	}

	// Fall through to GitHub-shorthand parsing. Any form
	// NormalizeGitHubRepoInput accepts gets promoted to a
	// canonical repo: URI.
	canonicalURI, owner, name, err := NormalizeGitHubRepoInput(bareInput)
	if err != nil {
		return nil, fmt.Errorf(
			"target %q is not a canonical URI (repo:/pkg:/identity:/org:/patch:) and does not parse as a github repo reference: %w",
			raw, err)
	}
	if requestedVersion != "" {
		canonicalURI += "@" + requestedVersion
	}
	return &ResolvedTarget{
		CanonicalURI: canonicalURI,
		ShortName:    name,
		Scheme:       "repo",
		Platform:     PlatformGitHub,
		Owner:        owner,
		CloneURL:     "https://github.com/" + owner + "/" + name,
		Version:      requestedVersion,
	}, nil
}

// parseNpmjsURL recognizes npmjs.com package URLs and extracts the
// package name plus an optional version from `/v/<version>` paths.
// Returns (name, version, true) on a match; version is "" when the
// URL has no `/v/` segment. Returns ("", "", false) on anything that
// isn't a well-formed npmjs.com/package/<name> URL.
//
// Accepted shapes:
//
//	https://www.npmjs.com/package/express                    → ("express", "", true)
//	https://npmjs.com/package/express                        → ("express", "", true)
//	http://www.npmjs.com/package/express                     → ("express", "", true)
//	https://www.npmjs.com/package/@types/node                → ("@types/node", "", true)
//	https://www.npmjs.com/package/express/v/4.18.2           → ("express", "4.18.2", true)
//	https://www.npmjs.com/package/@types/node/v/20.0.0       → ("@types/node", "20.0.0", true)
//	https://www.npmjs.com/package/express?activeTab=versions → ("express", "", true)
//	https://www.npmjs.com/package/express#readme             → ("express", "", true)
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
func parseNpmjsURL(input string) (name, version string, ok bool) {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")

	// Host anchoring: must be EXACTLY "npmjs.com/" at this point.
	rest, hostOK := strings.CutPrefix(s, "npmjs.com/")
	if !hostOK {
		return "", "", false
	}

	// Path must start with "package/".
	rest, pathOK := strings.CutPrefix(rest, "package/")
	if !pathOK || rest == "" {
		return "", "", false
	}

	// Drop fragment and query — the npmjs.com UI adds these for
	// tabs, version pickers, etc., and they're not part of the
	// package identifier.
	if before, _, hadHash := strings.Cut(rest, "#"); hadHash {
		rest = before
	}
	if before, _, hadQuery := strings.Cut(rest, "?"); hadQuery {
		rest = before
	}

	// Scoped packages: "@scope/name" takes TWO path segments; any
	// subsequent /v/<version> is version-page, everything else is
	// noise we ignore.
	if strings.HasPrefix(rest, "@") {
		parts := strings.SplitN(rest, "/", 4)
		if len(parts) < 2 || parts[0] == "@" || parts[1] == "" {
			return "", "", false
		}
		scopedName := parts[0] + "/" + parts[1]
		// /v/<version> at parts[2]/parts[3]
		if len(parts) >= 4 && parts[2] == "v" && parts[3] != "" {
			// parts[3] may itself contain further path segments
			// (/v/1.0/README.md etc.); take only up to the next slash.
			ver := parts[3]
			if idx := strings.IndexByte(ver, '/'); idx >= 0 {
				ver = ver[:idx]
			}
			return scopedName, ver, true
		}
		return scopedName, "", true
	}

	// Unscoped: first path segment is the name; optional /v/<version>
	// follows; anything else (/README, trailing slash) is stripped.
	segs := strings.SplitN(rest, "/", 4)
	if segs[0] == "" {
		return "", "", false
	}
	nameOut := segs[0]
	if len(segs) >= 3 && segs[1] == "v" && segs[2] != "" {
		ver := segs[2]
		if idx := strings.IndexByte(ver, '/'); idx >= 0 {
			ver = ver[:idx]
		}
		return nameOut, ver, true
	}
	return nameOut, "", true
}

// parsePyPIURL recognizes pypi.org project URLs and extracts the
// package name (PEP 503-normalized) plus an optional version from
// `/project/<name>/<version>/` paths. Returns (name, version, true)
// on a match; version is "" when the URL has no version segment.
// Returns ("", "", false) on anything that isn't a well-formed
// pypi.org/project/<name> URL.
//
// Accepted shapes:
//
//	https://pypi.org/project/requests/               → ("requests", "", true)
//	https://pypi.org/project/requests                → ("requests", "", true)
//	https://www.pypi.org/project/requests/           → ("requests", "", true)
//	http://pypi.org/project/requests/                → ("requests", "", true)
//	https://pypi.org/project/Requests/               → ("requests", "", true)   (normalized)
//	https://pypi.org/project/python_dotenv/          → ("python-dotenv", "", true) (normalized)
//	https://pypi.org/project/requests/2.31.0/        → ("requests", "2.31.0", true)
//	https://pypi.org/project/Requests/2.31.0/        → ("requests", "2.31.0", true)
//	https://pypi.org/project/requests/?activeTab=x   → ("requests", "", true)
//	https://pypi.org/project/requests/#history       → ("requests", "", true)
//
// Host-anchoring: the check rejects lookalike hosts like
// `pypi.org.attacker.com/project/x` by requiring an exact match on
// "pypi.org/" after the optional www./scheme strip. Same trick
// parseNpmjsURL and isGitHubURL use.
//
// Unlike parseNpmjsURL, this function applies PEP 503 name
// normalization before returning — the caller sees the already-
// normalized form so there's no opportunity for accidental
// non-normalized storage. Version is returned verbatim; PEP 440
// version normalization is not attempted here.
//
// Does NOT validate the extracted name against PEP 508's name
// grammar — that's Layer 4's job via a future pypi.ValidatePackageName
// analogous to npm's. This function's contract is "did the URL shape
// match pypi.org/project/<something>" plus "normalize the <something>
// per PEP 503," not "is <something> a syntactically legal PyPI name."
func parsePyPIURL(input string) (name, version string, ok bool) {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")

	// Host anchoring: must be EXACTLY "pypi.org/" at this point.
	rest, hostOK := strings.CutPrefix(s, "pypi.org/")
	if !hostOK {
		return "", "", false
	}

	// Path must start with "project/".
	rest, pathOK := strings.CutPrefix(rest, "project/")
	if !pathOK || rest == "" {
		return "", "", false
	}

	// Drop fragment and query — PyPI's UI adds these for tabs,
	// history anchors, etc., and they're not part of the package
	// identifier.
	if before, _, hadHash := strings.Cut(rest, "#"); hadHash {
		rest = before
	}
	if before, _, hadQuery := strings.Cut(rest, "?"); hadQuery {
		rest = before
	}

	// PyPI URL shape: project/<name>[/<version>][/].
	// Split on / and trim trailing empty segments so "requests/"
	// and "requests/2.31.0/" both parse cleanly.
	segs := strings.Split(rest, "/")
	for len(segs) > 0 && segs[len(segs)-1] == "" {
		segs = segs[:len(segs)-1]
	}
	if len(segs) == 0 || segs[0] == "" {
		return "", "", false
	}

	rawName := segs[0]
	rawVersion := ""
	if len(segs) >= 2 && segs[1] != "" {
		rawVersion = segs[1]
	}
	// Any additional segments (segs[2:]) are tolerated and ignored.
	// PyPI's UI doesn't produce them for copy-paste cases, but a
	// permissive parse is cheaper than a false-reject on a
	// harmlessly-extra trailing element.

	return NormalizePyPIName(rawName), rawVersion, true
}

// parseCratesIOURL recognizes crates.io crate URLs and extracts the
// crate name plus an optional version. Returns (name, version, true)
// on a match; version is "" when the URL has no version segment.
// Returns ("", "", false) on anything that isn't a well-formed
// crates.io/crates/<name> URL.
//
// Accepted shapes:
//
//	https://crates.io/crates/serde                → ("serde", "", true)
//	https://crates.io/crates/serde/              → ("serde", "", true)
//	https://crates.io/crates/serde/1.0.219        → ("serde", "1.0.219", true)
//	http://crates.io/crates/serde                 → ("serde", "", true)
//	https://crates.io/crates/serde?foo=bar        → ("serde", "", true)
//	https://crates.io/crates/serde#readme         → ("serde", "", true)
//
// Host-anchoring: rejects lookalike hosts like `crates.io.evil.com`
// by requiring an exact match on "crates.io/" after scheme strip.
//
// Unlike npm/pypi, crate names don't need normalization — crates.io
// already canonicalizes (hyphen and underscore are treated as the same
// name, and the registry stores the original). The extracted name is
// returned verbatim.
func parseCratesIOURL(input string) (name, version string, ok bool) {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")

	// Host anchoring: must be EXACTLY "crates.io/" at this point.
	rest, hostOK := strings.CutPrefix(s, "crates.io/")
	if !hostOK {
		return "", "", false
	}

	// Path must start with "crates/".
	rest, pathOK := strings.CutPrefix(rest, "crates/")
	if !pathOK || rest == "" {
		return "", "", false
	}

	// Drop fragment and query.
	if before, _, hadHash := strings.Cut(rest, "#"); hadHash {
		rest = before
	}
	if before, _, hadQuery := strings.Cut(rest, "?"); hadQuery {
		rest = before
	}

	// crates.io URL shape: crates/<name>[/<version>][/].
	segs := strings.Split(rest, "/")
	for len(segs) > 0 && segs[len(segs)-1] == "" {
		segs = segs[:len(segs)-1]
	}
	if len(segs) == 0 || segs[0] == "" {
		return "", "", false
	}

	rawName := segs[0]
	rawVersion := ""
	if len(segs) >= 2 && segs[1] != "" {
		rawVersion = segs[1]
	}

	return rawName, rawVersion, true
}

// parseRubyGemsURL recognizes rubygems.org gem URLs and extracts the
// gem name plus an optional version from `/versions/<version>` paths.
// Returns (name, version, true) on a match; version is "" when the
// URL has no version segment. Returns ("", "", false) on anything that
// isn't a well-formed rubygems.org/gems/<name> URL.
//
// Accepted shapes:
//
//	https://rubygems.org/gems/rails                    → ("rails", "", true)
//	https://rubygems.org/gems/rails/                   → ("rails", "", true)
//	https://www.rubygems.org/gems/rails                → ("rails", "", true)
//	http://rubygems.org/gems/rails                     → ("rails", "", true)
//	https://rubygems.org/gems/rails/versions/7.1.3     → ("rails", "7.1.3", true)
//	https://rubygems.org/gems/rails/versions/7.1.3/    → ("rails", "7.1.3", true)
//	https://rubygems.org/gems/rails?locale=en          → ("rails", "", true)
//	https://rubygems.org/gems/rails#readme             → ("rails", "", true)
//
// Host-anchoring: rejects lookalike hosts like `rubygems.org.evil.com`
// by requiring an exact match on "rubygems.org/" after scheme strip.
//
// Gem names are returned verbatim (lowercase-only normalization is
// applied downstream in resolveCanonicalURI's NormalizeGemName call).
func parseRubyGemsURL(input string) (name, version string, ok bool) {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")

	// Host anchoring: must be EXACTLY "rubygems.org/" at this point.
	rest, hostOK := strings.CutPrefix(s, "rubygems.org/")
	if !hostOK {
		return "", "", false
	}

	// Path must start with "gems/".
	rest, pathOK := strings.CutPrefix(rest, "gems/")
	if !pathOK || rest == "" {
		return "", "", false
	}

	// Drop fragment and query.
	if before, _, hadHash := strings.Cut(rest, "#"); hadHash {
		rest = before
	}
	if before, _, hadQuery := strings.Cut(rest, "?"); hadQuery {
		rest = before
	}

	// rubygems.org URL shape: gems/<name>[/versions/<version>][/].
	segs := strings.Split(rest, "/")
	for len(segs) > 0 && segs[len(segs)-1] == "" {
		segs = segs[:len(segs)-1]
	}
	if len(segs) == 0 || segs[0] == "" {
		return "", "", false
	}

	rawName := segs[0]
	rawVersion := ""
	// Version segment follows /versions/ prefix on rubygems.org.
	if len(segs) >= 3 && segs[1] == "versions" && segs[2] != "" {
		rawVersion = segs[2]
	}

	return rawName, rawVersion, true
}

// parseMavenCentralURL recognizes Maven Central and related URLs and
// extracts the groupId, artifactId, and optional version. Three hosts
// share the same /artifact/<groupId>/<artifactId>[/<version>] pattern:
//
//   - central.sonatype.com — the current Maven Central portal
//   - search.maven.org     — the legacy search interface
//   - mvnrepository.com    — popular third-party Maven index
//
// Returns (groupID, artifactID, version, true) on a match; version is
// "" when the URL has no version segment. Returns ("", "", "", false)
// on anything that isn't a recognized Maven URL.
//
// Accepted shapes:
//
//	https://central.sonatype.com/artifact/com.google.guava/guava
//	https://central.sonatype.com/artifact/com.google.guava/guava/33.2.1-jre
//	https://search.maven.org/artifact/com.google.guava/guava/33.2.1-jre/jar
//	https://mvnrepository.com/artifact/com.google.guava/guava
//
// Host-anchoring: rejects lookalike hosts like
// `central.sonatype.com.evil.com` by requiring an exact match after
// scheme strip.
//
// Maven coordinates are case-sensitive — no normalization is applied.
// The fourth segment on search.maven.org URLs (packaging type like
// "jar") is stripped since it's not part of the coordinate identity.
func parseMavenCentralURL(input string) (groupID, artifactID, version string, ok bool) {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")

	// Host anchoring: try each known Maven host. We need the rest
	// (path after host) so we use CutPrefix for each.
	var rest string
	var hostOK bool
	for _, host := range []string{
		"central.sonatype.com/",
		"search.maven.org/",
		"mvnrepository.com/",
	} {
		if rest, hostOK = strings.CutPrefix(s, host); hostOK {
			break
		}
	}
	if !hostOK {
		return "", "", "", false
	}

	// Path must start with "artifact/".
	rest, pathOK := strings.CutPrefix(rest, "artifact/")
	if !pathOK || rest == "" {
		return "", "", "", false
	}

	// Drop fragment and query.
	if before, _, hadHash := strings.Cut(rest, "#"); hadHash {
		rest = before
	}
	if before, _, hadQuery := strings.Cut(rest, "?"); hadQuery {
		rest = before
	}

	// Split remaining path: <groupId>/<artifactId>[/<version>[/<packaging>]]
	segs := strings.Split(rest, "/")
	for len(segs) > 0 && segs[len(segs)-1] == "" {
		segs = segs[:len(segs)-1]
	}

	// Need at least groupId + artifactId (2 segments).
	if len(segs) < 2 || segs[0] == "" || segs[1] == "" {
		return "", "", "", false
	}

	group := segs[0]
	artifact := segs[1]
	ver := ""

	// Third segment is the version (if present).
	if len(segs) >= 3 && segs[2] != "" {
		ver = segs[2]
	}
	// Fourth segment (packaging type like "jar") is ignored.

	return group, artifact, ver, true
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
	scheme, body, _ := strings.Cut(uri, ":")
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
		// repo:<platform>/<owner>/<name>[@<version>]
		// Optional @<version> suffix on the LAST path segment names
		// a git ref (tag, branch, commit). Used by signatory
		// handoff to clone the named ref. Storage strips the suffix
		// (entity is at the unversioned URI; version goes on the
		// posture row) — see SplitURIVersion.
		if len(parts) < 3 {
			return nil, fmt.Errorf("repo URI %q: expected platform/owner/name, got %d segment(s)", uri, len(parts))
		}
		out.Platform = parts[0]
		out.Owner = parts[1]
		// Extract optional @<version> from the last segment using
		// the same shape as the pkg: case. Two `@` are not
		// permitted (nested separators are ambiguous).
		lastSeg := parts[len(parts)-1]
		if name, version, ok := strings.Cut(lastSeg, "@"); ok {
			if name == "" {
				return nil, fmt.Errorf("repo URI %q: empty name before '@version'", uri)
			}
			if version == "" {
				return nil, fmt.Errorf("repo URI %q: empty version after '@'", uri)
			}
			if strings.ContainsRune(version, '@') {
				return nil, fmt.Errorf("repo URI %q: version contains '@' (nested separators not allowed)", uri)
			}
			out.ShortName = name
			out.Version = version
		} else {
			out.ShortName = lastSeg
		}
		// Case fold platform/owner/name to the canonical form
		// CanonicalRepoURI produces. Without this, a caller passing
		// `repo:github/BurntSushi/toml` (already-canonical-shaped but
		// not yet case-folded) would survive validation unchanged
		// and then fragment against entities the constructor created
		// at the lowercased URI. The fold also normalizes
		// `repo:GITHUB/...` → `repo:github/...` so the PlatformGitHub
		// equality check below catches uppercase-platform inputs.
		out.Platform = strings.ToLower(out.Platform)
		out.Owner = strings.ToLower(out.Owner)
		out.ShortName = strings.ToLower(out.ShortName)
		out.CanonicalURI = CanonicalRepoURI(out.Platform, out.Owner, out.ShortName)
		if out.Version != "" {
			out.CanonicalURI += "@" + out.Version
		}
		if out.Platform == PlatformGitHub {
			out.CloneURL = "https://github.com/" + out.Owner + "/" + out.ShortName
		}
		// Other platforms (gitlab, bitbucket) intentionally leave
		// CloneURL empty until each is first-classed. Callers that
		// check for non-empty CloneURL before clone-dir actions
		// won't silently invoke `git clone` against a URL shape
		// the CLI hasn't validated.

	case "pkg":
		// pkg:<ecosystem>/<name...>[@<version>] where name may contain
		// further slashes (npm scoped packages: pkg:npm/@types/node)
		// and an optional `@<version>` suffix on the LAST segment
		// (pkg:npm/X@1.2.3, pkg:npm/@types/node@20.0.0).
		if len(parts) < 2 {
			return nil, fmt.Errorf("pkg URI %q: expected ecosystem/name, got %d segment(s)", uri, len(parts))
		}
		out.Ecosystem = parts[0]

		// Extract the optional @<version> suffix from the last
		// segment. The scope prefix on scoped npm names (@types) lives
		// in its OWN segment and never collides with the version @.
		lastIdx := len(parts) - 1
		lastSeg := parts[lastIdx]
		if name, version, ok := strings.Cut(lastSeg, "@"); ok {
			if name == "" {
				return nil, fmt.Errorf("pkg URI %q: empty name before '@version'", uri)
			}
			if version == "" {
				return nil, fmt.Errorf("pkg URI %q: empty version after '@'", uri)
			}
			if strings.ContainsRune(version, '@') {
				return nil, fmt.Errorf("pkg URI %q: version contains '@' (nested separators not allowed)", uri)
			}
			out.ShortName = name
			out.Version = version
		} else {
			out.ShortName = lastSeg
		}

		// PEP 503 name normalization for PyPI. Unlike npm (case-
		// sensitive) and Go (path-preserving), PyPI's registry
		// canonicalizes names on lookup — `Requests`, `requests`,
		// and `python_dotenv` all map to the same identity. The
		// canonical URI must match, or storage fragments across
		// identities the registry considers the same. See pypi.go
		// for the normalization spec. No-op when the extracted name
		// is already in canonical form.
		if out.Ecosystem == "pypi" {
			normalized := NormalizePyPIName(out.ShortName)
			if normalized != out.ShortName {
				out.ShortName = normalized
				out.CanonicalURI = "pkg:pypi/" + normalized
				if out.Version != "" {
					out.CanonicalURI += "@" + out.Version
				}
			}
		}

		// Cargo name normalization: crates.io treats hyphens and
		// underscores as equivalent for lookups — both `serde_json`
		// and `serde-json` resolve to the same crate. We normalize
		// to the hyphen form to prevent storage fragmentation, since
		// Cargo.toml dep keys conventionally use hyphens and
		// `cargo add` prefers them.
		//
		// Unlike PyPI (where the registry enforces a single canonical
		// form), crates.io stores the owner-published spelling. We
		// pick hyphen-canonical because: (1) Cargo.toml dep keys
		// use it by convention, (2) purl spec examples use it, (3)
		// the API accepts both for lookups, so fragmentation is the
		// only real risk.
		if out.Ecosystem == "cargo" {
			normalized := NormalizeCrateName(out.ShortName)
			if normalized != out.ShortName {
				out.ShortName = normalized
				out.CanonicalURI = "pkg:cargo/" + normalized
				if out.Version != "" {
					out.CanonicalURI += "@" + out.Version
				}
			}
		}

		// Go modules: derive a github CloneURL when the import path
		// has an algorithmic mapping. Pure string transformation
		// driven by the same equivalences alternates.go encodes; no
		// network. Vanity hosts without an algorithmic mapping
		// (gopkg.in, modernc.org, k8s.io, …) keep CloneURL empty —
		// proxy.golang.org Origin lookups are a v2 concern.
		//
		// Closes the v0.1 dispatch-gate gap surfaced when running
		// `signatory analyze --clone --refresh pkg:golang/...`:
		// without CloneURL stamped here, isGitHostedEntity returns
		// false in collectorsFor and the github + git + repofiles
		// + openssf collectors silently skip.
		if out.Ecosystem == "golang" || out.Ecosystem == "go" {
			// Reconstruct the import path from parts after the
			// ecosystem prefix, replacing the last segment's @V
			// with the bare name (out.Version is already extracted
			// by the lastSeg parsing above).
			importParts := make([]string, len(parts)-1)
			copy(importParts, parts[1:])
			if out.Version != "" {
				importParts[len(importParts)-1] = out.ShortName
			}
			out.CloneURL = derivedGoCloneURL(strings.Join(importParts, "/"))
		}

	case "identity", "org":
		// identity:<platform>/<user> or org:<platform>/<org>
		if len(parts) < 2 {
			return nil, fmt.Errorf("%s URI %q: expected platform/name, got %d segment(s)", scheme, uri, len(parts))
		}
		out.Platform = strings.ToLower(parts[0])
		out.ShortName = strings.ToLower(parts[len(parts)-1])
		// Rebuild CanonicalURI from the case-folded components so
		// already-canonical-shaped inputs match what
		// CanonicalIdentityURI / CanonicalOrgURI produce. See the
		// repo case above for the full rationale.
		if scheme == "identity" {
			out.CanonicalURI = CanonicalIdentityURI(out.Platform, out.ShortName)
		} else {
			out.CanonicalURI = CanonicalOrgURI(out.Platform, out.ShortName)
		}

	case "patch":
		// patch:<platform>/<owner>/<repo>/<id>
		if len(parts) < 4 {
			return nil, fmt.Errorf("patch URI %q: expected platform/owner/repo/id, got %d segment(s)", uri, len(parts))
		}
		// Case fold platform/owner/repo to match CanonicalPatchURI.
		// The patch id (parts[3]) is preserved verbatim — patch ids
		// are opaque tokens (PR/issue numbers on github, MR numbers
		// on gitlab, etc.) and CanonicalPatchURI does not lowercase
		// them.
		out.Platform = strings.ToLower(parts[0])
		out.Owner = strings.ToLower(parts[1])
		repo := strings.ToLower(parts[2])
		id := parts[3]
		// ShortName renders as "repo#id" for human display —
		// preserves both the repo and the patch number in one
		// token without requiring callers to hand-compose.
		out.ShortName = repo + "#" + id
		out.CanonicalURI = CanonicalPatchURI(out.Platform, out.Owner, repo, id)

	default:
		// Scheme is in validURISchemes but this switch doesn't
		// know it. That means validURISchemes grew without a
		// matching case here — loud failure is better than a
		// silent degradation to a default ShortName.
		return nil, fmt.Errorf("canonical URI %q uses scheme %q which ResolveTarget does not yet handle", uri, scheme)
	}

	return out, nil
}

// parsePkgGoDevURL recognizes pkg.go.dev module/package URLs and
// extracts the import path plus an optional `@<version>` suffix.
// Returns (importPath, version, true) on a match; version is empty
// when no @V is present. Returns ("", "", false) on anything that
// isn't a well-formed pkg.go.dev/<import-path> URL.
//
// Accepted shapes:
//
//	https://pkg.go.dev/github.com/owner/repo
//	https://pkg.go.dev/github.com/owner/repo@v1.2.3
//	https://pkg.go.dev/github.com/owner/repo/cmd/sub        (subpackage; importPath is full path)
//	https://pkg.go.dev/golang.org/x/mod
//	https://pkg.go.dev/gopkg.in/yaml.v3
//	http://pkg.go.dev/...                                   (http variant)
//
// Host-anchoring rejects lookalike hosts like
// "pkg.go.dev.attacker.example/..." by requiring an exact match on
// "pkg.go.dev/" after the optional scheme strip. Mirrors the
// isGitHubURL / parseNpmjsURL anchoring trick.
//
// Output is the import path verbatim — does NOT lowercase, does NOT
// strip subpackages. Caller (canonicalGoModuleURI) applies those
// transforms when constructing the canonical pkg:golang/ URI.
func parsePkgGoDevURL(input string) (importPath, version string, ok bool) {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")

	rest, hostOK := strings.CutPrefix(s, "pkg.go.dev/")
	if !hostOK || rest == "" {
		return "", "", false
	}

	// Drop fragment and query — the pkg.go.dev UI adds these for
	// version pickers, doc anchors, etc. Not part of the module
	// identifier.
	rest, _, _ = strings.Cut(rest, "#")
	rest, _, _ = strings.Cut(rest, "?")
	rest = strings.TrimSuffix(rest, "/")

	if rest == "" {
		return "", "", false
	}

	if path, ver, hasVersion := strings.Cut(rest, "@"); hasVersion {
		if path == "" || ver == "" {
			return "", "", false
		}
		// Reject nested @: a path with multiple @ separators is
		// ambiguous (Go versions don't contain @, so the second one
		// must be malformed input).
		if strings.ContainsRune(ver, '@') {
			return "", "", false
		}
		return path, ver, true
	}
	return rest, "", true
}

// canonicalGoModuleURI returns the pkg:golang/<canonical-path> form
// for a Go import path, aligned with the [purl spec](https://github.com/package-url/purl-spec).
//
// Two cases:
//
//   - github-hosted (importPath starts with "github.com/"): owner/repo
//     are case-folded to lowercase (github is case-insensitive at the
//     host layer; lowercase is the canonical form per
//     CanonicalRepoURI's invariant). Anything beyond owner/repo is
//     stripped — the canonical entity is the Go module at its module
//     root, not a subpackage docs page. Mirrors the subpackage
//     stripping in gomod's canonicalizeGoImportPath.
//
//   - non-github vanity paths: returned verbatim under pkg:golang/.
//     Module boundaries depend on each vanity host's resolver, so we
//     don't try to identify a module root within the path; the full
//     import path is the canonical identity.
//
// Empty input returns empty output; caller decides how to handle.
//
// Used by both ResolveTarget's vanity discriminator and the
// pkg.go.dev URL parser so the two write paths stay in sync. The
// gomod parser doesn't use this helper — it deliberately uses the
// repo:github/ lens for github-hosted modules to support survey/CLI
// repo-identity workflows.
func canonicalGoModuleURI(importPath string) string {
	if importPath == "" {
		return ""
	}
	const githubPrefix = "github.com/"
	if rest, ok := strings.CutPrefix(importPath, githubPrefix); ok {
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return "pkg:golang/" + githubPrefix +
				strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1])
		}
		// Malformed github prefix without owner+repo. Fall through to
		// preserve the input as a verbatim pkg:golang/ path; the
		// caller's downstream validation surfaces the real issue.
	}
	return "pkg:golang/" + importPath
}

// derivedGoCloneURL returns the https://github.com/... clone URL for
// a Go module import path that has an algorithmic mapping to github.
// Returns "" for vanity hosts (gopkg.in, modernc.org, k8s.io, …) that
// need external resolution; those are a v2 concern.
//
// Mapping rules mirror alternates.go:
//
//   - github.com/<owner>/<repo>[/...]  →  https://github.com/<owner>/<repo>
//   - golang.org/x/<Y>[/...]           →  https://github.com/golang/<Y>
//
// Owner/repo are lowercased to match canonicalGoModuleURI's invariant
// (github is case-insensitive at the host layer; lowercase is the
// canonical form). Subpackages beyond the module root are stripped.
//
// Pure string transformation, no I/O. Empty input → empty output.
func derivedGoCloneURL(importPath string) string {
	if importPath == "" {
		return ""
	}
	if rest, ok := strings.CutPrefix(importPath, "github.com/"); ok {
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return "https://github.com/" +
				strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1])
		}
		return ""
	}
	if rest, ok := strings.CutPrefix(importPath, "golang.org/x/"); ok {
		// golang.org/x/Y → github.com/golang/Y. Strip any subpackage.
		firstSeg, _, _ := strings.Cut(rest, "/")
		if firstSeg != "" {
			return "https://github.com/golang/" + strings.ToLower(firstSeg)
		}
	}
	return ""
}
