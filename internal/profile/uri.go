package profile

import (
	"fmt"
	"regexp"
	"strings"
)

// Canonical URIs
//
// Every entity carries a canonical_uri — the stable, parseable, external
// identifier that gets compared for equality across the system. Two
// inputs that refer to the same underlying thing must produce identical
// canonical URIs, or posture decisions and signals will fragment across
// duplicate entities (#53).
//
// This file defines the canonical URI schemes:
//
//	pkg:<ecosystem>/<name>         — packages (purl-compatible prefix)
//	repo:<platform>/<owner>/<name> — source repositories
//	identity:<platform>/<user>     — contributor identities
//	org:<platform>/<name>          — organizations
//	patch:<platform>/<owner>/<repo>/<id>
//	                               — pull requests / merge requests / patches
//
// The platform is lowercased in canonical form (`github`, not `GitHub`).
// Names preserve their original case because case can be semantically
// meaningful (e.g., npm is case-sensitive and `Express` ≠ `express`).
//
// Package URIs use the `pkg:` prefix from the purl spec so that SBOM
// tools (SPDX, CycloneDX, OSV) can consume them without translation.
// Non-package types use signatory's own scheme because purl doesn't
// cover repos/identities/orgs/patches as distinct entity kinds.

// URI scheme prefixes — kept as constants so callers can branch on them
// without magic strings.
const (
	URISchemePackage  = "pkg:"
	URISchemeRepo     = "repo:"
	URISchemeIdentity = "identity:"
	URISchemeOrg      = "org:"
	URISchemePatch    = "patch:"
)

// Platform is the host/namespace that backs a non-package entity.
//
// PlatformCodeberg names the Forgejo-based Codeberg instance. Forgejo
// is a soft-fork of Gitea, and codeberg.org is its largest public
// deployment. The constant is added ahead of full Codeberg URL-input
// parsing (see Normalize*RepoInput) so the canonical URI shape
// (repo:codeberg/<owner>/<name>) and the Platform field on
// ResolvedTarget have a single source of truth before downstream
// dispatch is wired up.
const (
	PlatformGitHub   = "github"
	PlatformGitLab   = "gitlab"
	PlatformCodeberg = "codeberg"
)

// validPathSegment restricts the owner/name/user portion of a URI to
// characters safe for both filesystem paths and URL paths. This is
// intentionally permissive — it's not an HTTP-safety boundary (that's
// the collector's job with its own validators), it's an "is this
// plausibly a name" check.
var validPathSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// MaxCanonicalURILength is the hard upper bound on canonical URI length.
// Real-world URIs are well under 100 bytes; 512 is generous slack and a
// hard cap to prevent log/display blowup, expensive rendering, and
// length-based DoS via attacker-supplied input.
const MaxCanonicalURILength = 512

// validURISchemes is the set of scheme prefixes ValidateCanonicalURI
// will accept. New schemes added to this list automatically pass
// validation; nothing else does.
var validURISchemes = []string{
	URISchemePackage,
	URISchemeRepo,
	URISchemeIdentity,
	URISchemeOrg,
	URISchemePatch,
}

// ValidateCanonicalURI checks that uri is safe to persist, render,
// log, and display. It is the persistence-boundary defense for
// issue #78 — even if a CLI command, library caller, or future code
// path forgets to validate input, the store rejects bad data.
//
// The validator deliberately does NOT enforce per-scheme semantic
// rules (those live in the Canonical*URI constructors and the
// Normalize*Input parsers). Its job is "this string is safe at the
// boundaries" — not "this string is a semantically perfect canonical
// URI." The two responsibilities have different scopes; mixing them
// would over-restrict legitimate inputs (e.g., scoped npm packages).
//
// Rules, in order:
//
//  1. Non-empty.
//  2. Length ≤ MaxCanonicalURILength.
//  3. No leading or trailing whitespace.
//  4. Every byte is in the printable ASCII range [0x20, 0x7E].
//     This single check defends against:
//     - Control characters (NUL, newline, tab, escape) → log injection
//     - Non-ASCII bytes → lookalike fragmentation (Cyrillic а ≠ Latin a),
//     which is the entire reason canonical URIs were introduced (#53)
//  5. Starts with one of the known scheme prefixes (validURISchemes).
//  6. Has a non-empty body after the scheme.
func ValidateCanonicalURI(uri string) error {
	if uri == "" {
		return fmt.Errorf("canonical URI is empty")
	}
	if len(uri) > MaxCanonicalURILength {
		return fmt.Errorf("canonical URI exceeds maximum length of %d bytes (got %d)",
			MaxCanonicalURILength, len(uri))
	}
	if uri != strings.TrimSpace(uri) {
		return fmt.Errorf("canonical URI has leading or trailing whitespace")
	}

	// Single pass over bytes — anything outside printable ASCII is
	// rejected with a specific error class so callers can tell which
	// boundary the input crossed.
	for i := 0; i < len(uri); i++ {
		b := uri[i]
		if b < 0x20 || b == 0x7F {
			return fmt.Errorf("canonical URI contains control character (0x%02X) at position %d", b, i)
		}
		if b > 0x7F {
			return fmt.Errorf("canonical URI contains non-ASCII byte (0x%02X) at position %d", b, i)
		}
	}

	var scheme string
	for _, s := range validURISchemes {
		if strings.HasPrefix(uri, s) {
			scheme = s
			break
		}
	}
	if scheme == "" {
		return fmt.Errorf("canonical URI %q does not start with a known scheme (expected one of: %s)",
			uri, strings.Join(validURISchemes, ", "))
	}
	if uri == scheme {
		return fmt.Errorf("canonical URI %q has empty body after scheme", uri)
	}

	return nil
}

// CanonicalPackageURI returns the purl-style canonical URI for a
// package entity. Example: CanonicalPackageURI("npm", "express") →
// "pkg:npm/express".
//
// Ecosystem is normalized to lowercase. Name handling depends on the
// ecosystem:
//
//   - npm: preserved as-is (the registry is case-sensitive).
//   - golang: preserved as-is (import paths are verbatim).
//   - pypi: PEP 503-normalized (lowercased + runs of `.`/`-`/`_`
//     collapsed to a single `-`) because PyPI's registry canonicalizes
//     names on lookup; mismatched casing or separator style would
//     fragment the store across identities the registry considers the
//     same. See profile/pypi.go for the normalization spec.
//
// The pypi branch is defense-in-depth: the input-parsing path
// (resolveCanonicalURI) already normalizes pypi names before storing,
// but a caller that constructs a pkg:pypi/ URI directly via this
// function would otherwise emit a non-canonical URI.
func CanonicalPackageURI(ecosystem, name string) string {
	eco := strings.ToLower(ecosystem)
	switch eco {
	case "pypi":
		name = NormalizePyPIName(name)
	case "cargo":
		name = NormalizeCrateName(name)
	case "gem":
		name = NormalizeGemName(name)
	}
	return URISchemePackage + eco + "/" + name
}

// CanonicalRepoURI returns the canonical URI for a source repository.
// Example: CanonicalRepoURI("github", "alecthomas", "kong") →
// "repo:github/alecthomas/kong".
//
// Platform, owner, and name are all lowercased. The platforms we
// support today (GitHub, GitLab, Codeberg) are case-insensitive on
// owner+repo path segments at both the API and git-clone layer, so
// equivalent real-world references (BurntSushi/toml, burntsushi/toml,
// BURNTSUSHI/TOML) must collapse to one canonical URI to prevent
// fragmenting one entity across multiple store rows. If a future
// supported platform is genuinely case-sensitive on owner/name,
// this function gains a platform-aware branch.
func CanonicalRepoURI(platform, owner, name string) string {
	return URISchemeRepo + strings.ToLower(platform) + "/" +
		strings.ToLower(owner) + "/" + strings.ToLower(name)
}

// CloneURLForRepoPlatform returns the canonical https clone URL for a
// recognized forge platform, or "" when the platform is not yet
// first-classed at the clone layer. Empty CloneURL is the staged-
// build signal: clone-dispatch sites (cmd/signatory's
// resolveClonePath, ensureCloneAtPath, isGitHostedEntity) skip
// targets whose platform hasn't been wired rather than invoke
// `git clone` against an unvalidated URL shape.
//
// owner and name are lowercased to match CanonicalRepoURI, keeping
// the two paths that emit ResolvedTarget.CloneURL — parseRepoBody
// for canonical-URI input, ResolveTarget's shorthand branch for
// URL/SCP input — in agreement on the URL string for any given
// target.
//
// Adding a forge means adding a case here AND opening the URL gate
// (rejectUnrecognizedForgeURL + the matching is*URL helper) AND adding
// host-prefix entries to forgeHostPrefix.
func CloneURLForRepoPlatform(platform, owner, name string) string {
	owner = strings.ToLower(owner)
	name = strings.ToLower(name)
	switch strings.ToLower(platform) {
	case PlatformGitHub:
		return "https://github.com/" + owner + "/" + name
	case PlatformCodeberg:
		return "https://codeberg.org/" + owner + "/" + name
	case PlatformGitLab:
		return "https://gitlab.com/" + owner + "/" + name
	}
	return ""
}

// CanonicalIdentityURI returns the canonical URI for a contributor
// identity. Example: CanonicalIdentityURI("github", "alecthomas") →
// "identity:github/alecthomas".
//
// User is lowercased — see CanonicalRepoURI for rationale.
func CanonicalIdentityURI(platform, user string) string {
	return URISchemeIdentity + strings.ToLower(platform) + "/" + strings.ToLower(user)
}

// CanonicalOrgURI returns the canonical URI for an organization.
// Example: CanonicalOrgURI("github", "stretchr") →
// "org:github/stretchr".
//
// Name is lowercased — see CanonicalRepoURI for rationale.
func CanonicalOrgURI(platform, name string) string {
	return URISchemeOrg + strings.ToLower(platform) + "/" + strings.ToLower(name)
}

// CanonicalPatchURI returns the canonical URI for a patch (PR/MR).
// Example: CanonicalPatchURI("github", "alecthomas", "kong", "593") →
// "patch:github/alecthomas/kong/593".
//
// Owner and repo are lowercased — see CanonicalRepoURI for rationale.
// The id is preserved verbatim (typically numeric; case-irrelevant
// by nature of being an ID rather than a path segment).
func CanonicalPatchURI(platform, owner, repo, id string) string {
	return URISchemePatch + strings.ToLower(platform) + "/" +
		strings.ToLower(owner) + "/" + strings.ToLower(repo) + "/" + id
}

// NormalizeGitHubRepoInput takes user-supplied input that refers to a
// GitHub repository and returns its canonical URI plus the extracted
// owner and repo name. It accepts any of the common forms a user might
// type or paste:
//
//	alecthomas/kong
//	github.com/alecthomas/kong
//	https://github.com/alecthomas/kong
//	https://github.com/alecthomas/kong.git
//	http://github.com/alecthomas/kong/
//
// Returns an error if the input doesn't look like "owner/repo" after
// stripping known prefixes, or if either segment contains characters
// that aren't valid in a GitHub name.
//
// This is intentionally GitHub-specific. Multi-forge call sites
// (ResolveTarget, validateCloneOrigin, ensureCloneAtPath, the
// cargo/gem/gopublish/npm/pypi resolvers) use NormalizeForgeRepoInput
// instead — that path admits codeberg.org and gitlab.com via a
// host-prefix table. NormalizeGitHubRepoInput is retained for the
// remaining github-only sites:
//
//   - cmd/signatory/handoff.go: handoff's --network-precheck calls
//     the github API directly and has no multi-forge equivalent yet.
//   - internal/signal/openssf: OpenSSF Scorecard only ranks github
//     repos, so the collector gates with isGitHubHost upstream and
//     uses the github-only normalizer for owner/repo extraction.
//   - internal/mcp/tools: target normalization for MCP-surface
//     callers; multi-forge generalization is a planned follow-up.
//
// New code that wants multi-forge classification should call
// NormalizeForgeRepoInput instead.
func NormalizeGitHubRepoInput(input string) (uri, owner, name string, err error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", "", "", fmt.Errorf("empty input")
	}

	// Strip common prefixes.
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")
	s = strings.TrimPrefix(s, "www.")
	s = strings.TrimPrefix(s, "github.com/") // HTTPS path form
	s = strings.TrimPrefix(s, "github.com:") // SSH form (git@ stripped above, leaving github.com:owner/repo)

	// Strip `.git` suffix and any trailing slash.
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")

	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("cannot parse GitHub repo from %q: expected owner/repo form", input)
	}

	// Take the first two path segments. Allow trailing path segments
	// (e.g., a URL like github.com/owner/repo/pull/42 — we just want
	// owner/repo here).
	owner = parts[0]
	name = parts[1]

	if owner == "" || name == "" {
		return "", "", "", fmt.Errorf("cannot parse GitHub repo from %q: empty owner or name", input)
	}
	if !validPathSegment.MatchString(owner) {
		return "", "", "", fmt.Errorf("invalid GitHub owner %q in input %q", owner, input)
	}
	if !validPathSegment.MatchString(name) {
		return "", "", "", fmt.Errorf("invalid GitHub repo name %q in input %q", name, input)
	}

	return CanonicalRepoURI(PlatformGitHub, owner, name), owner, name, nil
}

// forgeHostPrefix maps a recognized forge host prefix (path or SCP
// form, terminated by '/' or ':') to the canonical platform identifier
// CanonicalRepoURI expects. Listed in the order the gate uses; the
// path form appears before the SCP form per host so the path-form
// strip wins for inputs that lack a `git@` scheme prefix.
//
// This table is the single source of truth NormalizeForgeRepoInput
// dispatches on. Adding a forge means adding entries here AND opening
// the URL gate (rejectUnrecognizedForgeURL + the matching is*URL helper) to
// admit the host.
var forgeHostPrefix = []struct {
	prefix   string
	platform string
}{
	{"github.com/", PlatformGitHub},
	{"github.com:", PlatformGitHub},
	{"codeberg.org/", PlatformCodeberg},
	{"codeberg.org:", PlatformCodeberg},
	{"gitlab.com/", PlatformGitLab},
	{"gitlab.com:", PlatformGitLab},
}

// NormalizeForgeRepoInput parses user-supplied input that refers to a
// repository on a recognized forge (GitHub, Codeberg, GitLab) and
// returns its canonical URI plus the detected platform, owner, and
// repo name. Generalizes NormalizeGitHubRepoInput across forges:
//
//	alecthomas/kong                          → repo:github/alecthomas/kong (default)
//	github.com/alecthomas/kong               → repo:github/alecthomas/kong
//	https://github.com/alecthomas/kong       → repo:github/alecthomas/kong
//	https://codeberg.org/forgejo/forgejo     → repo:codeberg/forgejo/forgejo
//	https://gitlab.com/gitlab-org/gitlab     → repo:gitlab/gitlab-org/gitlab
//	git@codeberg.org:forgejo/forgejo.git     → repo:codeberg/forgejo/forgejo
//
// Platform detection is host-anchored against the forgeHostPrefix
// table. Bare "owner/repo" with no host defaults to PlatformGitHub
// for backward compatibility with signatory's long-standing CLI
// shorthand (the original /analyze invocations were github-only).
//
// Unrecognized hosts must already have been rejected by the URL gate
// (rejectUnrecognizedForgeURL); reaching this function with such input would
// fall through to the bare-shorthand branch and produce a misleading
// repo:github/<host>/<owner> URI. The gate exists precisely to
// prevent that regression.
//
// Returns owner and name in the user-typed casing; the canonical URI
// is already case-folded via CanonicalRepoURI. Callers that need the
// canonical form use the URI; callers that want to surface the
// user-typed casing in error messages or display use the raw fields.
func NormalizeForgeRepoInput(input string) (uri, platform, owner, name string, err error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", "", "", "", fmt.Errorf("empty input")
	}

	// Strip scheme/auth prefixes. Order is irrelevant in practice —
	// "git@" and "https://" / "http://" are mutually exclusive shapes
	// — but keeping the strip order matches NormalizeGitHubRepoInput
	// so divergence between the two functions stays cosmetic rather
	// than behavioral.
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")
	s = strings.TrimPrefix(s, "www.")

	// Detect host via prefix-table lookup. The first match wins;
	// ordering in forgeHostPrefix is significant only between path
	// and SCP form per host, where either may legitimately match
	// after the scheme strip above.
	platform = PlatformGitHub
	for _, h := range forgeHostPrefix {
		if strings.HasPrefix(s, h.prefix) {
			s = s[len(h.prefix):]
			platform = h.platform
			break
		}
	}

	// Strip `.git` suffix and any trailing slash.
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")

	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return "", "", "", "", fmt.Errorf("cannot parse forge repo from %q: expected owner/repo form", input)
	}

	// Take the first two path segments — allow trailing segments
	// (e.g. .../owner/repo/pull/42).
	owner = parts[0]
	name = parts[1]

	if owner == "" || name == "" {
		return "", "", "", "", fmt.Errorf("cannot parse forge repo from %q: empty owner or name", input)
	}
	if !validPathSegment.MatchString(owner) {
		return "", "", "", "", fmt.Errorf("invalid forge owner %q in input %q", owner, input)
	}
	if !validPathSegment.MatchString(name) {
		return "", "", "", "", fmt.Errorf("invalid forge repo name %q in input %q", name, input)
	}

	return CanonicalRepoURI(platform, owner, name), platform, owner, name, nil
}

// SplitURIVersion splits a canonical URI into (base, version). The
// base is the URI without any `@version` suffix; the version is the
// suffix value or empty.
//
// This is the Plan-A canonicalization primitive: postures for
// `pkg:npm/X@V` are stored at the `pkg:npm/X` entity with the posture
// row's `version` column set to "V". `posture set`, `posture get`,
// `posture unset`, `posture accept`, and the summary assembler all
// route URIs through this helper before touching the store, so a
// query for `pkg:npm/X@V` and a query with `X --version V` both
// resolve to the same row.
//
// Rules (v0.1 grammar):
//   - pkg: URIs carry @version suffixes for ecosystem-native version
//     identifiers (e.g., pkg:npm/X@1.2.3, pkg:go/M@v1.0.0).
//   - repo: URIs carry @version suffixes naming a git ref (tag,
//     branch, or commit-shaped string) — used by signatory handoff
//     to clone the named ref instead of HEAD. Storage strips the
//     suffix; the entity is at the unversioned URI and the version
//     lives on the posture row.
//   - identity:, org:, patch: URIs pass through unchanged with
//     version="" — they don't have a version-of-an-identity concept.
//   - The `@` that separates name from version lives in the LAST
//     path segment. Scoped npm packages like `pkg:npm/@types/node`
//     have an `@` inside their name but in the FIRST path segment —
//     the scan is deliberately anchored to the last segment so the
//     scope `@` is not mistaken for a version separator.
//   - Inputs that don't start with pkg: or repo: pass through verbatim.
//
// Designed to be cheap — a few indexing operations on a single
// string, no allocations beyond the returned substrings.
// Deliberately does NOT validate that the input is a well-formed
// canonical URI; callers that need validation use
// ValidateCanonicalURI first.
func SplitURIVersion(uri string) (base, version string) {
	if !strings.HasPrefix(uri, URISchemePackage) &&
		!strings.HasPrefix(uri, URISchemeRepo) {
		return uri, ""
	}
	lastSlash := strings.LastIndexByte(uri, '/')
	if lastSlash < 0 {
		// Malformed URI with no path. Pass through rather than
		// synthesize a split that the caller would have to second-
		// guess.
		return uri, ""
	}
	name, version, ok := strings.Cut(uri[lastSlash+1:], "@")
	if !ok {
		return uri, ""
	}
	return uri[:lastSlash+1] + name, version
}
