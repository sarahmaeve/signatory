package npm

import (
	"context"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// ResolveRepoURL queries the npm registry for the package's
// declared repository URL and normalizes it to a github-cloneable
// https URL. Returns empty string (not error) when:
//
//   - the registry doesn't declare a repository URL;
//   - the declared URL doesn't resolve to github.com;
//   - the URL is malformed.
//
// Returns an error only on fetch failure (network, 404, size-cap,
// etc.) — empty string is the correct "not resolvable" signal,
// distinct from "couldn't reach the registry."
//
// By design, repo resolution lives in analyze.go's orchestration,
// not inside the signal collector. This method is the provider-side
// answer the orchestrator calls; the orchestrator stamps the result
// on the entity; downstream collectors (github, git-local-clone)
// operate against the resolved entity without needing to know about
// npm.
func (c *Client) ResolveRepoURL(ctx context.Context, name string) (string, error) {
	pkg, err := c.GetPackage(ctx, name)
	if err != nil {
		return "", err
	}
	return NormalizeDeclaredRepoURL(pkg.Repository.URL), nil
}

// NormalizeDeclaredRepoURL converts an npm registry repository.url
// value into a github-cloneable https URL, or returns empty string
// if the input doesn't resolve to a github URL.
//
// npm packages declare repository URLs in several shapes:
//
//	"git+https://github.com/owner/repo.git"   (most common)
//	"git+ssh://git@github.com/owner/repo.git"
//	"https://github.com/owner/repo.git"
//	"https://github.com/owner/repo"
//	"github:owner/repo"                       (npm shorthand)
//	"git://github.com/owner/repo.git"         (legacy, upgraded)
//
// The function reduces each form to https://github.com/owner/repo
// by delegating to profile.ResolveTarget for the canonical github
// URL grammar, after stripping npm-specific prefixes and shorthand.
// Non-github hosts and unrecognized forms return empty string,
// which is the unambiguous "no repo to resolve" signal.
//
// git:// on github.com is upgraded to https: the scheme is
// plaintext, but the host anchors the identity, and downstream
// collectors hit https regardless. A long tail of older packages
// (image-size is one) declared git://github.com/... once a decade
// ago and never updated it; refusing the whole URL because of the
// scheme threw out a valid identity. git:// on any other host
// stays refused — the upgrade only works because github.com serves
// the same identity over https.
func NormalizeDeclaredRepoURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// npm shorthand: "github:owner/repo" optionally followed by
	// "#branch" or "#commit". The fragment is dropped — it's a
	// subpath hint, not part of the repo identity.
	if rest, ok := strings.CutPrefix(s, "github:"); ok {
		s = "https://github.com/" + rest
	}

	// Strip the "git+" prefix common in npm's repository.url.
	s = strings.TrimPrefix(s, "git+")

	// git:// is unauthenticated plaintext. On github.com we upgrade
	// to https because the host anchors the identity and we never
	// actually clone over git:// downstream. On any other host we
	// refuse — see function doc. The host check is anchored on the
	// path separator to block lookalikes like git://github.com.evil/.
	if rest, ok := strings.CutPrefix(s, "git://github.com/"); ok {
		s = "https://github.com/" + rest
	} else if strings.HasPrefix(s, "git://") {
		return ""
	}

	// Drop URL fragments (e.g., "#main" for a pinned branch).
	if before, _, ok := strings.Cut(s, "#"); ok {
		s = before
	}

	// Drop ssh:// if present and not git@-form — npm does emit
	// "ssh://git@github.com/owner/repo.git" rarely; convert to the
	// https form since we only clone over https.
	if rest, ok := strings.CutPrefix(s, "ssh://git@github.com"); ok {
		s = "https://github.com" + rest
	}

	// Delegate to ResolveTarget for the actual github-URL grammar
	// (handles https://, http://, www., .git suffix, git@... and
	// shorthand). Non-github hosts get rejected with a clear error
	// we map to "return empty string."
	resolved, err := profile.ResolveTarget(s)
	if err != nil {
		return ""
	}
	if resolved.CloneURL == "" || resolved.Platform != profile.PlatformGitHub {
		return ""
	}
	return resolved.CloneURL
}
