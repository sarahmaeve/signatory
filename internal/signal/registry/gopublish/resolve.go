package gopublish

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// ResolveRepoURL returns the canonical github.com clone URL for a Go
// module path, or empty string if the module can't be resolved to a
// github source.
//
// Resolution chain (in order):
//
//  1. Query proxy.golang.org for @latest to get the current version.
//  2. Query @v/<version>.info for the Origin block. If Origin.URL
//     resolves to github via profile.ResolveTarget, return that.
//  3. If proxy returns 404, OR returns a version-info with no Origin
//     block (pre-Go-1.20 publish), OR returns an Origin URL that
//     doesn't resolve as github: fall back to fetching the vanity
//     host's go-import meta tag at <modulePath>?go-get=1 and
//     extracting the repo root.
//  4. The meta-tag's importPrefix MUST equal or be a prefix of the
//     requested module path (cross-origin attribution defense).
//  5. The meta-tag's repoRoot MUST resolve as github via
//     profile.ResolveTarget; non-github roots are dropped.
//  6. If neither the proxy nor the meta-tag yields a github URL,
//     return empty string. Callers map empty to absence:
//     repo_declaration.
//
// Returns an error only when the PROXY responds with something other
// than 200 or 404 — a 5xx, network, or decode error. Proxy 5xx does
// NOT trigger meta-tag fallback (the module probably exists; this is
// transient infrastructure failure, retry later).
//
// Module path is validated by the underlying GetLatest /
// GetVersionInfo calls and by the meta-tag URL builder's path
// containment. Callers need not pre-sanitize.
func (c *Client) ResolveRepoURL(ctx context.Context, modulePath string) (string, error) {
	// Step 1+2: try proxy.
	if url, decided, err := c.resolveViaProxy(ctx, modulePath); err != nil {
		return "", err
	} else if decided {
		return url, nil
	}

	// Step 3: proxy was inconclusive (404 or no Origin block or
	// non-github Origin). Fall back to meta-tag fetch.
	return c.resolveViaMetaTag(ctx, modulePath)
}

// resolveViaProxy queries the proxy for @latest + .info and returns:
//
//   - (url, true, nil): proxy gave a github URL → return it directly.
//   - ("", false, nil): proxy returned 404 OR no Origin block OR a
//     non-github Origin. Caller should fall back to meta tag.
//   - ("", false, err): proxy 5xx / network / decode error. Caller
//     surfaces this as a wrapped error; no meta-tag fallback.
//
// The decided bool distinguishes "definite answer (use this URL)"
// from "inconclusive (try the next path)" — important because both
// states are no-error but produce different caller behavior.
func (c *Client) resolveViaProxy(ctx context.Context, modulePath string) (string, bool, error) {
	latest, err := c.GetLatest(ctx, modulePath)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", false, nil // inconclusive — fall back
		}
		return "", false, fmt.Errorf("resolve repo URL for %q: %w", modulePath, err)
	}

	info, err := c.GetVersionInfo(ctx, modulePath, latest.Version)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve repo URL for %q: %w", modulePath, err)
	}

	if info.Origin.URL == "" {
		return "", false, nil // inconclusive — fall back
	}

	// Reduce the Origin URL to the canonical https://<host>/<owner>/<repo>
	// form via the CLI-wide target parser. First-classed forges
	// (github, gitlab, codeberg) pass; non-first-classed origins
	// (bitbucket, self-hosted GHE, unparseable URLs) get rejected
	// here, because stamping an unsupported URL would trip
	// isGitHostedEntity into a false positive followed by a
	// forge-API call against a host we have no collector for.
	url, ok := canonicalForgeURL(info.Origin.URL)
	if !ok {
		// Origin URL is on a forge signatory hasn't first-classed yet
		// (or is unparseable). The proxy was authoritative; we don't
		// fall back to the meta tag in this case because the proxy
		// already named the repo and saying "not a supported forge" —
		// the meta tag wouldn't change that answer.
		return "", true, nil
	}
	return url, true, nil
}

// resolveViaMetaTag fetches <modulePath>?go-get=1 from the vanity
// host, parses the go-import meta tag, and returns the github URL
// it points at — or empty if no valid meta tag is present.
//
// The cross-origin defense: the meta tag's importPrefix (first
// content field) must equal the requested modulePath, or the
// modulePath must start with importPrefix + "/" (the vanity host
// declares the module root, the user asked about a subpackage).
// Mismatched importPrefix is treated as not-resolvable.
//
// Returns an error only on transport failure (network, body cap,
// timeout). HTTP 4xx/5xx from the vanity host map to "not
// resolvable, not an error" — the vanity host's response is
// advisory; signatory's posture toward the module is just "no
// resolution available."
func (c *Client) resolveViaMetaTag(ctx context.Context, modulePath string) (string, error) {
	if err := ValidateModulePath(modulePath); err != nil {
		return "", fmt.Errorf("resolve via meta tag: %w", err)
	}

	// metaTagAPI has no baseURL — we pass a fully-qualified URL
	// because each module maps to a different vanity host. httpx
	// still applies the redirect/timeout/size-cap defenses; the
	// only behavioral wrinkle is that ALL failures here
	// (transport, non-200 status, body cap) map to "not resolvable,
	// not an error." The vanity host's response is advisory; the
	// proxy already declined to provide an Origin, and surfacing
	// these errors would turn every offline run on a proxy-
	// incomplete module into a hard --refresh failure.
	body, err := c.metaTagAPI.Get(ctx, c.metaTagURL(modulePath))
	if err != nil {
		return "", nil //nolint:nilerr // Intentional — vanity-host failure is "not resolvable".
	}

	// Try go-import first. Github-hosted vanity (k8s.io, golang.org/x/...)
	// declares the github URL directly here.
	if importPrefix, vcs, repoRoot, ok := parseGoImportMeta(body); ok && vcs == "git" {
		if !modulePathMatchesPrefix(modulePath, importPrefix) {
			return "", nil
		}
		if u, ok := canonicalForgeURL(repoRoot); ok {
			return u, nil
		}
		// go-import's repoRoot isn't github (e.g., gopkg.in proxies
		// git itself instead of pointing at github). Fall through to
		// the go-source meta tag, which conventionally points at the
		// github mirror even when go-import doesn't.
	}

	// Fall back to go-source. Its directory and file fields are
	// templated URLs typically pointing at the canonical github
	// mirror — even for vanity hosts that proxy git themselves.
	// Example for gopkg.in/yaml.v3:
	//
	//	<meta name="go-source" content="gopkg.in/yaml.v3 _
	//	   https://github.com/go-yaml/yaml/tree/v3.0.1{/dir}
	//	   https://github.com/go-yaml/yaml/blob/v3.0.1{/dir}/{file}#L{line}">
	//
	// extractGitHubURLFromString reduces the templated form to the
	// canonical https://github.com/<owner>/<repo>.
	if importPrefix, _, dir, file, ok := parseGoSourceMeta(body); ok {
		if !modulePathMatchesPrefix(modulePath, importPrefix) {
			return "", nil
		}
		for _, candidate := range []string{dir, file} {
			if u := extractGitHubURLFromString(candidate); u != "" {
				return u, nil
			}
		}
	}

	return "", nil
}

// metaTagURL builds the GET URL for the vanity-host meta-tag fetch.
// In production (metaTagURLPrefix == ""), the URL is the live
// "https://<modulePath>?go-get=1". In tests, metaTagURLPrefix is set
// to an httptest server URL, and the fetch becomes
// "<metaTagURLPrefix>/<modulePath>?go-get=1" — keeping the rest of
// the path shape the same so the parser sees identical request
// patterns regardless of the test/prod split.
func (c *Client) metaTagURL(modulePath string) string {
	if c.metaTagURLPrefix != "" {
		return c.metaTagURLPrefix + "/" + modulePath + "?go-get=1"
	}
	return "https://" + modulePath + "?go-get=1"
}

// canonicalForgeURL returns the canonical https://<host>/<owner>/<repo>
// form if raw resolves to a first-classed forge (github / codeberg /
// gitlab) via profile.ResolveTarget, or ("", false) if it doesn't
// (forge not first-classed, malformed URL).
//
// Used by both the proxy and meta-tag resolution paths so they
// produce identical canonical output for the same upstream URL.
//
// Empty CloneURL is the canonical "not first-classed" signal —
// profile.CloneURLForRepoPlatform stamps non-empty only for the
// recognized forges, so a single CloneURL == "" check captures both
// "URL gate rejected" and "platform not yet wired."
func canonicalForgeURL(raw string) (string, bool) {
	resolved, err := profile.ResolveTarget(raw)
	if err != nil {
		return "", false
	}
	if resolved.CloneURL == "" {
		return "", false
	}
	return resolved.CloneURL, true
}

// modulePathMatchesPrefix reports whether modulePath equals
// importPrefix exactly OR starts with importPrefix + "/" (the
// subpackage case where a user asks about
// "k8s.io/client-go/tools/cache" and the vanity host declares
// "k8s.io/client-go" as the module root).
func modulePathMatchesPrefix(modulePath, importPrefix string) bool {
	if modulePath == importPrefix {
		return true
	}
	return strings.HasPrefix(modulePath, importPrefix+"/")
}
