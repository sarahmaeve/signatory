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

// Info is the project-level metadata block. Only the fields that
// inform source resolution are modelled today:
//
//   - ProjectURLs: free-form publisher-supplied map. Keys vary
//     wildly (Repository, Source, Source Code, Homepage, Code,
//     GitHub, Repo, …); the priority lookup in resolve.go walks a
//     fixed key order to pick the most-likely-correct repo URL.
//   - HomePage: the deprecated PEP 621 predecessor of project_urls.
//     Still populated on older releases and used as the final
//     fallback when no project_urls key resolves.
//
// Other fields the collector will eventually need (author,
// requires_python, license, version) land here additively when
// Layer 5 wires them up.
type Info struct {
	ProjectURLs map[string]string `json:"project_urls"`
	HomePage    string            `json:"home_page"`
}
