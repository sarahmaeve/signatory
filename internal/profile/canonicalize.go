package profile

import "strings"

// CanonicalizeURI returns the canonical form of a non-canonical URI,
// or the input verbatim when it's already canonical. Used by
// `signatory prune duplicates` to decide which entity rows to merge
// or rename — every fragmentation class boils down to "this URI's
// canonical form is X."
//
// Three classes handled:
//
//  1. Case fragmentation: repo:/identity:/org:/patch: schemes whose
//     constructors lowercase platform/owner/name. A non-lowercase
//     stored URI gets folded to its lowercase canonical.
//
//  2. Ecosystem prefix: pkg:go/<path> → pkg:golang/<path>. The "go"
//     ecosystem identifier was a 2026-04-20 internal coining;
//     "golang" is the [purl spec](https://github.com/package-url/purl-spec)
//     type. New writers emit pkg:golang/; pre-existing pkg:go/ rows
//     consolidate here.
//
//  3. Versioned-entity (M1 violation): <base>@V → <base>. Plan-A
//     canonicalization stores entities at the unversioned base URI
//     with the version captured on the analyst_output row's
//     target_version column. Pre-v10 ingest paths sometimes wrote
//     @V into entity rows; those consolidate to the base. Scoped
//     npm packages (pkg:npm/@scope/name) are NOT versioned-entities
//     — the @ in scoped names lives in the FIRST path segment, not
//     the last, so SplitURIVersion correctly leaves them alone.
//
// Returns input verbatim when no class applies. Returns "" for
// empty input.
//
// Composition order: case-fold first, then ecosystem-prefix, then
// version-strip. Each transform may unlock the next — e.g.
// pkg:go/github.com/Owner/Repo@v1 → after ecosystem-prefix
// pkg:golang/github.com/Owner/Repo@v1 → after version-strip
// pkg:golang/github.com/Owner/Repo. Case-fold for pkg:golang/github
// paths is not currently applied here (the constructors that
// produce pkg:golang/ don't case-fold today either; if that becomes
// a separate fragmentation class with its own dogfood evidence,
// add a fourth transform).
func CanonicalizeURI(uri string) string {
	if uri == "" {
		return ""
	}

	out := uri

	// Step 1: case-fold for repo:/identity:/org:/patch: schemes whose
	// constructors lowercase. Use ResolveTarget to do the heavy lifting:
	// it already case-folds canonical-shaped repo URIs (added earlier
	// for the BurntSushi/toml dogfood). Defensive: only consume the
	// resolver's output when the scheme is one we trust to canonicalize.
	for _, prefix := range []string{"repo:", "identity:", "org:", "patch:"} {
		if strings.HasPrefix(out, prefix) {
			if resolved, err := ResolveTarget(out); err == nil && resolved.CanonicalURI != "" {
				out = resolved.CanonicalURI
			}
			break
		}
	}

	// Step 2: pkg:go → pkg:golang. Apply BEFORE version-strip so the
	// transformed URI flows through SplitURIVersion in step 3.
	if rest, ok := strings.CutPrefix(out, "pkg:go/"); ok {
		out = "pkg:golang/" + rest
	}

	// Step 3: strip @V from entity URIs (Plan-A). SplitURIVersion
	// correctly leaves scoped npm packages alone (the @ in @scope/name
	// is in the first segment, not the last).
	base, _ := SplitURIVersion(out)
	if base != out {
		out = base
	}

	return out
}
