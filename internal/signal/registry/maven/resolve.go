package maven

import (
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// NormalizeDeclaredRepoURL reduces a raw SCM URL extracted from a
// maven POM's <scm><url> or <scm><connection> element to signatory's
// canonical https://<forge>/<owner>/<name> form. Returns empty
// string for inputs that don't map to a first-classed forge
// (github.com, codeberg.org, gitlab.com) — the unambiguous "no
// source signatory can clone" signal.
//
// Maven SCM URLs come in several shapes in practice:
//
//	https://github.com/owner/repo                  (most common <url>)
//	https://github.com/owner/repo.git
//	git@github.com:owner/repo.git                  (SCP shorthand —
//	                                                in micrometer's <url>)
//	scm:git:https://github.com/owner/repo.git      (connection form)
//	scm:git:git@github.com:owner/repo.git
//	scm:svn:https://...                            (legacy)
//
// The function strips the maven-specific scm:git: / scm:svn:
// prefixes (parseSCMURL already strips them from <connection>; this
// re-stripping is defensive for the case of <url> elements that
// inadvertently carry the prefix, and idempotent when the prefix is
// absent), then delegates to profile.ResolveTarget for the canonical
// forge-URL grammar.
//
// The SCP-shorthand case is the regression this function exists to
// fix: parsing "git@github.com:owner/repo.git" through Go's net/url
// returns an error ("first path segment in URL cannot contain
// colon"), so the downstream safeGitCloneURL guard rejects the raw
// string outright. ResolveTarget's NormalizeForgeRepoInput admits
// the SCP form (strips git@, matches github.com: in forgeHostPrefix)
// and produces a clean https clone URL via CloneURLForRepoPlatform.
//
// Mirrors npm.NormalizeDeclaredRepoURL by delegating to ResolveTarget
// after stripping ecosystem-specific noise. Bringing maven into line
// with the npm/pypi/cargo/gem/golang pattern resolves a structural
// asymmetry where the maven analyze-flow resolver (resolveMavenRepo
// in cmd/signatory/analyze.go) stamped raw POM strings on entity.URL
// without normalization.
func NormalizeDeclaredRepoURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}

	// Strip maven SCM provider prefixes. parseSCMURL already strips
	// these from <connection>; re-stripping is idempotent and covers
	// the case of a POM that puts the prefix inside <url>.
	s = strings.TrimPrefix(s, "scm:git:")
	s = strings.TrimPrefix(s, "scm:svn:")

	resolved, err := profile.ResolveTarget(s)
	if err != nil {
		return ""
	}
	if resolved.CloneURL == "" {
		return ""
	}
	return resolved.CloneURL
}
