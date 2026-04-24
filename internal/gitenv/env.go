// Package gitenv provides the canonical sanitized environment slice
// for every git subprocess signatory spawns — production and test.
//
// # Design: deny-by-default, explicit re-admit
//
// Base state is absence. Any environment variable is assumed dangerous
// and stripped unless explicitly re-admitted. Specifically:
//
//   - EVERY variable whose name starts with "GIT_" is stripped.
//     Git's documented environment interface is a moving target —
//     2.5 added GIT_COMMON_DIR, 2.31 added GIT_CONFIG_COUNT /
//     GIT_CONFIG_KEY_* / GIT_CONFIG_VALUE_*, future releases will
//     add more — so naming specific vars produces a deny-list that's
//     structurally incomplete. Stripping the whole namespace is
//     complete by construction and needs no maintenance per release.
//
//   - EVERY variable whose name starts with "SSH_ASKPASS" is
//     stripped (SSH_ASKPASS, SSH_ASKPASS_REQUIRE). These name binaries
//     invoked for credential prompts; an attacker-controlled value
//     captures credentials mid-operation.
//
//   - The libcurl proxy-control variables are stripped by exact name
//     (case-insensitive): HTTP_PROXY, HTTPS_PROXY, ALL_PROXY, NO_PROXY.
//     These don't share a meaningful prefix with anything else and are
//     not GIT_-namespaced, but git's HTTPS transport is libcurl-backed
//     and libcurl honors them: an attacker-controlled HTTPS_PROXY
//     redirects every clone's HTTPS fetch to an intercepting proxy.
//     MITM requires the attacker to also compromise TLS trust (CA
//     trust store, user click-through), but even without TLS break,
//     the proxy sees request metadata and can silently fail-close to
//     DoS the clone. Both upper- and lower-case spellings are honored
//     by libcurl and therefore both are stripped.
//
//   - SSH_AUTH_SOCK and every other non-GIT / non-SSH_ASKPASS / non-
//     proxy variable passes through. Legitimate git operations need PATH (to find
//     ssh / credential helpers / git-* subcommand binaries), HOME
//     (~/.gitconfig / credential cache), USER (committer identity
//     fallback), SSL_CERT_FILE / CURL_CA_BUNDLE / REQUESTS_CA_BUNDLE
//     (custom CA roots), TMPDIR, TERM, LANG, LC_*, TZ, XDG_*, etc.
//     We don't enumerate these — the prefix-strip rule makes them
//     pass through by default.
//
//   - GIT_TERMINAL_PROMPT=0 is force-appended, guaranteeing
//     non-interactive behavior regardless of what else we stripped.
//
// # Why this shape, not a deny-list of named vars
//
// An enumerated deny-list requires knowing every dangerous name. The
// 2026-04-24 postmortem traced a shared-config corruption to test
// helpers that inherited GIT_DIR from a pre-commit hook; a subsequent
// audit found the deny-list also missed GIT_INDEX_FILE and
// GIT_COMMON_DIR, both of which a pre-commit hook also sets and both
// of which redirect git's operation independently of GIT_DIR. Every
// audit inherits the auditor's ignorance. The failure mode of a
// deny-list miss is silent — wrong signals, wrong config writes,
// no error. The failure mode of the prefix-strip is loud — if
// signatory ever legitimately needs a GIT_* var, git emits a clear
// "X not set" error and we re-admit it explicitly.
//
// Loud failure is strictly better than silent misbehavior for
// security-relevant code.
//
// # Trade-offs preserved on purpose
//
// HOME is passed through. Git reads ~/.gitconfig and ~/.git-credentials
// from the home directory; stripping HOME breaks legitimate operations
// (credential caching, per-user config). But this means a hostile
// HOME lets an attacker materialize an attacker-controlled .gitconfig
// that sets, e.g., credential.helper=/tmp/evil-helper or http.proxy=
// http://attacker. Under the signatory v0.1 threat model the operator
// owns the environment they launch signatory from; a hostile HOME
// indicates a prior-stage compromise outside signatory's defense
// perimeter. If that assumption ever loosens (e.g. signatory runs
// under another user's HOME, or in a multi-tenant CI context), revisit
// this choice and consider stripping HOME too.
//
// PATH is passed through for the same reason — git needs to find ssh,
// credential helpers, and git-* subcommand binaries. Same trust
// assumption as HOME.
//
// # What signatory gives up
//
// Callers of git subprocesses cannot inherit any user-set GIT_*
// variable. For signatory's use case this is fine, arguably required:
//
//   - Production code never creates commits; GIT_AUTHOR_* /
//     GIT_COMMITTER_* inheritance isn't needed.
//   - Production code talks to GitHub over HTTPS with standard
//     config; GIT_SSL_* / GIT_HTTP_* customizations aren't in scope.
//   - Tests that need identity or date overrides append them on top
//     of SafeEnv() explicitly — see internal/signal/git/identity_test
//     (commitAs) and collector_test (backdated-commit site).
//
// # Callers
//
//   - cmd/signatory: validateExistingClone, gitCloneFull, runGitClone
//   - internal/signal/git: runGit (the workhorse used by every
//     git-derived signal — commit signing, tags, identity, vitality)
//   - every _test.go in the above two packages that spawns a git
//     subprocess, plus any future test helper
//
// Every exec.Command("git", ...) site in this codebase — production
// AND test — MUST set cmd.Env = gitenv.SafeEnv() before Run() /
// Output() / CombinedOutput(). Inheriting the parent env (either
// implicitly by not setting cmd.Env, or explicitly via cmd.Environ())
// is forbidden. New git subprocess sites added in the future must
// follow the same rule; reviewers should flag any exec.Command("git",
// ...) without an accompanying cmd.Env = gitenv.SafeEnv().
package gitenv

import (
	"os"
	"strings"
)

// denyPrefixes enumerates the prefixes whose entire namespace is
// stripped. New dangerous prefix families (hypothetical future:
// "GITHUB_" that git starts reading, etc.) can be added here; every
// name matching any listed prefix is dropped.
//
// The list is deliberately short — the "strip the git namespace"
// rule is the main guarantee. Adding more prefixes widens the strip;
// it never narrows it. If a specific inherited GIT_* variable is
// ever legitimately needed (currently: none), add an explicit
// re-admit in SafeEnv after the deny loop.
var denyPrefixes = []string{
	"GIT_",
	"SSH_ASKPASS",
}

// denyExactLower enumerates exact variable names (lower-cased for
// case-insensitive matching) stripped in addition to the prefix set.
// These names don't share a meaningful prefix with the git-namespace
// rule but honor by libcurl — which git uses for HTTPS transport —
// so they participate in the same attack surface.
//
// Current entries cover the libcurl proxy-control interface. Both
// upper- and lower-case spellings of each are honored by libcurl
// (e.g. both HTTPS_PROXY and https_proxy), and isDenied compares
// against the lower-cased input key to catch either.
var denyExactLower = map[string]bool{
	"http_proxy":  true,
	"https_proxy": true,
	"all_proxy":   true,
	"no_proxy":    true,
}

// SafeEnv returns os.Environ() with every variable in the dangerous
// prefix set stripped, and GIT_TERMINAL_PROMPT=0 force-appended.
//
// The returned slice is independent of os.Environ()'s backing array;
// callers can append to it safely.
//
// See the package doc for the rationale behind the prefix-strip
// design and the threat model.
func SafeEnv() []string {
	raw := os.Environ()
	safe := make([]string, 0, len(raw)+1)
	for _, kv := range raw {
		key, _, _ := strings.Cut(kv, "=")
		if isDenied(key) {
			continue
		}
		safe = append(safe, kv)
	}
	// Force non-interactive behavior. Appended after the strip loop
	// so this value is the only GIT_TERMINAL_PROMPT in the output
	// regardless of what the parent had.
	//
	// Last-wins caveat for callers. POSIX exec-time env resolution
	// takes the LAST value for a duplicated key, which means a caller
	// that does `cmd.Env = append(SafeEnv(), "GIT_TERMINAL_PROMPT=1")`
	// would override the forced zero and cause git to block on a
	// credential prompt. No existing caller does this; future test
	// authors who append identity overrides (GIT_AUTHOR_NAME,
	// GIT_COMMITTER_DATE, etc.) MUST NOT also append GIT_TERMINAL_PROMPT.
	// The deny-prefix rule already strips any inherited
	// GIT_TERMINAL_PROMPT, so the only way to break this invariant
	// is an explicit append after SafeEnv returns.
	safe = append(safe, "GIT_TERMINAL_PROMPT=0")
	return safe
}

// isDenied reports whether a variable name should be stripped: any
// prefix match against denyPrefixes, or any case-insensitive exact
// match against denyExactLower. Kept as a tiny helper to make the
// main loop read as pure policy (for each var, is it denied?) and
// to give the tests a direct unit to exercise.
func isDenied(key string) bool {
	for _, p := range denyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	if denyExactLower[strings.ToLower(key)] {
		return true
	}
	return false
}
