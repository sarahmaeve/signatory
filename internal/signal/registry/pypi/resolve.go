package pypi

import (
	"context"
)

// projectURLPriority is the fixed lookup order for resolving a
// publisher's declared source repository from the free-form
// info.project_urls map. The list captures every key signatory
// has observed in real PyPI projects (Repository being the PEP
// 621-recommended canonical key) plus the common variants Poetry
// and other tooling emit.
//
// Order matters: when a project sets multiple keys (e.g.,
// "Repository" → the actual repo, "Homepage" → a docs site), the
// first match wins. "Homepage" is intentionally last among
// project_urls keys — it's the most semantically loose, so the
// other keys take precedence when present. info.home_page (the
// deprecated PEP 621 predecessor) is the FINAL fallback after
// every project_urls key, since it's only populated on legacy
// releases and the project_urls map is the modern source of
// truth.
//
// Casing in the keys is preserved exactly as PyPI publishers type
// them — the registry stores keys verbatim, so case-insensitive
// matching would over-merge legitimately-distinct keys (e.g., a
// project that intentionally uses "source" for one URL and
// "Source" for another).
var projectURLPriority = []string{
	"Repository",
	"Source",
	"Source Code",
	"SourceCode",
	"source",
	"Code",
	"GitHub",
	"Repo",
	"Homepage",
}

// ResolveRepoURL fetches the project's metadata, walks the
// project_urls priority list to find a declared source repository,
// normalizes it to a github-cloneable https URL, and returns it.
//
// Returns ("", nil) when:
//
//   - the project's info has no project_urls and no home_page;
//   - none of the project_urls keys (in priority order) carry a
//     value that normalizes to a github URL;
//   - the deprecated home_page fallback also doesn't normalize.
//
// Returns an error only on fetch failure (network, 404, body
// cap). Empty string is the unambiguous "no resolvable github
// source" signal — distinct from "couldn't reach the registry,"
// which the caller (signatory's resolver registry) routes as
// transient.
//
// This is the function signatory's Layer 6 PyPI resolver wraps.
// LLM analyst agents previously discovered this URL by hand-walking
// pypi.org via WebFetch; replacing that step with deterministic
// code is the entire point of the v0.1 source-resolution slice.
func (c *Client) ResolveRepoURL(ctx context.Context, name string) (string, error) {
	info, err := c.GetProjectInfo(ctx, name)
	if err != nil {
		return "", err
	}

	for _, key := range projectURLPriority {
		raw, ok := info.ProjectURLs[key]
		if !ok || raw == "" {
			continue
		}
		if normalized := NormalizeDeclaredRepoURL(raw); normalized != "" {
			return normalized, nil
		}
	}

	// Final fallback: the deprecated info.home_page field. Some
	// older PyPI releases populate ONLY this — see
	// TestResolveRepoURL_HomePageFallback for the legacy shape.
	if normalized := NormalizeDeclaredRepoURL(info.HomePage); normalized != "" {
		return normalized, nil
	}

	return "", nil
}
