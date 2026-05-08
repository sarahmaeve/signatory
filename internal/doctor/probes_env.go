package doctor

import (
	"strings"
)

// probeGitHubToken checks whether GITHUB_TOKEN is set. Without it,
// signatory's github collector still runs but most signals come back
// empty — QUICKSTART calls this out explicitly. Warn, not fail: the
// pipeline functions, the analysis is just thinner.
//
// Empty / whitespace-only values count as unset (a profile that
// exports `GITHUB_TOKEN=" "` behaves identically to unset for the
// API). The token value is NEVER echoed into Message or Fix —
// doctor reports get pasted into bug tickets and chat logs, and a
// real PAT leaking that way is the kind of credential disclosure
// we should not enable by accident.
func probeGitHubToken(r resolved) Result {
	if strings.TrimSpace(r.getenv("GITHUB_TOKEN")) == "" {
		return Result{
			Name:    "github-token",
			Status:  StatusWarn,
			Message: "GITHUB_TOKEN is not set; github collectors will return mostly empty signals",
			Fix:     "export GITHUB_TOKEN=<a github personal access token with public_repo scope>",
		}
	}
	return Result{
		Name:    "github-token",
		Status:  StatusOK,
		Message: "GITHUB_TOKEN is set",
	}
}

// probeMkcertOnPath checks whether mkcert is installed. mkcert is
// the dependency `signatory certs init` shells out to for the local
// CA. A user who already has NODE_EXTRA_CA_CERTS set up doesn't
// strictly need mkcert on PATH any more, so this is a warn rather
// than a fail — we want to flag the gap without blocking healthy
// setups that arrived at it some other way.
func probeMkcertOnPath(r resolved) Result {
	path, err := r.lookPath("mkcert")
	if err != nil {
		return Result{
			Name:    "mkcert-on-path",
			Status:  StatusWarn,
			Message: "mkcert not found on PATH",
			Fix:     "install mkcert (`brew install mkcert` on macOS) if you need to run `signatory certs init`",
		}
	}
	return Result{
		Name:    "mkcert-on-path",
		Status:  StatusOK,
		Message: path,
	}
}
