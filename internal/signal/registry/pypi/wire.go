package pypi

// Project models the subset of PyPI's /pypi/<name>/json response
// signatory's Layer 6 source-resolution slice reads. The legacy
// JSON endpoint also returns "releases" (the full historical
// version map, multi-MB for popular packages) and "urls" (the
// latest release's distribution files); both are deliberately
// unmodelled here — the json package skips unknown fields by
// default, so they're decoded but not allocated.
//
// When Layer 5's signal collector lands it will extend this file
// with Releases / Distribution / Vulnerabilities — additive only,
// no shape changes to the existing fields, so commit 5's resolver
// stays stable across the v0.1 → Layer 5 transition.
type Project struct {
	Info Info `json:"info"`
}

// Info is the project-level metadata block. Modelled today:
//
//   - ProjectURLs: free-form publisher-supplied map. Keys vary
//     wildly (Repository, Source, Source Code, Homepage, Code,
//     GitHub, Repo, …); the priority lookup in resolve.go walks a
//     fixed key order to pick the most-likely-correct repo URL.
//   - HomePage: the deprecated PEP 621 predecessor of project_urls.
//     Still populated on older releases and used as the final
//     fallback when no project_urls key resolves.
//   - Author / AuthorEmail / Maintainer / MaintainerEmail: legacy
//     PEP 621 single-string fields. Publisher-supplied free text:
//     historically a comma-separated list of human-readable names
//     ("Saurabh Kumar" or "Some Person, Other Person") with optional
//     <email@addr> wrappers. The collector parses these
//     conservatively for publisher-entity minting (collector.go,
//     extractPyPILogins) — login-shaped values only, free-text
//     display names are rejected.
//   - Maintainers: PEP 639 / Trove-style multi-maintainer list. Each
//     entry is a {name, email} object. Newer registry responses
//     populate this; legacy responses leave it nil and use the
//     single-string Maintainer field above.
//
// Other fields the full Layer 5 collector will eventually want
// (requires_python, license, version, downloads, …) land here
// additively when those signals come online.
type Info struct {
	ProjectURLs     map[string]string `json:"project_urls"`
	HomePage        string            `json:"home_page"`
	Author          string            `json:"author"`
	AuthorEmail     string            `json:"author_email"`
	Maintainer      string            `json:"maintainer"`
	MaintainerEmail string            `json:"maintainer_email"`
	Maintainers     []Person          `json:"maintainers"`
}

// Person models one entry in PyPI's PEP 639-style maintainers /
// authors list (the multi-entry parallel to the legacy single-string
// Author / Maintainer fields). Both fields are publisher-supplied;
// Name is the conventional carrier of the registry login when the
// publisher chose to use one rather than a display name. The
// collector applies the same login-shape filter as for the legacy
// fields (extractPyPILogins).
type Person struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}
