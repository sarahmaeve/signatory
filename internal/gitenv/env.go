// Package gitenv provides the canonical constructor and sanitized
// environment slice for every git subprocess signatory spawns —
// production and test.
//
// Two siblings of the same attack class, defended at one chokepoint:
//
//   - Env-vector: GIT_CONFIG_KEY_*, GIT_SSH_COMMAND, etc. Defended by
//     SafeEnv stripping every GIT_*, SSH_ASKPASS*, and libcurl proxy
//     var before the subprocess starts.
//   - File-vector: a malicious on-disk `.git/config` shipped in a
//     tarball (CVE-2025-41390 / TALOS-2025-2243 / CWE-829). Defended
//     by NewCmd prepending a `-c key=value` argv prefix from
//     safeOverrides; the per-invocation `-c` flags override any on-
//     disk directive that could exec an attacker binary
//     (gpg.program, core.hooksPath, credential.helper, etc.).
//
// Both disciplines are applied unconditionally by every constructed
// *exec.Cmd. Bypassing either is a regression — see the test patterns
// at the end of "Callers" below.
//
// # API summary
//
//   - NewCmd is the constructor for local-porcelain git subprocesses
//     (read-only operations against an already-cloned repo: log,
//     for-each-ref, rev-list, rev-parse, remote get-url, etc.). It
//     sets cmd.Env to SafeEnv AND prepends safeOverrides as `-c k=v`
//     argv flags before user args. It does NOT set WaitDelay; these
//     operations don't spawn ssh/askpass/credential-helper
//     grandchildren in practice (see "WaitDelay rationale" below
//     for why).
//   - NewCloneCmd is the constructor for outbound git clones — the
//     operations that talk to a remote and may fork ssh / askpass /
//     credential-helper grandchildren that won't inherit SIGKILL.
//     It applies NewCmd's env-strip + override-prefix discipline AND
//     sets cmd.WaitDelay to WaitDelay. Used by defaultGitClone
//     (handoff path) and gitCloneFull (analyze path). Both are the
//     network-spawning sites.
//   - SafeEnv returns the hardened env slice (dangerous vars
//     stripped, GIT_TERMINAL_PROMPT=0 force-appended). Use directly
//     when you need to append identity / date overrides on top of
//     the hardened env (commitAs in identity_test.go, the
//     backdated-commit site in collector_test.go, the date-override
//     site in vitality_test.go). Note: SafeEnv covers only the env
//     vector; callers that build their own *exec.Cmd around SafeEnv
//     do NOT inherit the file-vector defense — prefer NewCmd /
//     NewCloneCmd whenever possible.
//   - WaitDelay is the exported value NewCloneCmd sets on
//     cmd.WaitDelay. 5 seconds. See "WaitDelay rationale" below.
//
// # Design: deny-by-default, explicit re-admit (env vector)
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
// # Design: deny-by-default, explicit override (file vector)
//
// The env-strip closes the env-vector class of attacks against git
// (GIT_CONFIG_KEY_*, GIT_SSH_COMMAND, etc.). The sibling class —
// CVE-2025-41390 / TALOS-2025-2243 / CWE-829 — is FILE-vector: a
// malicious `.git/config` shipped in a tarball, zip, or any archive
// the operator extracts and points signatory at. The on-disk config
// can declare e.g. `[gpg] program = /tmp/evil` and a single signed-
// shaped commit is enough for `git log --format=%G?` (which
// collectCommitSigning runs) to exec the attacker's binary. The
// same shape applies to core.hooksPath via `git fetch`'s reference-
// transaction hook, credential.helper for any auth'd fetch, and
// other directives in the `safeOverrides` catalog below.
//
// The defense is structurally identical to the env strip, applied at
// the same chokepoint: every NewCmd / NewCloneCmd invocation prepends
// a `-c key=value` argv prefix from `safeOverrides` BEFORE any user
// arg. Per-invocation `-c` overrides take precedence over the on-disk
// `.git/config`, so the attacker's directives are observed-but-
// neutralized: gpg.program=/usr/bin/false, core.hooksPath=/dev/null,
// credential.helper=, etc. See safeOverrides for the catalog and
// design/analysis/cve-2025-41390.md for the per-entry threat
// rationale.
//
// The two defenses are siblings, both load-bearing. Env-strip alone
// leaves the file-vector exposed; argv-override alone leaves the env-
// vector exposed. NewCmd applies both unconditionally.
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
// # WaitDelay rationale
//
// NewCloneCmd sets cmd.WaitDelay = WaitDelay (5s) on every
// constructed *exec.Cmd. WaitDelay bounds the time cmd.Wait spends
// draining stdout/stderr after Go's CommandContext sends SIGKILL on
// context expiry. Without it, a grandchild that inherited the pipes
// — typically ssh, askpass, or a credential helper that didn't
// itself receive the kill — can hold them open indefinitely,
// blocking Wait long after git itself has died.
//
// Why clone-only. The grandchild concern is specific to clone-shaped
// operations that talk to a remote: git forks ssh/askpass/credential-
// helper to authenticate the network call. Local porcelain commands
// (log, for-each-ref, rev-list, rev-parse, remote get-url against an
// already-cloned repo) read the object store and don't trigger
// network helpers in practice. Setting WaitDelay on every git
// subprocess introduced an empirically-observed slowdown in the
// cmd/signatory test suite that this two-constructor split avoids.
// The runtime-cost mechanism wasn't fully diagnosed; the structural
// fix is to scope WaitDelay to where its threat model actually
// applies.
//
// Scope. WaitDelay only bounds cmd.Wait's pipe-drain. It does NOT
// terminate the grandchild process. After Wait returns
// exec.ErrWaitDelay, the surviving grandchild keeps running — it
// can continue consuming CPU, hold network connections open, write
// to disk, and exit on its own schedule. For signatory's threat
// model that's a bounded annoyance (no signatory state is held by
// the grandchild) but worth understanding when reading audit logs:
// "git clone returned" is not the same as "all work git started has
// finished."
//
// 5 seconds is well above any legitimate post-kill drain on modern
// systems. The bound is set once here so the two outbound clone
// sites share it; drift between them would be a silent
// inconsistency in subprocess hardening.
//
// # Callers
//
// Every exec.Command / exec.CommandContext("git", ...) site in this
// codebase — production AND test — MUST go through NewCmd or
// NewCloneCmd, OR set cmd.Env to a slice rooted in SafeEnv()
// (identity-override case). Inheriting the parent env (either
// implicitly by not setting cmd.Env, or explicitly via
// cmd.Environ()) is forbidden. The canonical sites today:
//
//   - cmd/signatory: defaultGitClone, gitCloneFull (clone-shaped,
//     network-spawning) — via NewCloneCmd
//   - cmd/signatory: validateExistingClone (local porcelain:
//     `git -C path remote get-url origin`) — via NewCmd
//   - internal/signal/git: runGit (the workhorse used by every
//     git-derived signal — commit signing, tags, identity, vitality;
//     all porcelain reads against the object store) — via NewCmd
//   - test helpers in cmd/signatory and internal/signal/git that
//     build fixture repos or run local porcelain — via NewCmd
//   - test helpers that need to append identity / date overrides on
//     top of the hardened env — via SafeEnv directly (commitAs in
//     identity_test.go, the backdated-commit site in collector_test.go,
//     the date-override site in vitality_test.go)
//
// Reviewers should flag any exec.Command("git", ...) that doesn't
// route through NewCmd / NewCloneCmd or use SafeEnv as the env
// basis.
package gitenv

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// WaitDelay is the post-kill pipe-drain bound NewCloneCmd sets on
// every constructed *exec.Cmd. See package doc "WaitDelay rationale"
// for why 5s, why clone-only, and what WaitDelay does and doesn't
// bound.
const WaitDelay = 5 * time.Second

// NewCmd builds an *exec.Cmd for the git binary with the two
// disciplines every git subprocess in this codebase must follow:
//
//   - cmd.Env = SafeEnv() — strips GIT_*, SSH_ASKPASS*, libcurl proxy
//     vars; force-appends GIT_TERMINAL_PROMPT=0. Defends against the
//     env-vector class of `.git/config`-shaped attacks.
//   - argv carries the safeOverrides "-c key=value" prefix before
//     any user-supplied arg. Defends against the FILE-vector class
//     (CVE-2025-41390 / TALOS-2025-2243 / CWE-829): an attacker-
//     controlled `.git/config` shipped in a tarball cannot reach
//     gpg.program / core.hooksPath / credential.helper / etc. because
//     the per-invocation -c override wins over the on-disk config.
//     See safeOverrides for the catalog and design/analysis/
//     cve-2025-41390.md for the threat model.
//
// Use NewCmd for local porcelain operations against an already-
// cloned repo (log, for-each-ref, rev-list, rev-parse, remote
// get-url, etc.). For clone-shaped operations that talk to a remote
// and may fork ssh/askpass/credential-helper grandchildren, use
// NewCloneCmd instead — it adds cmd.WaitDelay on top of the same
// env-strip + override-prefix discipline.
//
// The args are passed verbatim as argv to git, AFTER the safeOverrides
// prefix. No shell. Each arg occupies one argv slot; metacharacters
// are not interpreted.
//
// G204 note. Every call site that constructs args from caller-
// supplied data must validate those args upstream (URL schemes,
// path containment, ref-name shape, etc.) — argv-form exec is
// shell-injection-safe but does not protect against argv-flag
// injection (a "-evil" first arg parsed as a flag). Validation
// remains the call site's responsibility; NewCmd guarantees only
// the env and -c-override disciplines.
func NewCmd(ctx context.Context, args ...string) *exec.Cmd {
	full := make([]string, 0, 2*len(safeOverrides)+len(args))
	for _, kv := range safeOverrides {
		full = append(full, "-c", kv)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...) //nolint:gosec // G204: argv-form; args validated by callers (URL schemes, path containment, ref-name shape)
	cmd.Env = SafeEnv()
	return cmd
}

// NewCloneCmd builds an *exec.Cmd for clone-shaped git operations:
// it applies NewCmd's env-strip discipline AND sets cmd.WaitDelay =
// WaitDelay so cmd.Wait can't block indefinitely on an
// ssh/askpass/credential-helper grandchild that didn't inherit the
// parent's SIGKILL.
//
// Use only for outbound clone or fetch operations that may fork a
// network helper. For local porcelain reads against an already-
// cloned repo, use NewCmd — see package doc "Why clone-only" for
// why WaitDelay is scoped to clone-shaped sites.
//
// G204 and argv discipline are the same as NewCmd; see that
// constructor's doc.
func NewCloneCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := NewCmd(ctx, args...)
	cmd.WaitDelay = WaitDelay
	return cmd
}

// safeOverrides is the catalog of `-c key=value` argv flags every
// NewCmd / NewCloneCmd invocation prepends, in addition to the env-
// strip discipline. Defends against the file-vector sibling of the
// env-vector attack class: a malicious `.git/config` shipped in a
// tarball or zip (CVE-2025-41390 / TALOS-2025-2243 / CWE-829) cannot
// drive arbitrary command execution because git's per-invocation -c
// flags take precedence over on-disk config.
//
// # Why each entry
//
// The catalog is sized to the threat model in design/analysis/
// cve-2025-41390.md, which traced every git subcommand signatory
// runs to the dangerous directives it could reach:
//
//   - gpg.program / gpg.openpgp.program / gpg.x509.program /
//     gpg.ssh.program — reachable via `git log --format=%G?` in
//     collectCommitSigning. The file-vector RCE Talos describes for
//     `git status` (via core.fsmonitor) replays through %G? against
//     attacker-controlled gpg.program. /usr/bin/false has the right
//     exit shape for git's status parser (non-zero → status E,
//     uncheckable, which classifySigning maps to classUnsigned).
//   - core.hooksPath=/dev/null — reachable via `git fetch`'s
//     reference-transaction hook on the --clone --refresh path.
//     /dev/null is a directory-shaped sentinel git treats as "no
//     hooks here" without erroring.
//   - core.fsmonitor= — not currently reached by signatory's command
//     set, but neutralized for defense-in-depth: it's the directive
//     Talos named in the original CVE.
//   - core.pager=cat — not currently reached (we capture stdout into
//     bytes.Buffer, isatty(stdout) is false), but cheap structural
//     guard against a future caller that wires a tty-shaped writer.
//   - core.sshCommand= — not currently reached (origin URL is
//     https), defense-in-depth.
//   - credential.helper= — reachable via `git fetch` when the remote
//     requires auth. Empty value disables every helper, including
//     attacker-supplied ones.
//   - protocol.file.allow=user / protocol.ext.allow=never — defends
//     against attacker-controlled URLs in `.git/config` (e.g. an
//     `[remote "origin"] url = ext::evil` overlay) that git fetch
//     would otherwise resolve via the `ext` helper. `user` for
//     protocol.file matches git's CVE-2022-39253 default;
//     `protocol.ext.allow=never` is stricter than git's default.
//
// # Why a hardcoded catalog rather than runtime configurability
//
// Same rationale as denyPrefixes / denyExactLower: deny by default,
// explicit re-admit. A configurable list would let a misuse, a test
// helper, or a future "just for this one case" call site silently
// shrink the file-vector defense and break the chokepoint property.
// The catalog is part of the trust contract; changes go through
// code review.
//
// # Behavioral side-effect on signing signals
//
// With gpg.program=/usr/bin/false in effect, %G? always reports E
// (cannot check) for any signed commit. classifySigning already maps
// E → classUnsigned (see internal/signal/git/signing.go), so the
// signing-ratio signals downgrade to "treat as unsigned" rather than
// crediting an unverifiable signature. This is the conservative
// trust-model call documented in classifySigning's docstring;
// neutralizing the file-vector attack does not change the policy.
var safeOverrides = []string{
	"gpg.program=/usr/bin/false",
	"gpg.openpgp.program=/usr/bin/false",
	"gpg.x509.program=/usr/bin/false",
	"gpg.ssh.program=/usr/bin/false",
	"core.hooksPath=/dev/null",
	"core.fsmonitor=",
	"core.pager=cat",
	"core.sshCommand=",
	"credential.helper=",
	"protocol.file.allow=user",
	"protocol.ext.allow=never",
}

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
	return denyExactLower[strings.ToLower(key)]
}
