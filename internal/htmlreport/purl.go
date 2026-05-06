package htmlreport

import (
	"net/url"
	"strings"
)

// PURLToRegistryURL maps a canonical signatory URI ("pkg:pypi/dark-matter",
// "repo:github/owner/name") to the human-facing registry URL the
// metadata block links to. Returns "" for URIs we don't have a
// mapping for (identity:, org:, patch:, unknown ecosystems) — the
// caller renders the URI as plain text in that case.
//
// The inverse of internal/profile/target.go's URL → PURL parsing.
// Kept local to htmlreport so the renderer doesn't grow a profile
// dependency for one cosmetic feature; if a third caller needs the
// same mapping later, hoist it.
//
// Version suffix handling: "@VERSION" is stripped before lookup and
// re-attached to the URL for ecosystems whose registry pages support
// version-specific URLs (PyPI, npm, crates.io, RubyGems). For
// ecosystems where the version segment is awkward to represent
// (golang, maven), the unversioned page is returned and the version
// is dropped silently.
func PURLToRegistryURL(uri string) string {
	if uri == "" {
		return ""
	}

	// Repo: URIs map to their hosting platform. Only github is wired
	// up in v0.1; gitlab/bitbucket can land here when collectors do.
	if rest, ok := strings.CutPrefix(uri, "repo:github/"); ok {
		return "https://github.com/" + escapePathSegments(rest)
	}

	body, ok := strings.CutPrefix(uri, "pkg:")
	if !ok {
		return ""
	}

	// Split off optional @VERSION suffix. PURL puts the version after
	// the name, so the version '@' is the *last* '@' AND it must
	// come after the last '/' — otherwise a leading '@' belongs to
	// an npm scope prefix ("@types/node") and isn't a version
	// separator at all.
	var version string
	at := strings.LastIndex(body, "@")
	slash := strings.LastIndex(body, "/")
	if at > slash {
		version = body[at+1:]
		body = body[:at]
	}

	// Split ecosystem from name.
	ecosystem, name, ok := strings.Cut(body, "/")
	if !ok {
		return ""
	}

	switch ecosystem {
	case "pypi":
		return joinSlash("https://pypi.org/project", escapePathSegments(name), version) + "/"
	case "npm":
		// npm scoped packages keep the @-prefix on the name segment;
		// the version sits separately. "https://www.npmjs.com/package/<name>/v/<version>"
		// is the version-specific URL.
		base := "https://www.npmjs.com/package/" + escapePathSegments(name)
		if version != "" {
			return base + "/v/" + url.PathEscape(version)
		}
		return base
	case "cargo":
		base := "https://crates.io/crates/" + escapePathSegments(name)
		if version != "" {
			return base + "/" + url.PathEscape(version)
		}
		return base
	case "gem":
		base := "https://rubygems.org/gems/" + escapePathSegments(name)
		if version != "" {
			return base + "/versions/" + url.PathEscape(version)
		}
		return base
	case "maven":
		// Maven names are <group>/<artifact>; central.sonatype.com
		// uses /artifact/<group>/<artifact>. Version path differs
		// across UIs; v0.1 returns the unversioned page.
		group, artifact, ok := strings.Cut(name, "/")
		if !ok {
			return ""
		}
		return "https://central.sonatype.com/artifact/" +
			escapePathSegments(group) + "/" + escapePathSegments(artifact)
	case "golang":
		// pkg.go.dev uses the import path verbatim.
		base := "https://pkg.go.dev/" + escapePathSegments(name)
		if version != "" {
			return base + "@" + url.PathEscape(version)
		}
		return base
	default:
		return ""
	}
}

// escapePathSegments percent-encodes each '/' separated segment of
// path while preserving the slashes. Plain url.PathEscape would
// escape the slashes themselves, breaking npm scoped names like
// "@types/node" → "@types%2Fnode" (npmjs.com expects the slash).
func escapePathSegments(p string) string {
	if p == "" {
		return ""
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

// joinSlash joins non-empty parts with single slashes. Convenience
// for assembling registry URLs that may or may not include a
// version segment.
func joinSlash(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "/") {
			b.WriteByte('/')
		}
		b.WriteString(p)
	}
	return b.String()
}
