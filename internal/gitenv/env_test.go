package gitenv

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests guard SafeEnv's deny-by-prefix contract under an
// adversarial parent environment.
//
// Methodology note. Earlier versions of this test suite (and of
// SafeEnv itself) used an enumerated deny-list: explicit names, one
// by one. That proved structurally insufficient — the 2026-04-24
// postmortem plus a follow-up audit found the enumerated list
// missed GIT_INDEX_FILE and GIT_COMMON_DIR, both of which git hooks
// set and both of which redirect git's operation. The gap went
// undetected because a clean-shell `go test ./...` run never
// exercised the adversarial env state that exposed it.
//
// The tests below therefore do two things the previous suite did
// not:
//
//   1. Inject novel variable names (e.g. GIT_FUTURE_FEATURE_42)
//      that don't correspond to any documented current git var.
//      These test the *rule* (prefix-strip) rather than specific
//      names. If the rule regresses to an enumerated deny-list, the
//      novel-name cases fail.
//
//   2. Inject the full set of git hook vars (GIT_DIR, GIT_INDEX_FILE,
//      GIT_COMMON_DIR, and the bulk-injection trio) so the suite
//      exercises the actual threat environment a pre-commit hook
//      creates. Previously these could only be caught by committing
//      through the hook itself.
//
// t.Setenv and t.Parallel are mutually exclusive (t.Setenv mutates
// global process state). These tests are intentionally sequential.

// TestSafeEnv_StripsAllGitPrefix is the load-bearing test for the
// prefix-strip rule. It injects a mix of well-known dangerous names
// AND synthetic "novel" names that don't correspond to any real git
// var; every one must be stripped.
//
// Revert proof: change the GIT_ prefix check to an enumerated list;
// this test fails on the novel-name cases (GIT_NOVEL_FUTURE_*) that
// a named list wouldn't cover.
func TestSafeEnv_StripsAllGitPrefix(t *testing.T) {
	hostile := []string{
		// Known dangerous — must stay stripped.
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_COMMON_DIR",       // worktrees share config via this; missed by the pre-gitenv deny list
		"GIT_INDEX_FILE",       // set by pre-commit hooks; missed by the pre-gitenv deny list
		"GIT_OBJECT_DIRECTORY", // redirects the object store
		"GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_NAMESPACE",
		"GIT_CEILING_DIRECTORIES",
		"GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_SYSTEM",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_KEY_99",   // distant index; tests HasPrefix behavior
		"GIT_CONFIG_KEY_1000", // still distant index
		"GIT_SSH",
		"GIT_SSH_COMMAND",
		"GIT_PROXY_COMMAND",
		"GIT_EXEC_PATH",
		"GIT_ASKPASS",
		"GIT_TERMINAL_PROMPT",
		"GIT_AUTHOR_NAME",
		"GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME",
		"GIT_COMMITTER_EMAIL",
		"GIT_EDITOR",
		"GIT_PAGER",
		"GIT_PREFIX",
		"GIT_REFLOG_ACTION",

		// Novel names — do not correspond to any current git var.
		// If a future git release adds these exact names, or if an
		// attacker picks an unusual name hoping to slip through a
		// named deny-list, the prefix rule catches them.
		"GIT_NOVEL_FUTURE_42",
		"GIT_SPARSE_CHECKOUT_FOO",
		"GIT_ATTACKER_CONTROLLED_PROBE",

		// SSH_ASKPASS family.
		"SSH_ASKPASS",
		"SSH_ASKPASS_REQUIRE",
		"SSH_ASKPASS_NOVEL_VARIANT",
	}
	for _, key := range hostile {
		t.Setenv(key, "attacker-controlled-value")
	}

	env := SafeEnv()
	envMap := toMap(env)

	for _, key := range hostile {
		// GIT_TERMINAL_PROMPT is stripped and force-set to 0 — it's
		// allowed in the output at that exact value, not as the
		// attacker's value.
		if key == "GIT_TERMINAL_PROMPT" {
			assert.Equal(t, "0", envMap[key],
				"GIT_TERMINAL_PROMPT must be force-set to 0, not the attacker value")
			continue
		}
		_, present := envMap[key]
		assert.Falsef(t, present,
			"hostile var %q must not appear in SafeEnv() output (prefix-strip regression?)", key)
	}
}

// TestSafeEnv_StripsProxyVars verifies the libcurl proxy-control
// variables are stripped regardless of case. Git's HTTPS transport
// is libcurl-backed, and libcurl honors HTTP_PROXY / HTTPS_PROXY /
// ALL_PROXY / NO_PROXY in either case. An attacker-controlled value
// redirects every clone at an intercepting proxy.
//
// The test covers both upper- and lower-case spellings, plus a
// novel-suffix name ("HTTPSOMETHING_PROXY") to verify this is an
// exact-name check, not a substring check — a too-eager matcher
// would over-strip, a too-strict matcher would miss the lowercase
// case.
//
// Revert proof: remove denyExactLower from isDenied; this test
// fails for every injected case variant.
func TestSafeEnv_StripsProxyVars(t *testing.T) {
	hostile := map[string]string{
		"HTTP_PROXY":  "http://attacker.example",
		"HTTPS_PROXY": "http://attacker.example",
		"ALL_PROXY":   "socks5://attacker.example",
		"NO_PROXY":    "not-an-exclusion-hostile-flip",
		"http_proxy":  "http://attacker.example",
		"https_proxy": "http://attacker.example",
		"all_proxy":   "socks5://attacker.example",
		"no_proxy":    "not-an-exclusion-hostile-flip",
	}
	for key, value := range hostile {
		t.Setenv(key, value)
	}

	env := SafeEnv()
	envMap := toMap(env)

	for key := range hostile {
		_, present := envMap[key]
		assert.Falsef(t, present,
			"libcurl proxy var %q must not reach the git subprocess", key)
	}

	// Counter-case: a name that's NOT one of the proxy vars but
	// contains the substring "proxy" must pass through. This proves
	// the check is exact-name, not substring.
	t.Setenv("MYAPP_PROXY_CONFIG", "not-a-libcurl-var")
	envMap = toMap(SafeEnv())
	assert.Equal(t, "not-a-libcurl-var", envMap["MYAPP_PROXY_CONFIG"],
		"non-libcurl var with 'proxy' substring must pass through (exact-match policy, not substring)")
}

// TestSafeEnv_PreservesNonGitEnv verifies that vars outside the deny
// prefixes pass through unchanged. PATH, HOME, USER, SSL_CERT_FILE,
// TMPDIR, and XDG_CONFIG_HOME are representative — if the prefix
// list accidentally over-strips (e.g., someone adds "G" as a
// prefix), these fail.
//
// Revert proof: broaden a denyPrefix to "G" or add a blanket
// "strip everything"; this test fails because the preserved vars
// disappear.
func TestSafeEnv_PreservesNonGitEnv(t *testing.T) {
	preserved := map[string]string{
		"PATH":                        "/usr/local/bin:/usr/bin:/bin",
		"HOME":                        "/home/testuser",
		"USER":                        "testuser",
		"SSL_CERT_FILE":               "/etc/ssl/certs/ca-bundle.crt",
		"CURL_CA_BUNDLE":              "/etc/ssl/certs/ca-bundle.crt",
		"TMPDIR":                      "/tmp/mytmp",
		"TERM":                        "xterm-256color",
		"LANG":                        "en_US.UTF-8",
		"LC_ALL":                      "en_US.UTF-8",
		"TZ":                          "UTC",
		"XDG_CONFIG_HOME":             "/home/testuser/.config",
		"SSH_AUTH_SOCK":               "/tmp/ssh-agent.sock",
		"SSH_AGENT_PID":               "12345",
		"GITHUB_TOKEN":                "not-a-git-var-despite-github-prefix",
		"NOT_A_GIT_VAR_BUT_HAS_UNDER": "should-survive",
	}
	for k, v := range preserved {
		t.Setenv(k, v)
	}

	env := SafeEnv()
	envMap := toMap(env)

	for k, wantValue := range preserved {
		gotValue, present := envMap[k]
		require.Truef(t, present, "%q must be preserved in SafeEnv() output", k)
		assert.Equalf(t, wantValue, gotValue,
			"%q value must be preserved unchanged", k)
	}
}

// TestSafeEnv_ForceTerminalPromptZero verifies that
// GIT_TERMINAL_PROMPT=0 is always appended, and that no other value
// for it leaks through. Verified under three conditions:
//
//   - parent has GIT_TERMINAL_PROMPT=1 (the attacker tries to force
//     an interactive prompt that would hang a non-interactive caller)
//   - parent has GIT_TERMINAL_PROMPT unset
//   - parent has a garbage value
//
// Each case must produce exactly one GIT_TERMINAL_PROMPT= entry in
// the output, with value "0".
//
// Revert proof: remove the `safe = append(safe, ...)` line after the
// strip loop; this test fails because GIT_TERMINAL_PROMPT is absent.
func TestSafeEnv_ForceTerminalPromptZero(t *testing.T) {
	// Slice of {name, value} rather than a map so subtest order is
	// stable. The two cases don't share state today, but future
	// refactors that add shared state would produce flaky failures
	// under map iteration's randomized order.
	cases := []struct {
		name  string
		value string
	}{
		{"hostile_one", "1"},
		{"hostile_garbage", "please-prompt-me-attacker-controlled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GIT_TERMINAL_PROMPT", tc.value)

			env := SafeEnv()
			count := 0
			var found string
			for _, kv := range env {
				if strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
					count++
					found = kv
				}
			}
			assert.Equal(t, 1, count,
				"GIT_TERMINAL_PROMPT must appear exactly once in output")
			assert.Equal(t, "GIT_TERMINAL_PROMPT=0", found,
				"GIT_TERMINAL_PROMPT must be force-set to 0, regardless of parent value")
		})
	}

	// And the unset case — parent has no GIT_TERMINAL_PROMPT at all;
	// output must still contain exactly GIT_TERMINAL_PROMPT=0.
	t.Run("unset", func(t *testing.T) {
		// No t.Setenv here; we want the var absent in parent.
		env := SafeEnv()
		count := 0
		var found string
		for _, kv := range env {
			if strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
				count++
				found = kv
			}
		}
		assert.Equal(t, 1, count,
			"GIT_TERMINAL_PROMPT must appear even when parent had none")
		assert.Equal(t, "GIT_TERMINAL_PROMPT=0", found)
	})
}

// TestSafeEnv_ReturnsIndependentSlice verifies that callers can
// append to the returned slice without affecting subsequent calls —
// i.e., SafeEnv doesn't cache a shared slice that later callers
// observe post-mutation. Matters because helpers like commitAs /
// the backdated-commit site do
// `cmd.Env = append(SafeEnv(), "GIT_AUTHOR_NAME=...")` and must not
// corrupt a parallel caller's view.
//
// Revert proof: have SafeEnv return a package-level cached slice
// (e.g. `var cached = computeOnce(); return cached`) with nonzero
// spare capacity; this test fails because the append writes into
// the shared backing array and env2 observes APPEND_TEST.
func TestSafeEnv_ReturnsIndependentSlice(t *testing.T) {
	env1 := SafeEnv()
	env1 = append(env1, "APPEND_TEST=first-call-value")
	// Read env1 after the append so its value is observed, not
	// ineffectually reassigned — both as a sanity probe (the append
	// actually happened) and to satisfy ineffassign on the
	// assignment above.
	require.Contains(t, env1, "APPEND_TEST=first-call-value",
		"append must have extended env1 before we check env2")

	env2 := SafeEnv()
	for _, kv := range env2 {
		assert.NotEqual(t, "APPEND_TEST=first-call-value", kv,
			"mutation to a prior SafeEnv() return must not leak into subsequent calls")
	}
}

// TestIsDenied is a pure unit test of the prefix predicate, exercised
// directly without going through SafeEnv's Environ walk. Keeps the
// policy testable in isolation from the mechanics of slice building.
func TestIsDenied(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// GIT_ prefix — all denied.
		{"GIT_DIR", true},
		{"GIT_INDEX_FILE", true},
		{"GIT_COMMON_DIR", true},
		{"GIT_", true}, // exact prefix
		{"GIT_NOVEL_FUTURE_VAR", true},

		// SSH_ASKPASS prefix — denied.
		{"SSH_ASKPASS", true},
		{"SSH_ASKPASS_REQUIRE", true},
		{"SSH_ASKPASS_NOVEL", true},

		// libcurl proxy vars — denied by case-insensitive exact
		// match. Both cases honored by libcurl → both denied here.
		{"HTTP_PROXY", true},
		{"HTTPS_PROXY", true},
		{"ALL_PROXY", true},
		{"NO_PROXY", true},
		{"http_proxy", true},
		{"https_proxy", true},
		{"all_proxy", true},
		{"no_proxy", true},
		// Mixed case — still caught by ToLower comparison.
		{"Http_Proxy", true},
		{"HTTPS_proxy", true},

		// Non-denied.
		{"SSH_AUTH_SOCK", false},
		{"SSH_AGENT_PID", false},
		{"SSH_CLIENT", false},
		{"PATH", false},
		{"HOME", false},
		{"GITHUB_TOKEN", false},       // not GIT_-prefixed despite the name
		{"MYGIT_FOO", false},          // must match at start, not substring
		{"MYAPP_PROXY_CONFIG", false}, // 'proxy' substring is not enough
		{"HTTPD_PROXY", false},        // not one of the libcurl names
		{"", false},                   // degenerate
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isDenied(tc.name))
		})
	}
}

// TestNewCmd_SetsSafeEnv locks the env-strip discipline at the
// local-porcelain constructor boundary. Every git subprocess in the
// codebase routes through NewCmd, NewCloneCmd, or appends to SafeEnv
// — see package doc "Callers". This test is the structural guard
// for the NewCmd path; the subprocess-boundary tests
// (TestDefaultGitClone_StripsDangerousEnv et al. in cmd/signatory)
// verify the environment actually reaches the child.
//
// Asserts that cmd.Env is a SafeEnv-shaped slice. The exhaustive
// strip-rule coverage lives in TestSafeEnv_* above; here we just
// prove NewCmd does call SafeEnv (vs. silently leaving cmd.Env nil,
// which would inherit os.Environ via exec).
//
// Revert proof: comment out `cmd.Env = SafeEnv()` in NewCmd; the
// assertion fails because cmd.Env is nil and the GIT_TERMINAL_PROMPT=0
// marker is absent.
func TestNewCmd_SetsSafeEnv(t *testing.T) {
	t.Parallel()
	cmd := NewCmd(t.Context(), "log")
	require.NotNil(t, cmd.Env,
		"NewCmd must set cmd.Env (nil cmd.Env causes exec to inherit os.Environ — the 2026-04-24 hazard)")
	assert.Contains(t, cmd.Env, "GIT_TERMINAL_PROMPT=0",
		"NewCmd's cmd.Env must be a SafeEnv-shaped slice (force-appended GIT_TERMINAL_PROMPT=0 is the marker)")
}

// TestNewCmd_DoesNotSetWaitDelay locks the deliberate clone-only
// scope of WaitDelay. NewCmd is for local porcelain reads (log,
// for-each-ref, rev-list, etc.) which don't fork network helpers
// in practice; setting WaitDelay on every git invocation produced
// an empirically-observed slowdown in cmd/signatory's test suite,
// which is why the constructor was split. See package doc "Why
// clone-only" for the rationale.
//
// Revert proof: copy `cmd.WaitDelay = WaitDelay` from NewCloneCmd
// into NewCmd; the assertion fails because cmd.WaitDelay is no
// longer the zero Duration.
func TestNewCmd_DoesNotSetWaitDelay(t *testing.T) {
	t.Parallel()
	cmd := NewCmd(t.Context(), "log")
	assert.Zero(t, cmd.WaitDelay,
		"NewCmd is for local porcelain — WaitDelay belongs on NewCloneCmd only (see package doc 'Why clone-only')")
}

// TestNewCmd_PassesArgs locks the argv passthrough — args supplied
// to NewCmd must reach cmd.Args verbatim, prefixed only by the "git"
// binary name (cmd.Args[0] convention). Catches a regression where
// NewCmd accidentally rewrites or filters args.
//
// Revert proof: change NewCmd to e.g. exec.CommandContext(ctx, "git",
// args[1:]...) (drop the first arg); this test fails because
// cmd.Args[1] is no longer "log".
func TestNewCmd_PassesArgs(t *testing.T) {
	t.Parallel()
	cmd := NewCmd(t.Context(), "log", "--oneline", "-n", "5")
	assert.Equal(t,
		[]string{"git", "log", "--oneline", "-n", "5"},
		cmd.Args,
		"NewCmd must pass args verbatim with 'git' as argv[0]")
}

// TestNewCloneCmd_SetsWaitDelay locks the post-SIGKILL pipe-drain
// bound on the clone-shaped constructor. Without it, a grandchild
// that inherited stdout/stderr (typically ssh, askpass, or a
// credential helper that didn't receive the kill) holds the
// parent's pipes open indefinitely after Go's CommandContext
// SIGKILLs git on context expiry, blocking cmd.Wait. See package
// doc "WaitDelay rationale" for the threat shape and "Why
// clone-only" for why this isn't applied to NewCmd.
//
// Locking the constant value too — drift would be a silent
// inconsistency in subprocess hardening, and this assertion keeps
// the bound aligned to the documented number.
//
// Safety note. Pure constructor inspection — no Run / Output /
// CombinedOutput call, no PATH-shimmed fake-git, no env mutation.
// The 2026-04-24 worktree-corruption hazard requires a *running*
// git subprocess with an unscrubbed env to manifest; this test
// runs none.
//
// Revert proof: comment out `cmd.WaitDelay = WaitDelay` in
// NewCloneCmd; the assertion fails on the zero-Duration default.
func TestNewCloneCmd_SetsWaitDelay(t *testing.T) {
	t.Parallel()
	cmd := NewCloneCmd(t.Context(), "clone", "https://example.invalid/repo.git", "/tmp/x")
	assert.Equal(t, WaitDelay, cmd.WaitDelay,
		"NewCloneCmd must carry WaitDelay to bound post-kill pipe-drain on clone-shaped operations")
	assert.Equal(t, 5*time.Second, WaitDelay,
		"WaitDelay constant must match the documented 5s bound — see package doc before changing")
}

// TestNewCloneCmd_AlsoSetsSafeEnv locks the symmetry between
// constructors: NewCloneCmd must inherit NewCmd's env-strip
// discipline. The current implementation does this by delegating
// (NewCloneCmd calls NewCmd internally), but the assertion guards
// against a future "simplification" that forgets one or the other.
//
// Revert proof: rewrite NewCloneCmd to build the cmd inline without
// calling NewCmd, omitting cmd.Env = SafeEnv(); this test fails on
// the missing GIT_TERMINAL_PROMPT=0 marker.
func TestNewCloneCmd_AlsoSetsSafeEnv(t *testing.T) {
	t.Parallel()
	cmd := NewCloneCmd(t.Context(), "clone", "https://example.invalid/repo.git", "/tmp/x")
	require.NotNil(t, cmd.Env,
		"NewCloneCmd must set cmd.Env (the env-strip discipline is the OLD invariant; WaitDelay is the NEW one)")
	assert.Contains(t, cmd.Env, "GIT_TERMINAL_PROMPT=0",
		"NewCloneCmd's cmd.Env must be a SafeEnv-shaped slice")
}

// toMap converts a KEY=VALUE slice into a map keyed by variable
// name. Entries without an '=' are included with the full string as
// the key and empty value — none of our assertions care about that
// pathological case, but it keeps the helper total.
func toMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}
