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
//   - GitHub shorthand:   owner/repo (back-compat default; bare
//     shorthand with no host resolves to GitHub)
//   - Forge hostname:     github.com/owner/repo,
//     gitlab.com/owner/repo,
//     codeberg.org/owner/repo
//   - Forge URL:          https://{github.com|gitlab.com|codeberg.org}/
//     owner/repo (optional .git suffix)
//   - SSH form:           git@<host>:owner/repo.git for each
//     first-classed forge
//   - Canonical repo:     repo:<forge>/owner/repo (forge ∈ github,
//     gitlab, codeberg)
//   - Canonical pkg:      pkg:cargo/atuin (and other ecosystems)
//   - Canonical identity: identity:<forge>/<user>
//   - Canonical org:      org:<forge>/<name>
//   - Canonical patch:    patch:<forge>/owner/repo/42
//
// Returns an error for empty input, malformed canonical URIs, or
// non-URI strings that don't parse as a GitHub shorthand or
// recognized forge URL.
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

	// Already-canonical case: input starts with one of our known
	// schemes. Validate the whole URI and extract per-scheme metadata.
	for _, prefix := range validURISchemes {
		if strings.HasPrefix(s, prefix) {
			return resolveCanonicalURI(s)
		}
	}

	// Copy-paste-from-browser registry URLs (npmjs.com, pypi.org,
	// crates.io, rubygems.org, Maven Central, pkg.go.dev). Each is
	// host-anchored against lookalike attacks before being
	// converted to its pkg: canonical form.
	if uri, ok := tryRegistryURLs(s); ok {
		return resolveCanonicalURI(uri)
	}

	// Reject non-github URLs and non-github SCP-form inputs before
	// falling through to GitHub-shorthand parsing — without this
	// gate, NormalizeGitHubRepoInput's prefix-strip would silently
	// produce misleading repo:github/ URIs for gitlab/bitbucket/
	// self-hosted inputs.
	if err := rejectUnrecognizedForgeURL(s, raw); err != nil {
		return nil, err
	}

	// Pre-strip @<version> from github shorthand inputs before
	// delegating to NormalizeGitHubRepoInput. Without this,
	// NormalizeGitHubRepoInput's path-segment validator rejects
	// `Y@v1.0.0` because `@` isn't in its allowed character set.
	bareInput, requestedVersion, err := extractVersionSuffix(s, raw)
	if err != nil {
		return nil, err
	}

	// Discriminate Go vanity paths from github shorthand BEFORE
	// falling through to NormalizeGitHubRepoInput. validPathSegment
	// accepts dots in owner names, so without this branch
	// "modernc.org/sqlite" would satisfy NormalizeGitHubRepoInput as
	// owner=modernc.org, name=sqlite and produce
	// repo:github/modernc.org/sqlite — the misclassification dogfood
	// entry 1 surfaced.
	if r, ok, err := resolveVanityImportPath(bareInput, requestedVersion); ok {
		return r, err
	}

	// Fall through to forge-shorthand parsing. Any form
	// NormalizeForgeRepoInput accepts gets promoted to a canonical
	// repo: URI; the platform field comes from host detection inside
	// the normalizer, with bare "owner/repo" defaulting to GitHub.
	canonicalURI, platform, owner, name, err := NormalizeForgeRepoInput(bareInput)
	if err != nil {
		return nil, fmt.Errorf(
			"target %q is not a canonical URI (repo:/pkg:/identity:/org:/patch:) and does not parse as a forge repo reference: %w",
			raw, err)
	}
	return &ResolvedTarget{
		CanonicalURI: appendVersion(canonicalURI, requestedVersion),
		ShortName:    name,
		Scheme:       "repo",
		Platform:     platform,
		Owner:        owner,
		// CloneURLForRepoPlatform returns "" for platforms that aren't
		// yet first-classed at the clone layer; the canonical-URI path
		// (parseRepoBody) applies the same rule. The two paths must
		// agree, otherwise the same target reached through different
		// input shapes would carry different CloneURL values.
		CloneURL: CloneURLForRepoPlatform(platform, owner, name),
		Version:  requestedVersion,
	}, nil
}

// tryRegistryURLs recognizes copy-paste-from-browser package
// registry URLs and converts them to the equivalent pkg: canonical
// URI. Returns ("", false) for inputs that aren't a known registry
// URL form — the caller falls through to URL-rejection / version-
// extraction / vanity / GitHub-shorthand parsing.
//
// Recognized hosts:
//
//   - npmjs.com:    pkg:npm/<name>[@<version>] (case-preserving)
//   - pypi.org:     pkg:pypi/<name>[@<version>] (PEP 503-normalized
//     inside parsePyPIURL — so /project/Requests/ and
//     /project/requests/ both produce pkg:pypi/requests; the npm
//     registry is case-sensitive and we preserve case there)
//   - crates.io:    pkg:cargo/<name>[@<version>]
//   - rubygems.org: pkg:gem/<name>[@<version>]
//   - Maven Central (central.sonatype.com / search.maven.org /
//     mvnrepository.com): pkg:maven/<group>/<artifact>[@<version>]
//   - pkg.go.dev:   pkg:golang/<import-path>[@<version>] —
//     canonicalGoModuleURI strips subpackages and case-folds
//     github-hosted modules, matching the gomod parser.
//
// Each parser is host-anchored: the host prefix must match exactly
// after scheme/www strip, so lookalikes like
// `npmjs.com.attacker.example/...` fall through.
func tryRegistryURLs(s string) (canonicalURI string, ok bool) {
	if name, version, ok := parseNpmjsURL(s); ok {
		return appendVersion("pkg:npm/"+name, version), true
	}
	if name, version, ok := parsePyPIURL(s); ok {
		return appendVersion("pkg:pypi/"+name, version), true
	}
	if name, version, ok := parseCratesIOURL(s); ok {
		return appendVersion("pkg:cargo/"+name, version), true
	}
	if name, version, ok := parseRubyGemsURL(s); ok {
		return appendVersion("pkg:gem/"+name, version), true
	}
	if group, artifact, version, ok := parseMavenCentralURL(s); ok {
		return appendVersion("pkg:maven/"+group+"/"+artifact, version), true
	}
	if importPath, version, ok := parsePkgGoDevURL(s); ok {
		return appendVersion(canonicalGoModuleURI(importPath), version), true
	}
	return "", false
}

// rejectUnrecognizedForgeURL fails inputs that look like URLs or
// SCP-form references but don't target a recognized forge. Without
// this gate NormalizeForgeRepoInput's host-prefix detection falls
// through and the input ends up classified as a bare github-shorthand
// owner/repo, silently producing a misleading repo:github/<host>/<owner>
// URI (e.g. https://bitbucket.org/team/repo would become
// repo:github/bitbucket.org/team). Registry URLs (npmjs.com,
// pypi.org, crates.io, ...) are recognized earlier in ResolveTarget
// via tryRegistryURLs and never reach this gate.
//
// Recognized forges (URL form): github.com, codeberg.org, gitlab.com.
//
// SCP-form (git@host:owner/repo.git) is gated separately because
// NormalizeForgeRepoInput strips `git@` unconditionally and would
// misclassify `git@bitbucket.org:foo/bar.git` if the SCP host wasn't
// also gated.
func rejectUnrecognizedForgeURL(s, raw string) error {
	if strings.Contains(s, "://") &&
		!isGitHubURL(s) && !isCodebergURL(s) && !isGitLabURL(s) {
		return fmt.Errorf(
			"target %q is a URL but not a github.com, codeberg.org, gitlab.com, npmjs.com, pypi.org, crates.io, rubygems.org, or Maven Central URL; other hosting platforms are not yet supported by signatory",
			raw)
	}
	if strings.HasPrefix(s, "git@") &&
		!strings.HasPrefix(s, "git@github.com:") &&
		!strings.HasPrefix(s, "git@codeberg.org:") &&
		!strings.HasPrefix(s, "git@gitlab.com:") {
		return fmt.Errorf(
			"target %q is an SCP-form URL but not a github.com, codeberg.org, or gitlab.com host; other hosting platforms are not yet supported by signatory",
			raw)
	}
	return nil
}

// extractVersionSuffix peels an optional @<version> suffix off a
// github-shorthand input (owner/repo@vX.Y.Z), returning the bare
// input and the version. Returns (input, "", nil) when no version
// is present.
//
// Three guards:
//
//   - SCP-form (git@github.com:...) is skipped — the @ there is
//     part of the SSH user-host syntax, not a version separator.
//     SCP-form was already gated by rejectUnrecognizedForgeURL; here we
//     re-guard so we don't misread that @ as a version split.
//   - The version separator @ must come AFTER any / — otherwise
//     it's part of the host portion of a URL (URLs at this point
//     have https:// stripped, so a / before @ is the
//     path-vs-version boundary).
//   - Nested @ in the path portion is rejected. Catches inputs like
//     "owner/repo@v1.0@extra" where LastIndexByte would otherwise
//     accept "extra" as the version and silently drop the middle @.
//
// The canonical URI ResolveTarget synthesizes on the way out
// preserves the @V suffix — mirrors the pkg: shape and lets
// storage code route both through SplitURIVersion uniformly.
func extractVersionSuffix(s, raw string) (bare, version string, err error) {
	at := strings.LastIndexByte(s, '@')
	if at < 0 {
		return s, "", nil
	}
	if strings.HasPrefix(s, "git@") {
		return s, "", nil
	}
	candidate := s[at+1:]
	before := s[:at]
	if !strings.ContainsRune(before, '/') {
		return s, "", nil
	}
	if candidate == "" {
		return "", "", fmt.Errorf("target %q: empty version after '@'", raw)
	}
	if strings.ContainsRune(before, '@') {
		return "", "", fmt.Errorf("target %q: nested separators not allowed (multiple '@' in input)", raw)
	}
	return before, candidate, nil
}

// resolveVanityImportPath detects Go vanity import paths
// (modernc.org/sqlite, gopkg.in/yaml.v3, k8s.io/client-go) by
// looking for a dot in the first path segment — GitHub
// usernames/orgs cannot contain dots (per github.com's name rules:
// alphanumeric and hyphens only), so a dot in the first segment is
// unambiguous evidence of a vanity host.
//
// Returns (resolved, true, err) on a match (err comes from
// resolveCanonicalURI's parsing of the synthesized pkg:go/<path>
// URI; ok stays true even when err is non-nil so the caller doesn't
// fall through to forge-shorthand). Returns (nil, false, nil) when
// the input doesn't look like a vanity path; the caller falls
// through to forge-shorthand parsing.
//
// Known forge hosts (github.com, codeberg.org, gitlab.com) are
// excluded from vanity classification — bare "codeberg.org/foo/bar"
// has a dot in its first segment but is unambiguously a Codeberg
// repo reference, not a Go vanity path. The exclusion list mirrors
// the host table NormalizeForgeRepoInput dispatches on.
//
// Output mirrors what the gomod parser produces for the same
// import path (pkg:go/<full-path>), keeping ResolveTarget and the
// parser in agreement on canonical form. Vanity-resolution to a
// github form (e.g. golang.org/x/Y → repo:github/golang/Y) is a
// lookup-side alternate handled elsewhere — see LookupEntity /
// AlternateURIs.
func resolveVanityImportPath(bareInput, requestedVersion string) (*ResolvedTarget, bool, error) {
	firstSeg, _, ok := strings.Cut(bareInput, "/")
	if !ok || firstSeg == "" {
		return nil, false, nil
	}
	// Exclude every known forge host from the vanity branch. The
	// path-form prefixes (host + "/") are sufficient here — SCP-form
	// "git@" inputs are excluded by the dedicated check below, and
	// scheme-prefixed URL inputs (https://...) are excluded because
	// firstSeg lacks a dot in their case ("https:" vs the host).
	for _, h := range forgeHostPrefix {
		if !strings.HasSuffix(h.prefix, "/") {
			continue
		}
		if strings.HasPrefix(bareInput, h.prefix) {
			return nil, false, nil
		}
	}
	if strings.HasPrefix(bareInput, "git@") {
		return nil, false, nil
	}
	if !strings.ContainsRune(firstSeg, '.') {
		return nil, false, nil
	}
	canonicalURI := appendVersion(canonicalGoModuleURI(bareInput), requestedVersion)
	r, err := resolveCanonicalURI(canonicalURI)
	return r, true, err
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
	rest, ok := stripNpmjsHostPrefix(input)
	if !ok {
		return "", "", false
	}
	rest = stripURLFragmentAndQuery(rest)
	return splitNpmjsNameAndVersion(rest)
}

// stripNpmjsHostPrefix strips the optional scheme/www and the
// "npmjs.com/package/" prefix from input, returning the remainder
// of the path. Returns ("", false) when the input doesn't have an
// npmjs.com/package/<rest> shape after prefix-stripping, including
// when <rest> is empty.
//
// Host anchoring: rejects lookalike hosts like
// "npmjs.com.attacker.example/package/x" by requiring an exact
// match on "npmjs.com/" after the optional www./scheme strip.
func stripNpmjsHostPrefix(input string) (rest string, ok bool) {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	s, hostOK := strings.CutPrefix(s, "npmjs.com/")
	if !hostOK {
		return "", false
	}
	s, pathOK := strings.CutPrefix(s, "package/")
	if !pathOK || s == "" {
		return "", false
	}
	return s, true
}

// stripURLFragmentAndQuery removes any trailing #fragment or ?query
// portion from a URL path. The npmjs.com UI appends these for tabs,
// version pickers, etc., and they're not part of the package
// identifier.
func stripURLFragmentAndQuery(s string) string {
	if before, _, hadHash := strings.Cut(s, "#"); hadHash {
		s = before
	}
	if before, _, hadQuery := strings.Cut(s, "?"); hadQuery {
		s = before
	}
	return s
}

// splitNpmjsNameAndVersion splits the path after npmjs.com/package/
// into (name, version). Handles both shapes:
//
//   - scoped:   @scope/name[/v/<version>][/...]   (name is two segments)
//   - unscoped: name[/v/<version>][/...]          (name is one segment)
//
// Optional /v/<version> at the trailing position yields the
// version; anything else (subpaths, trailing /README) is ignored.
// Version extraction is delegated to extractNpmjsVPathVersion so
// both branches share the same logic.
func splitNpmjsNameAndVersion(rest string) (name, version string, ok bool) {
	if strings.HasPrefix(rest, "@") {
		parts := strings.SplitN(rest, "/", 4)
		if len(parts) < 2 || parts[0] == "@" || parts[1] == "" {
			return "", "", false
		}
		return parts[0] + "/" + parts[1], extractNpmjsVPathVersion(parts[2:]), true
	}
	segs := strings.SplitN(rest, "/", 4)
	if segs[0] == "" {
		return "", "", false
	}
	return segs[0], extractNpmjsVPathVersion(segs[1:]), true
}

// extractNpmjsVPathVersion returns the version string when segs
// starts with "v" / "<version>" (the npmjs.com /v/<version> page
// convention); otherwise returns "". Trims any subpath off the
// version when present — defensive against the scoped split landing
// subpaths in segs[1] for inputs like "@scope/name/v/1.0/README.md"
// (SplitN limit-4 keeps "1.0/README.md" as a single trailing
// segment).
func extractNpmjsVPathVersion(segs []string) string {
	if len(segs) < 2 || segs[0] != "v" || segs[1] == "" {
		return ""
	}
	ver := segs[1]
	if idx := strings.IndexByte(ver, '/'); idx >= 0 {
		ver = ver[:idx]
	}
	return ver
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

// isCodebergURL returns true when input is an http(s) URL whose host
// (after scheme strip) is "codeberg.org". Mirrors isGitHubURL — same
// scheme/www strip, same host-anchored prefix check that requires
// either an exact host match or a trailing slash so lookalike hosts
// like `codeberg.org.attacker.example` are rejected.
//
// Codeberg is a Forgejo deployment; this helper only answers "could
// this be a Codeberg URL?" — owner/name validation and clone-URL
// construction live in NormalizeForgeRepoInput.
func isCodebergURL(input string) bool {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	return strings.HasPrefix(s, "codeberg.org/") || s == "codeberg.org"
}

// isGitLabURL returns true when input is an http(s) URL whose host
// (after scheme strip) is "gitlab.com". Mirrors isGitHubURL and
// isCodebergURL — same scheme/www strip, same host-anchored prefix
// check.
//
// Recognizes only the public gitlab.com instance; self-hosted GitLab
// (gitlab.<company>.com, etc.) remains rejected because we have no
// way to validate it doesn't aim at an internal-only host the
// operator didn't intend to query. Adding self-hosted support requires
// an explicit allow-list mechanism, deferred to a follow-up.
func isGitLabURL(input string) bool {
	s := strings.TrimPrefix(input, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	return strings.HasPrefix(s, "gitlab.com/") || s == "gitlab.com"
}

// resolveCanonicalURI handles an input already in canonical form
// (matched by one of the known scheme prefixes). Validates,
// decomposes into per-scheme fields, and populates CloneURL for
// supported repo: platforms. Per-scheme parsing lives in
// parseRepoBody / parsePkgBody / parseIdentityOrOrgBody /
// parsePatchBody so each can read top-down without nesting.
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

	switch scheme {
	case "repo":
		return parseRepoBody(parts, uri)
	case "pkg":
		return parsePkgBody(parts, uri)
	case "identity", "org":
		return parseIdentityOrOrgBody(scheme, parts, uri)
	case "patch":
		return parsePatchBody(parts, uri)
	default:
		// Scheme is in validURISchemes but this switch doesn't
		// know it. That means validURISchemes grew without a
		// matching case here — loud failure is better than a
		// silent degradation to a default ShortName.
		return nil, fmt.Errorf("canonical URI %q uses scheme %q which ResolveTarget does not yet handle", uri, scheme)
	}
}

// appendVersion returns uri unchanged when version is empty,
// otherwise uri + "@" + version. Used by every place that
// reconstructs a canonical URI from its parts (registry-URL
// converters, scheme-specific normalizers, the GitHub-shorthand
// fall-through path) so the "version-suffix shape" rule lives in
// one spot.
func appendVersion(uri, version string) string {
	if version == "" {
		return uri
	}
	return uri + "@" + version
}

// parseLastSegmentAtVersion splits a possible "name@version" suffix
// from a URI's last path segment. Used by both the repo and pkg
// scheme handlers to extract the optional version with consistent
// validation: empty name, empty version, and nested '@' are all
// rejected with scheme-labeled error messages.
//
// On a clean segment with no '@', returns (segment, "", nil).
// On a "name@version" pair, returns (name, version, nil).
func parseLastSegmentAtVersion(lastSeg, schemeLabel, uri string) (name, version string, err error) {
	n, v, ok := strings.Cut(lastSeg, "@")
	if !ok {
		return lastSeg, "", nil
	}
	if n == "" {
		return "", "", fmt.Errorf("%s URI %q: empty name before '@version'", schemeLabel, uri)
	}
	if v == "" {
		return "", "", fmt.Errorf("%s URI %q: empty version after '@'", schemeLabel, uri)
	}
	if strings.ContainsRune(v, '@') {
		return "", "", fmt.Errorf("%s URI %q: version contains '@' (nested separators not allowed)", schemeLabel, uri)
	}
	return n, v, nil
}

// parseRepoBody extracts the platform/owner/name (+ optional
// version) from a repo:<platform>/<owner>/<name>[@<version>] URI.
// The optional @<version> suffix on the last segment names a git
// ref (tag, branch, commit) — used by signatory handoff to clone
// the named ref. Storage strips the suffix before keying the entity
// row (see SplitURIVersion).
//
// Case-folds platform/owner/name to the form CanonicalRepoURI
// produces. Without this, a caller passing
// `repo:github/BurntSushi/toml` (already-canonical-shaped but not
// yet case-folded) would survive ValidateCanonicalURI unchanged and
// then fragment against entities the constructor created at the
// lowercased URI.
//
// CloneURL is populated for first-classed platforms (github,
// codeberg, gitlab) via CloneURLForRepoPlatform; other platforms
// intentionally leave it empty so callers that check for non-empty
// CloneURL before clone-dir actions don't silently invoke
// `git clone` against a URL shape the CLI hasn't validated. The
// shorthand branch in ResolveTarget applies the same helper, so
// canonical-URI input and shorthand URL input produce the same
// CloneURL for any given target.
func parseRepoBody(parts []string, uri string) (*ResolvedTarget, error) {
	if len(parts) < 3 {
		return nil, fmt.Errorf("repo URI %q: expected platform/owner/name, got %d segment(s)", uri, len(parts))
	}
	name, version, err := parseLastSegmentAtVersion(parts[len(parts)-1], "repo", uri)
	if err != nil {
		return nil, err
	}
	platform := strings.ToLower(parts[0])
	owner := strings.ToLower(parts[1])
	short := strings.ToLower(name)
	return &ResolvedTarget{
		CanonicalURI: appendVersion(CanonicalRepoURI(platform, owner, short), version),
		Scheme:       "repo",
		Platform:     platform,
		Owner:        owner,
		ShortName:    short,
		Version:      version,
		CloneURL:     CloneURLForRepoPlatform(platform, owner, short),
	}, nil
}

// parsePkgBody extracts the ecosystem/name (+ optional version)
// from a pkg:<ecosystem>/<name...>[@<version>] URI. Name may contain
// further slashes (npm scoped packages: pkg:npm/@types/node); the
// scope prefix on scoped names lives in its own segment and never
// collides with the version @.
//
// Delegates ecosystem-specific normalization (PyPI PEP 503,
// Cargo hyphen-canonical, Go module clone-URL derivation) to
// applyPkgEcosystemNormalization so the dispatch table stays in
// one place.
func parsePkgBody(parts []string, uri string) (*ResolvedTarget, error) {
	if len(parts) < 2 {
		return nil, fmt.Errorf("pkg URI %q: expected ecosystem/name, got %d segment(s)", uri, len(parts))
	}
	name, version, err := parseLastSegmentAtVersion(parts[len(parts)-1], "pkg", uri)
	if err != nil {
		return nil, err
	}
	out := &ResolvedTarget{
		CanonicalURI: uri,
		Scheme:       "pkg",
		Ecosystem:    parts[0],
		ShortName:    name,
		Version:      version,
	}
	applyPkgEcosystemNormalization(out, parts)
	return out, nil
}

// applyPkgEcosystemNormalization mutates out in place to apply
// per-ecosystem canonical-form rules:
//
//   - pypi: PEP 503 name normalization. Unlike npm (case-sensitive)
//     and Go (path-preserving), PyPI's registry canonicalizes
//     names on lookup — `Requests`, `requests`, and `python_dotenv`
//     all map to the same identity. The canonical URI must match,
//     or storage fragments across identities the registry considers
//     the same. See pypi.go for the normalization spec.
//   - cargo: hyphen-canonical name. crates.io treats hyphens and
//     underscores as equivalent for lookups — both `serde_json` and
//     `serde-json` resolve to the same crate. Hyphen-canonical
//     because (1) Cargo.toml dep keys use it by convention, (2)
//     purl spec examples use it, (3) the API accepts both for
//     lookups so fragmentation is the only real risk.
//   - golang/go: derive a github CloneURL when the import path has
//     an algorithmic mapping. Pure string transformation driven by
//     the same equivalences alternates.go encodes; no network.
//     Vanity hosts without an algorithmic mapping (gopkg.in,
//     modernc.org, k8s.io, …) keep CloneURL empty — proxy.golang.org
//     Origin lookups are a v2 concern. Closes the v0.1 dispatch-gate
//     gap surfaced when running `signatory analyze --clone --refresh
//     pkg:golang/...` (without CloneURL stamped here,
//     isGitHostedEntity returns false in collectorsFor and the
//     git-side collectors silently skip).
func applyPkgEcosystemNormalization(out *ResolvedTarget, parts []string) {
	switch out.Ecosystem {
	case "pypi":
		if normalized := NormalizePyPIName(out.ShortName); normalized != out.ShortName {
			out.ShortName = normalized
			out.CanonicalURI = appendVersion("pkg:pypi/"+normalized, out.Version)
		}
	case "cargo":
		if normalized := NormalizeCrateName(out.ShortName); normalized != out.ShortName {
			out.ShortName = normalized
			out.CanonicalURI = appendVersion("pkg:cargo/"+normalized, out.Version)
		}
	case "golang", "go":
		// Reconstruct the import path from parts after the
		// ecosystem prefix, replacing the last segment's @V
		// with the bare name (out.Version is already extracted
		// upstream).
		importParts := make([]string, len(parts)-1)
		copy(importParts, parts[1:])
		if out.Version != "" {
			importParts[len(importParts)-1] = out.ShortName
		}
		out.CloneURL = derivedGoCloneURL(strings.Join(importParts, "/"))
	}
}

// parseIdentityOrOrgBody extracts the platform/name from
// identity:<platform>/<user> or org:<platform>/<org> URIs.
// Case-folds platform and name and rebuilds CanonicalURI from the
// case-folded components so already-canonical-shaped inputs match
// what CanonicalIdentityURI / CanonicalOrgURI produce. See the
// godoc on parseRepoBody for the case-folding rationale.
func parseIdentityOrOrgBody(scheme string, parts []string, uri string) (*ResolvedTarget, error) {
	if len(parts) < 2 {
		return nil, fmt.Errorf("%s URI %q: expected platform/name, got %d segment(s)", scheme, uri, len(parts))
	}
	platform := strings.ToLower(parts[0])
	name := strings.ToLower(parts[len(parts)-1])
	out := &ResolvedTarget{
		Scheme:    scheme,
		Platform:  platform,
		ShortName: name,
	}
	if scheme == "identity" {
		out.CanonicalURI = CanonicalIdentityURI(platform, name)
	} else {
		out.CanonicalURI = CanonicalOrgURI(platform, name)
	}
	return out, nil
}

// parsePatchBody extracts the platform/owner/repo/id from a
// patch:<platform>/<owner>/<repo>/<id> URI. Case-folds
// platform/owner/repo to match CanonicalPatchURI; the patch id is
// preserved verbatim — patch ids are opaque tokens (PR/issue
// numbers on github, MR numbers on gitlab, etc.) and
// CanonicalPatchURI does not lowercase them.
//
// ShortName renders as "repo#id" for human display so callers can
// surface both the repo and the patch number in one token without
// hand-composing.
func parsePatchBody(parts []string, uri string) (*ResolvedTarget, error) {
	if len(parts) < 4 {
		return nil, fmt.Errorf("patch URI %q: expected platform/owner/repo/id, got %d segment(s)", uri, len(parts))
	}
	platform := strings.ToLower(parts[0])
	owner := strings.ToLower(parts[1])
	repo := strings.ToLower(parts[2])
	id := parts[3]
	return &ResolvedTarget{
		CanonicalURI: CanonicalPatchURI(platform, owner, repo, id),
		Scheme:       "patch",
		Platform:     platform,
		Owner:        owner,
		ShortName:    repo + "#" + id,
	}, nil
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
