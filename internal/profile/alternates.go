package profile

import (
	"slices"
	"strings"
)

// AlternateURIs returns canonical-URI forms equivalent to canonical,
// in preference order. The first element is canonical itself;
// subsequent elements are forms that may have been recorded under a
// different writer's convention or by an analyst's preferred URI shape.
//
// Used by lookup-side helpers (LookupEntity, the MCP summary tool, the
// survey resolver) to find an entity even when the caller's URI form
// disagrees with the form the store row was written under.
//
// The list is finite, deterministic, and performs no I/O. Equivalences
// covered:
//
//   - Version stripping: foo@V ↔ foo. Plan-A canonicalization stores
//     entities at the unversioned base; pre-v10 stores fragmented by
//     version, so the @V form is also worth trying as a historical
//     leftover (e.g. testify's M1 violation).
//
//   - Cross-scheme github: pkg:go/github.com/X ↔ pkg:golang/github.com/X
//     ↔ repo:github/X. Three writers in v0.1 produce these three
//     equivalent forms — the gomod parser writes pkg:go/, analysts
//     following the purl spec write pkg:golang/, and direct repo URI
//     callers write repo:github/. All three refer to the same entity.
//
//   - pkg:go ↔ pkg:golang for non-github paths (vanity hosts):
//     gomod parser writes pkg:go/, the purl spec uses pkg:golang/.
//     Pure prefix swap; no further resolution.
//
//   - golang.org/x organizational vanity: pkg:go/golang.org/x/Y and
//     pkg:golang/golang.org/x/Y both resolve to repo:github/golang/Y.
//     Specific to golang.org/x because that's the most common
//     organizational-vanity host signal-collection pipelines normalize.
//     Other vanity hosts (gopkg.in, modernc.org, k8s.io) are terminal:
//     the pkg:go/ form IS the canonical, no equivalent github form.
//
// Empty input returns nil.
func AlternateURIs(canonical string) []string {
	if canonical == "" {
		return nil
	}

	base, version := SplitURIVersion(canonical)

	// Step 1: build the equivalence set on BASE URIs only. The
	// versioned forms get applied uniformly to every base alternate
	// at the end, so this stage stays pure-string and easy to reason
	// about.
	bases := []string{base}
	add := func(uri string) {
		if !slices.Contains(bases, uri) {
			bases = append(bases, uri)
		}
	}

	if rest, ok := strings.CutPrefix(base, "pkg:go/github.com/"); ok {
		add("repo:github/" + rest)
		add("pkg:golang/github.com/" + rest)
	} else if rest, ok := strings.CutPrefix(base, "pkg:golang/github.com/"); ok {
		add("repo:github/" + rest)
		add("pkg:go/github.com/" + rest)
	} else if rest, ok := strings.CutPrefix(base, "repo:github/"); ok {
		add("pkg:go/github.com/" + rest)
		add("pkg:golang/github.com/" + rest)
	} else if rest, ok := strings.CutPrefix(base, "pkg:go/"); ok {
		// Non-github pkg:go/ (vanity host). Add pkg:golang/ as the
		// purl-spec equivalent.
		add("pkg:golang/" + rest)
	} else if rest, ok := strings.CutPrefix(base, "pkg:golang/"); ok {
		add("pkg:go/" + rest)
	}

	// Step 2: golang.org/x organizational vanity. Apply over the
	// current bases so we catch both pkg:go/golang.org/x/Y and the
	// pkg:golang/golang.org/x/Y form added in step 1. Iterate over a
	// snapshot so the append doesn't visit the freshly-added github
	// form (which has its own equivalences but isn't a golang.org/x
	// path so the inner check skips it anyway).
	for _, b := range append([]string{}, bases...) {
		for _, prefix := range []string{"pkg:go/golang.org/x/", "pkg:golang/golang.org/x/"} {
			if rest, ok := strings.CutPrefix(b, prefix); ok && rest != "" {
				add("repo:github/golang/" + rest)
			}
		}
	}

	// Step 3: assemble the final ordered list. Each base alternate
	// produces one entry; if the input had a version, also produce
	// the @V form right after the base form so callers iterating in
	// order try the most-likely-canonical (Plan-A unversioned base)
	// first while still reaching pre-v10 versioned-entity rows.
	//
	// Preference order:
	//
	//   1. Input verbatim (with @V if present).
	//   2. Input's base (if version was stripped).
	//   3. Each cross-scheme/cross-prefix base alternate, in the
	//      order step 1/2 added them.
	//   4. The versioned form of each cross-scheme alternate, when
	//      input had @V.
	out := []string{canonical}
	if version != "" {
		out = append(out, base)
	}
	// bases[0] is `base`, already covered above.
	out = append(out, bases[1:]...)
	if version != "" {
		for _, b := range bases[1:] {
			out = append(out, b+"@"+version)
		}
	}

	// Dedupe while preserving order. Cheap O(n^2) scan is fine for
	// the small lists this function produces (typically 3–8 entries).
	deduped := make([]string, 0, len(out))
	seen := map[string]bool{}
	for _, uri := range out {
		if !seen[uri] {
			seen[uri] = true
			deduped = append(deduped, uri)
		}
	}
	return deduped
}
