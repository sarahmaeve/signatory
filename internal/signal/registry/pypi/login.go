package pypi

import (
	"regexp"
	"strings"
)

// pypiLoginPattern matches values that COULD plausibly be a PyPI
// account login. PyPI's signup form constrains usernames to
// alphanumeric plus hyphen/underscore/period, with an alphanumeric
// first character; 50 chars is a comfortable upper bound (PyPI's
// own limit is higher, but anything close to that ceiling looks
// more like a sentence than a login).
//
// The pattern is deliberately stricter than the conceivable
// "anything that fits in a URL path segment" — we'd rather refuse
// to mint than mint identity:pypi/<display name> entities. PyPI's
// info.maintainer / info.author fields are publisher-supplied free
// text, not a structured login slot, so the extractor needs a
// pattern that distinguishes likely-login from likely-display-name.
//
// Real-world examples:
//
//	"ofek"          → accepted (login)
//	"konstin"       → accepted (login)
//	"alice_bob"     → accepted (login with underscore)
//	"Saurabh Kumar" → REJECTED (space → display name)
//	"alice@bob"     → REJECTED (@ → not in PyPI login charset)
//	""              → REJECTED (empty)
var pypiLoginPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,49}$`)

// extractPyPILogins parses the publisher-supplied identity strings
// from a PyPI Info block and returns the deduplicated, lowercased
// set of login-shaped values suitable for minting identity:pypi/
// <login> entity rows. Returns nil if no parseable login surfaces
// (so the caller can branch on len() without nil-handling).
//
// Field precedence:
//
//  1. info.maintainer — the conventional ownership-of-the-package
//     declaration. Wins over info.author when both are set
//     (governance preference for current ownership over original
//     authorship).
//  2. info.author — fallback when maintainer is empty. Original
//     authorship is the closest proxy for "who controls this" that
//     a legacy PyPI release exposes.
//  3. info.maintainers — PEP 639 multi-entry list. Always folded in
//     (a publisher who declared both a single maintainer string AND
//     a multi-entry list intends both to be present).
//
// Each candidate string is processed by:
//
//  1. Trim whitespace.
//  2. If "<" appears, take the substring before it (strips
//     "Name <email>" wrappers; the LHS is what registry tools
//     historically displayed as the publisher login).
//  3. Trim again (handles "ofek " before "<").
//  4. Reject if not pypiLoginPattern.
//  5. Lowercase.
//  6. Dedupe across all sources.
//
// Comma-separated input (a legacy convention from setup.py-style
// metadata where one field carried multiple names) is split on
// commas before per-token processing — so "alice, bob, John Smith"
// produces "alice" and "bob" but skips the display name.
//
// nil-safe: a nil *Info returns nil.
func extractPyPILogins(info *Info) []string {
	if info == nil {
		return nil
	}

	seen := map[string]struct{}{}
	var out []string

	add := func(raw string) {
		login, ok := normalizePyPILogin(raw)
		if !ok {
			return
		}
		if _, exists := seen[login]; exists {
			return
		}
		seen[login] = struct{}{}
		out = append(out, login)
	}

	// Pick the dominant single-string source: maintainer wins, then
	// author. Both are comma-separable in legacy metadata, so split
	// before per-token processing.
	primary := info.Maintainer
	if primary == "" {
		primary = info.Author
	}
	for part := range strings.SplitSeq(primary, ",") {
		add(part)
	}

	// PEP 639 list — always folded in (publishers can populate both
	// the single-string field and the list, and we want the union).
	for _, m := range info.Maintainers {
		add(m.Name)
	}

	return out
}

// normalizePyPILogin applies the "looks like a login" filter to one
// raw string and returns (canonical-lowercase-form, true) on match
// or ("", false) on rejection. Helper extracted from
// extractPyPILogins so the rules stay testable in isolation and the
// main extractor reads as a list-walk.
//
// Returns false for empty/whitespace, free-text display names
// (anything failing pypiLoginPattern), and values that — after
// stripping a "Name <email>" wrapper — leave nothing usable.
func normalizePyPILogin(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	// Strip a "Name <email>" wrapper. The "<" anchors the start of
	// the email portion in the conventional Python packaging form;
	// the LHS is the publisher-supplied "name" which is what we
	// might recognize as a login. If "<" appears at position 0 the
	// LHS is empty — fall through to the empty-trim check below.
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return "", false
	}
	if !pypiLoginPattern.MatchString(s) {
		return "", false
	}
	return strings.ToLower(s), true
}
