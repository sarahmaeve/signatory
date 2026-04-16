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
// This file defines the URI schemes from design/entity-model-v2.md:
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
const (
	PlatformGitHub = "github"
	PlatformGitLab = "gitlab"
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
// Ecosystem is normalized to lowercase. Name is preserved as-is because
// most package ecosystems treat names case-sensitively.
func CanonicalPackageURI(ecosystem, name string) string {
	return URISchemePackage + strings.ToLower(ecosystem) + "/" + name
}

// CanonicalRepoURI returns the canonical URI for a source repository.
// Example: CanonicalRepoURI("github", "alecthomas", "kong") →
// "repo:github/alecthomas/kong".
//
// Platform is lowercased; owner and name are preserved as-is.
func CanonicalRepoURI(platform, owner, name string) string {
	return URISchemeRepo + strings.ToLower(platform) + "/" + owner + "/" + name
}

// CanonicalIdentityURI returns the canonical URI for a contributor
// identity. Example: CanonicalIdentityURI("github", "alecthomas") →
// "identity:github/alecthomas".
func CanonicalIdentityURI(platform, user string) string {
	return URISchemeIdentity + strings.ToLower(platform) + "/" + user
}

// CanonicalOrgURI returns the canonical URI for an organization.
// Example: CanonicalOrgURI("github", "stretchr") →
// "org:github/stretchr".
func CanonicalOrgURI(platform, name string) string {
	return URISchemeOrg + strings.ToLower(platform) + "/" + name
}

// CanonicalPatchURI returns the canonical URI for a patch (PR/MR).
// Example: CanonicalPatchURI("github", "alecthomas", "kong", "593") →
// "patch:github/alecthomas/kong/593".
func CanonicalPatchURI(platform, owner, repo, id string) string {
	return URISchemePatch + strings.ToLower(platform) + "/" + owner + "/" + repo + "/" + id
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
// This is intentionally GitHub-specific — other platforms will get
// their own Normalize*Input functions as they're added.
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
	s = strings.TrimPrefix(s, "github.com/")  // HTTPS path form
	s = strings.TrimPrefix(s, "github.com:")  // SSH form (git@ stripped above, leaving github.com:owner/repo)

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
