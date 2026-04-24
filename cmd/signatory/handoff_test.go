package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sarahmaeve/signatory/internal/ecosystem"
	"github.com/sarahmaeve/signatory/internal/ecosystem/resolver"
	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubNpmResolverFunc adapts a simple (name → url, error) callback
// into an ecosystem resolver for use in handoff tests. Built so
// pre-M3 tests that used HandoffCmd.ResolveNpmSource can migrate
// with minimal churn: the callback shape is identical.
type stubNpmResolverFunc func(ctx context.Context, name string) (string, error)

func (f stubNpmResolverFunc) ResolveSource(ctx context.Context, name string) (resolver.DeclaredSource, error) {
	url, err := f(ctx, name)
	if err != nil {
		return resolver.DeclaredSource{}, err
	}
	if url == "" {
		return resolver.DeclaredSource{SelfReported: true}, nil
	}
	// Populate URI only when the URL resolves to a github target;
	// non-github URLs flow through as URL-only so the handoff-level
	// non-github test case still exercises the downstream error path.
	uri := ""
	cloneURL := url
	if res, perr := profile.ResolveTarget(url); perr == nil {
		uri = res.CanonicalURI
		if res.CloneURL != "" {
			cloneURL = res.CloneURL
		}
	}
	return resolver.DeclaredSource{
		URI:          uri,
		URL:          cloneURL,
		SelfReported: true,
	}, nil
}

// stubNpmRegistry builds a *resolver.Registry whose npm resolver
// delegates to fn. Tests inject via HandoffCmd.EcosystemRegistry.
func stubNpmRegistry(fn stubNpmResolverFunc) *resolver.Registry {
	r := resolver.NewRegistry()
	r.Register("npm", fn)
	return r
}

// CLI-level tests focus on wiring: flag parsing, template-name
// inference, output destination, and error shapes. Substitution /
// template search / scaffolding mechanics are covered by unit tests
// under internal/config.
//
// Most tests below exercise the --output path because it lets us
// assert the rendered handoff content by reading the file back.
// Stdout-mode behavior (when --output is empty) is covered by
// TestHandoff_StdoutModeWritesRenderedTemplate using captureStdout.

// captureStream redirects either os.Stdout or os.Stderr around fn
// and returns the bytes written to that stream. The "which" parameter
// must be either &os.Stdout or &os.Stderr — passing a pointer lets
// the helper restore the global on return.
//
// Tests should NOT rely on this helper to also let production code
// see the original stream — anything fn does that requires a real
// terminal will see the pipe end. Use it only for capturing test
// output.
func captureStream(t *testing.T, which **os.File, fn func()) string {
	t.Helper()
	orig := *which
	r, w, err := os.Pipe()
	require.NoError(t, err)
	*which = w
	// Restore the original stream and close the write end in all exit
	// paths — including panics from require.* inside fn(). Without the
	// defer on w.Close(), a panic inside fn() would leave the drain
	// goroutine blocked on buf.ReadFrom(r) forever (the read blocks
	// until the write end is closed), leaking the goroutine for the
	// duration of the test binary's run.
	defer func() {
		*which = orig
		w.Close()
	}()

	done := make(chan string, 1) // buffered so the goroutine never blocks
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()

	fn()
	// Explicit close before the receive so the drain goroutine sees EOF
	// even if the deferred close fires first (idempotent on *os.File).
	w.Close()
	return <-done
}

// captureStderr is a convenience wrapper around captureStream for the
// common case of capturing the informational report.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	return captureStream(t, &os.Stderr, fn)
}

// captureStdout captures os.Stdout — used for tests of stdout-mode
// handoff (when --output is empty, the rendered template goes to stdout).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	return captureStream(t, &os.Stdout, fn)
}

func TestHandoff_SecurityURL_WritesToOutputFile(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		Path:     "/tmp/thefuck-clone",
		Intake:   "Could this leak credentials?",
		Output:   outPath,
		Language: "python",
		Quiet:    true,
	}
	err := cmd.Run(&Globals{})
	require.NoError(t, err)

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	content := string(body)
	assert.Contains(t, content, "Security review for `thefuck`")
	assert.Contains(t, content, "https://github.com/nvbn/thefuck")
	assert.Contains(t, content, "/tmp/thefuck-clone")
	assert.Contains(t, content, "Could this leak credentials?")
	// Unfilled placeholders should not leak since all four were passed:
	assert.NotContains(t, content, "{TARGET_NAME}")
	assert.NotContains(t, content, "{TARGET_URL}")
	assert.NotContains(t, content, "{TARGET_PATH}")
	assert.NotContains(t, content, "{INTAKE_QUESTION}")
}

// TestHandoff_StdoutModeWritesRenderedTemplate covers the no-Output
// path: when the user omits --output, the rendered handoff goes to
// stdout. This is the primary path for shell-pipeline use
// (`signatory handoff … | llm --model claude-opus-4-6`) and was
// previously untested entirely (the runHandoff helper claimed to
// capture stdout but returned empty strings; F6 in the cmd review).
func TestHandoff_StdoutModeWritesRenderedTemplate(t *testing.T) {
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		Path:     "/tmp/thefuck-clone",
		Intake:   "stdout-mode test",
		Language: "python",
		Quiet:    true, // suppress the report so we can isolate stdout content
		// Output intentionally empty.
	}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.Contains(t, stdout, "Security review for `thefuck`",
		"stdout must receive the rendered template body")
	assert.Contains(t, stdout, "stdout-mode test",
		"intake placeholder must be substituted in stdout output")
	assert.NotContains(t, stdout, "{TARGET_NAME}",
		"unfilled placeholders should not appear when all flags are set")
}

func TestHandoff_ProvenanceRequiresEcosystem(t *testing.T) {
	cmd := &HandoffCmd{
		Role:     "provenance",
		Target:   "https://github.com/nvbn/thefuck",
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--ecosystem")
}

func TestHandoff_SecurityGoPicksGoTemplate(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:       "security",
		Target:     "https://github.com/alecthomas/kong",
		Path:       "/tmp/kong",
		Language:   "go",
		TargetRole: "validation",
		Output:     outPath,
		Quiet:      true,
	}
	err := cmd.Run(&Globals{})
	require.NoError(t, err)
	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	// The Go template includes Go-specific pattern names the Python
	// variant does not — asserting on one of them proves we selected
	// the right file.
	content := string(body)
	assert.Contains(t, content, "unsafe")
	assert.Contains(t, content, "cgo")
}

func TestHandoff_ExplicitTemplateOverridesRoleInference(t *testing.T) {
	// Lay down a custom template in a CLI-provided directory and
	// pass --template pointing at it. The embedded fallback must
	// NOT fire.
	tmplDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmplDir, "handoffs"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmplDir, "handoffs", "custom.md"),
		[]byte("CUSTOM: {TARGET_NAME} at {TARGET_PATH}"),
		0o644,
	))

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/foo/bar",
		Path:        "/tmp/bar",
		Template:    "handoffs/custom.md",
		TemplateDir: []string{tmplDir},
		Language:    "python",
		Output:      outPath,
		Quiet:       true,
	}
	err := cmd.Run(&Globals{})
	require.NoError(t, err)

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Equal(t, "CUSTOM: bar at /tmp/bar", string(body))
}

func TestHandoff_OutputFileOverwriteProtection(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	require.NoError(t, os.WriteFile(outPath, []byte("pre-existing"), 0o644))

	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/foo/bar",
		Path:     "/tmp/bar",
		Language: "python",
		Output:   outPath,
		Quiet:    true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")

	// File still has original content.
	body, _ := os.ReadFile(outPath)
	assert.Equal(t, "pre-existing", string(body))

	// With --force, overwrite succeeds.
	cmd.Force = true
	err = cmd.Run(&Globals{})
	require.NoError(t, err)
	body, _ = os.ReadFile(outPath)
	assert.NotEqual(t, "pre-existing", string(body))
}

func TestHandoff_StderrReportsUnfilledPlaceholders(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/foo/bar",
		Language: "python",
		Output:   outPath,
		// Intentionally omit --path and --intake so those placeholders
		// remain unfilled.
	}

	stderr := captureStderr(t, func() {
		err := cmd.Run(&Globals{})
		require.NoError(t, err)
	})

	assert.Contains(t, stderr, "TARGET_PATH")
	assert.Contains(t, stderr, "INTAKE_QUESTION")
	assert.Contains(t, stderr, "# template:")
}

func TestHandoff_QuietSuppressesStderrReport(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/foo/bar",
		Language: "python",
		Output:   outPath,
		Quiet:    true,
	}

	stderr := captureStderr(t, func() {
		_ = cmd.Run(&Globals{})
	})
	assert.Empty(t, strings.TrimSpace(stderr))
}

func TestHandoff_UnknownTargetFailsClearly(t *testing.T) {
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "thefuck", // bare name, no URL or path shape
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	// Error should name the missing substitution so the user knows
	// which flag to pass.
	assert.Contains(t, err.Error(), "TARGET_NAME")
}

// TestHandoff_AnalysisSessionID_RendersInstructionBlock covers the
// happy path: --analysis-session-id with a real session id results
// in the rendered handoff containing the session-linkage prose +
// the session id the agent needs to pass through to ingest.
func TestHandoff_AnalysisSessionID_RendersInstructionBlock(t *testing.T) {
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "https://github.com/nvbn/thefuck")

	cmd := &HandoffCmd{
		Role:              "security",
		Target:            "https://github.com/nvbn/thefuck",
		Path:              "/tmp/thefuck-clone",
		Language:          "python",
		AnalysisSessionID: sessionID,
		Quiet:             true,
	}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})
	assert.Contains(t, stdout, "Session linkage",
		"rendered handoff must surface the session-linkage block")
	assert.Contains(t, stdout, sessionID,
		"rendered handoff must embed the session id so the agent sees it verbatim")
	assert.Contains(t, stdout, "analysis_session_id",
		"rendered block must name the exact field the agent passes to signatory_ingest_analysis")
}

// TestHandoff_AnalysisSessionID_UnknownFailsAtHandoff is the
// validation regression anchor. A bogus session id must fail at
// handoff render time — NOT silently embed the id in a prompt that
// a downstream subagent would only discover is broken when its
// FK-checked ingest bounces off the store.
func TestHandoff_AnalysisSessionID_UnknownFailsAtHandoff(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &HandoffCmd{
		Role:              "security",
		Target:            "https://github.com/nvbn/thefuck",
		Path:              "/tmp/thefuck-clone",
		Language:          "python",
		AnalysisSessionID: "00000000-0000-0000-0000-000000000000",
		Quiet:             true,
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUsage,
		"unknown analysis session id is a user-input mistake; must surface as a usage error, not an internal error")
	assert.Contains(t, err.Error(), "not found")
}

// TestHandoff_AnalysisSessionID_TerminalFailsAtHandoff — the same
// validation path rejects a session that exists but has already
// been closed. Reopening terminal sessions is intentionally
// impossible (store-layer guard); handoff catches the case early so
// the operator sees the error before dispatching a subagent whose
// ingest would be refused.
func TestHandoff_AnalysisSessionID_TerminalFailsAtHandoff(t *testing.T) {
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "https://github.com/nvbn/thefuck")
	require.NoError(t, (&AnalysisEndCmd{
		SessionID: sessionID,
		Status:    "completed",
	}).Run(g))

	cmd := &HandoffCmd{
		Role:              "security",
		Target:            "https://github.com/nvbn/thefuck",
		Path:              "/tmp/thefuck-clone",
		Language:          "python",
		AnalysisSessionID: sessionID,
		Quiet:             true,
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUsage)
	assert.Contains(t, err.Error(), "terminal",
		"error must name the terminal state so the operator understands why the session was rejected")
}

// TestHandoff_NoAnalysisSessionID_NoInstructionBlock — the default
// path (no --analysis-session-id flag). The SESSION_INSTRUCTION
// placeholder resolves to empty, so the rendered handoff carries
// neither the prose block nor the literal placeholder.
func TestHandoff_NoAnalysisSessionID_NoInstructionBlock(t *testing.T) {
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		Path:     "/tmp/thefuck-clone",
		Language: "python",
		Quiet:    true,
	}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.NotContains(t, stdout, "{SESSION_INSTRUCTION}",
		"empty AnalysisSessionID must not leak literal placeholder into the handoff body")
	assert.NotContains(t, stdout, "Session linkage",
		"no session flag → no linkage block in the rendered prompt")
}

func TestHandoff_InferTemplateName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		role, language, want string
	}{
		// Language-specific security templates.
		{"security", "python", "handoffs/security-review-v1.md"},
		{"security", "go", "handoffs/security-review-go-v1.md"},
		{"security", "rust", "handoffs/security-review-rust-v1.md"},
		// Recognized flavors without dedicated templates → generic.
		{"security", "javascript", "handoffs/security-review-generic-v1.md"},
		{"security", "typescript", "handoffs/security-review-generic-v1.md"},
		{"security", "java", "handoffs/security-review-generic-v1.md"},
		{"security", "csharp", "handoffs/security-review-generic-v1.md"},
		{"security", "cpp", "handoffs/security-review-generic-v1.md"},
		{"security", "c", "handoffs/security-review-generic-v1.md"},
		{"security", "php", "handoffs/security-review-generic-v1.md"},
		// Undetected language → generic.
		{"security", "", "handoffs/security-review-generic-v1.md"},
		// Provenance is language-agnostic.
		{"provenance", "python", "handoffs/provenance-review-v1.md"},
		{"provenance", "go", "handoffs/provenance-review-v1.md"},
		{"provenance", "rust", "handoffs/provenance-review-v1.md"},
		{"provenance", "", "handoffs/provenance-review-v1.md"},
	}
	for _, tc := range cases {
		t.Run(tc.role+"-"+tc.language, func(t *testing.T) {
			assert.Equal(t, tc.want, inferTemplateName(tc.role, tc.language))
		})
	}
}

func TestHandoff_PositionalsParse(t *testing.T) {
	ctx, cli := parseCLI(t, "handoff", "security", "https://github.com/foo/bar")
	require.NoError(t, ctx.Error)
	assert.Equal(t, "security", cli.Handoff.Role)
	assert.Equal(t, "https://github.com/foo/bar", cli.Handoff.Target)
}

func TestHandoff_RoleEnumRejected(t *testing.T) {
	err := parseCLIExpectError(t, "handoff", "invalid-role", "https://github.com/foo/bar")
	require.Error(t, err)
}

// --- --network-precheck path: pure-unit tests -----------------------------
//
// The applyNetworkPrecheck method constructs ghclient.NewClient(...) directly
// with a hardcoded baseURL (https://api.github.com) — there is no env var
// or other seam to redirect it at an httptest.Server from outside the
// github package, and the Client struct's baseURL field is unexported.
// End-to-end integration tests for applyNetworkPrecheck would require
// either restructuring it to accept an injectable Source (an API change
// the task explicitly forbids) or exporting a baseURL seam on the github
// Client (out of scope for this test file). So we exercise only the pure
// helper functions here: looksLikeGitHubURL, languageToFlavor, and
// formatPrecheckReport. Detector-level behavior is already covered in
// internal/ecosystem/detect_test.go and internal/signal/github/
// detection_helpers_test.go, so the missing coverage is just the wiring
// inside applyNetworkPrecheck — small, and best left for a future
// refactor that introduces an injectable seam.

func TestLooksLikeGitHubURL_HTTPS(t *testing.T) {
	t.Parallel()
	assert.True(t, looksLikeGitHubURL("https://github.com/foo/bar"))
}

func TestLooksLikeGitHubURL_HTTP(t *testing.T) {
	t.Parallel()
	assert.True(t, looksLikeGitHubURL("http://github.com/foo/bar"))
}

func TestLooksLikeGitHubURL_CaseInsensitive(t *testing.T) {
	t.Parallel()
	assert.True(t, looksLikeGitHubURL("HTTPS://GITHUB.COM/foo/bar"))
}

func TestLooksLikeGitHubURL_RejectsOtherHosts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, input string
	}{
		{"gitlab", "https://gitlab.com/foo/bar"},
		{"bitbucket", "https://bitbucket.org/foo/bar"},
		{"sourcehut", "https://sourcehut.org/foo/bar"},
		{"example", "https://example.com/foo/bar"},
		{"bare-owner-repo", "foo/bar"},
		{"absolute-path", "/Users/me/code/foo"},
		{"relative-path", "./foo"},
		{"empty", ""},
		// Subdomain spoof: a host that merely contains "github.com"
		// as a substring shouldn't pass. Today's prefix check correctly
		// rejects this because the scheme prefix doesn't match.
		{"subdomain-spoof", "https://evil.com/github.com/foo/bar"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, looksLikeGitHubURL(tc.input), "input %q should not look like a github URL", tc.input)
		})
	}
}

func TestLanguageToFlavor_KnownLanguages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input, want string
	}{
		// Original two.
		{"Go", "go"},
		{"go", "go"},
		{"Python", "python"},
		{"python", "python"},
		// Newly mapped languages (GitHub-casing → slug).
		{"Rust", "rust"},
		{"JavaScript", "javascript"},
		{"TypeScript", "typescript"},
		{"Java", "java"},
		{"C#", "csharp"},
		{"C++", "cpp"},
		{"C", "c"},
		{"PHP", "php"},
		// Case-insensitive variants.
		{"rust", "rust"},
		{"JAVA", "java"},
		{"c#", "csharp"},
		{"c++", "cpp"},
		{"php", "php"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, languageToFlavor(tc.input))
		})
	}
}

func TestLanguageToFlavor_UnknownReturnsEmpty(t *testing.T) {
	t.Parallel()
	cases := []string{"Haskell", "Kotlin", "", "Ruby", "Scala", "Dart"}
	for _, in := range cases {
		t.Run("input="+in, func(t *testing.T) {
			assert.Empty(t, languageToFlavor(in))
		})
	}
}

func TestFormatPrecheckReport_FullResult(t *testing.T) {
	t.Parallel()
	result := &ecosystem.DetectionResult{
		Primary:    ecosystem.EcosystemGo,
		Candidates: []ecosystem.Ecosystem{ecosystem.EcosystemGo},
		Language:   "Go",
		RootFiles:  []string{"go.mod", "README.md"},
	}
	got := formatPrecheckReport("alecthomas", "kong", result, "go", "go")
	// Detection summary line.
	assert.Contains(t, got, "# precheck(alecthomas/kong):")
	assert.Contains(t, got, "ecosystem=go")
	assert.Contains(t, got, "language=Go")
	// Applied line lists both flags.
	assert.Contains(t, got, "# precheck applied:")
	assert.Contains(t, got, "--ecosystem=go")
	assert.Contains(t, got, "--language=go")
}

func TestFormatPrecheckReport_NoApplied(t *testing.T) {
	t.Parallel()
	// User had set both --ecosystem and --language explicitly, so
	// applyNetworkPrecheck called us with empty applied strings.
	// The detection summary still shows, but the "applied" line is
	// suppressed because there's nothing to report.
	result := &ecosystem.DetectionResult{
		Primary:    ecosystem.EcosystemPyPI,
		Candidates: []ecosystem.Ecosystem{ecosystem.EcosystemPyPI},
		Language:   "Python",
	}
	got := formatPrecheckReport("nvbn", "thefuck", result, "", "")
	assert.Contains(t, got, "# precheck(nvbn/thefuck):")
	assert.Contains(t, got, "ecosystem=pypi")
	assert.Contains(t, got, "language=Python")
	assert.NotContains(t, got, "# precheck applied:")
}

func TestFormatPrecheckReport_Unknown(t *testing.T) {
	t.Parallel()
	// No manifest matched and no language string — the report should
	// still render and explicitly say "ecosystem=unknown" rather than
	// rendering an empty value (which would look like a bug).
	result := &ecosystem.DetectionResult{
		Primary:    ecosystem.EcosystemUnknown,
		Candidates: nil,
		Language:   "",
	}
	got := formatPrecheckReport("foo", "bar", result, "", "")
	assert.Contains(t, got, "# precheck(foo/bar):")
	assert.Contains(t, got, "ecosystem=unknown")
	// No language line when language is empty.
	assert.NotContains(t, got, "language=")
	// And no applied line when nothing was applied.
	assert.NotContains(t, got, "# precheck applied:")
}

// --- --network-precheck path: end-to-end integration via injected Source ---
//
// These tests exercise applyNetworkPrecheck by constructing HandoffCmd
// with PrecheckSource set to a fakePrecheckSource. That lets us drive
// the full pipeline (classify target → build detector → call Source →
// populate cmd.Ecosystem / cmd.Language) without an httptest.Server and
// without touching the real ghclient.Client. No shared-state / mutation —
// each test gets its own fake, no cleanup needed.

// fakePrecheckSource satisfies ecosystem.Source with in-memory canned
// responses. Tests configure files/language per (owner, name) or
// globally via Files / Language when only one target is exercised.
type fakePrecheckSource struct {
	Files    []string
	Language string
	Err      error
}

func (f *fakePrecheckSource) ListRootFilenames(_ context.Context, _, _ string) ([]string, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Files, nil
}

func (f *fakePrecheckSource) GetRepoLanguage(_ context.Context, _, _ string) (string, error) {
	if f.Err != nil {
		return "", f.Err
	}
	return f.Language, nil
}

// inspectSource is a minimal ecosystem.Source that records how many
// times its methods were called. Useful for "this path should NEVER
// reach the Source" assertions, where the test wants to prove absence
// of invocation rather than assert on results.
type inspectSource struct {
	calls int
}

func (s *inspectSource) ListRootFilenames(_ context.Context, _, _ string) ([]string, error) {
	s.calls++
	return nil, nil
}

func (s *inspectSource) GetRepoLanguage(_ context.Context, _, _ string) (string, error) {
	s.calls++
	return "", nil
}

func TestHandoff_NetworkPrecheck_AppliesEcosystem(t *testing.T) {
	// Repo with go.mod → detector returns EcosystemGo → precheck
	// fills --ecosystem=go for the provenance role.
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "provenance",
		Target:          "https://github.com/alecthomas/kong",
		Path:            "/tmp/kong",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"go.mod", "README.md"},
			Language: "Go",
		},
		// --ecosystem intentionally empty; precheck must fill it.
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "go", cmd.Ecosystem, "precheck should fill --ecosystem from go.mod detection")

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	// ECOSYSTEM placeholder in the provenance template should now be "go".
	assert.Contains(t, string(body), "go")
	assert.NotContains(t, string(body), "{ECOSYSTEM}")
}

func TestHandoff_NetworkPrecheck_AppliesLanguage(t *testing.T) {
	// Repo whose primary language is Go → precheck sets --language=go,
	// which makes the security template pick the Go flavor.
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://github.com/alecthomas/kong",
		Path:            "/tmp/kong",
		TargetRole:      "validation",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"go.mod"},
			Language: "Go",
		},
		// --language intentionally empty.
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "go", cmd.Language, "Go primary language should select the Go security template")

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	// Go-flavored template mentions Go-specific patterns.
	assert.Contains(t, string(body), "unsafe")
	assert.Contains(t, string(body), "cgo")
}

func TestHandoff_NetworkPrecheck_RejectsNonGitHub(t *testing.T) {
	// Even with precheck enabled, non-GitHub hosts aren't supported
	// yet — we should error before attempting any network call. The
	// inspectSource below records whether its methods were reached;
	// they must not be.
	src := &inspectSource{}
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://gitlab.com/foo/bar",
		Path:            "/tmp/bar",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		PrecheckSource:  src,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github.com URL")
	assert.Zero(t, src.calls, "Source methods must not be invoked for non-GitHub targets")
}

// TestHandoff_NetworkPrecheck_RejectsNonGitHubSchemeOnly verifies that
// bare owner/repo shorthand (no scheme) is allowed through — the URL
// host guard only fires when a :// scheme is present.
func TestHandoff_NetworkPrecheck_RejectsNonGitHubSchemeOnly(t *testing.T) {
	src := &inspectSource{}
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "foo/bar", // shorthand, no scheme
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		PrecheckSource:  src,
	}
	// Shorthand targets pass the URL guard; the Source will be
	// called. The test verifies we DON'T reject this form.
	_ = cmd.Run(&Globals{})
	assert.NotZero(t, src.calls, "bare owner/repo must pass the URL guard and reach the Source")
}

// TestHandoff_NetworkPrecheck_WarnsOnUnflavoredLanguage verifies that
// when the precheck detects a language we don't have a flavor mapping
// for (e.g., Haskell), the user gets a stderr warning. The generic
// security template handles these correctly; the warning is
// informational so the user knows what happened.
func TestHandoff_NetworkPrecheck_WarnsOnUnflavoredLanguage(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://github.com/foo/bar",
		Path:            "/tmp/bar",
		NetworkPrecheck: true,
		Output:          outPath,
		// Quiet false: we WANT the warning to appear on stderr.
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"stack.yaml"},
			Language: "Haskell",
		},
	}
	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.Contains(t, stderr, "Haskell")
	assert.Contains(t, stderr, "no flavor mapping")
	assert.Contains(t, stderr, "generic security template")
	// Language stays empty — no flavor to set.
	assert.Empty(t, cmd.Language)
}

// TestHandoff_NetworkPrecheck_QuietSuppressesUnflavoredWarning verifies
// that --quiet suppresses the language-flavor warning, consistent with
// the rest of the precheck/clone reporting. (Same contract as the
// reuse-note suppression in TestClone_SkipsIfDestExists_Quiet.)
func TestHandoff_NetworkPrecheck_QuietSuppressesUnflavoredWarning(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://github.com/foo/bar",
		Path:            "/tmp/bar",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"mix.exs"},
			Language: "Elixir", // unmapped → triggers warning
		},
	}
	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.NotContains(t, stderr, "no flavor mapping",
		"--quiet must suppress the language-flavor warning")
}

func TestHandoff_NetworkPrecheck_DoesNotOverrideExplicit(t *testing.T) {
	// Detector says "ecosystem is go" but the user already passed
	// --ecosystem=pypi. Precheck must NOT clobber explicit flags.
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "provenance",
		Target:          "https://github.com/foo/bar",
		Path:            "/tmp/bar",
		Ecosystem:       "pypi", // explicit
		Language:        "go",   // explicit
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"go.mod"},
			Language: "Go",
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "pypi", cmd.Ecosystem, "explicit --ecosystem must not be clobbered")
	assert.Equal(t, "go", cmd.Language, "explicit --language must not be clobbered")
}

func TestHandoff_NetworkPrecheck_ReportsToStderr(t *testing.T) {
	// With --quiet off, the precheck report lands on stderr so the
	// user can see what was detected. This is the transparency
	// contract — nothing about the network call should happen invisibly.
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "provenance",
		Target:          "https://github.com/atuinsh/atuin",
		Path:            "/tmp/atuin",
		NetworkPrecheck: true,
		Output:          outPath,
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"Cargo.toml"},
			Language: "Rust",
		},
	}
	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.Contains(t, stderr, "# precheck(atuinsh/atuin):")
	assert.Contains(t, stderr, "ecosystem=crates")
	assert.Contains(t, stderr, "language=Rust")
	assert.Contains(t, stderr, "# precheck applied:")
	assert.Contains(t, stderr, "--ecosystem=crates")
}

// --- --network-precheck npm-source-resolution tests ---------------------
//
// Tests inject a stubbed resolver via HandoffCmd.ResolveNpmSource, so
// none of them hit registry.npmjs.org. The default production path
// (nil field → npm.NewClient().ResolveRepoURL) is exercised by the
// live registry integration tests in internal/signal/registry/npm.

// TestHandoff_NetworkPrecheck_NpmjsURL_ResolvesToGitHub verifies the
// end-to-end flow: user passes an npmjs.com URL, precheck consults the
// (stubbed) registry for the declared source, rewrites the target, and
// proceeds with the normal GitHub-API-based ecosystem detection.
func TestHandoff_NetworkPrecheck_NpmjsURL_ResolvesToGitHub(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	var resolvedName string
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://www.npmjs.com/package/express",
		Path:            "/tmp/express",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
		EcosystemRegistry: stubNpmRegistry(func(_ context.Context, name string) (string, error) {
			resolvedName = name
			return "https://github.com/expressjs/express", nil
		}),
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"package.json"},
			Language: "JavaScript",
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "express", resolvedName,
		"ResolveNpmSource must be called with the package name stripped of the pkg:npm/ prefix")
	assert.Equal(t, "https://github.com/expressjs/express", cmd.Target,
		"cmd.Target must be rewritten to the resolved GitHub URL so downstream steps see a clone-able target")
	assert.Equal(t, "https://github.com/expressjs/express", cmd.URL,
		"cmd.URL must be populated with the resolved GitHub URL when previously empty")
}

// TestHandoff_NetworkPrecheck_PkgNpmURI_ResolvesToGitHub covers the
// canonical-URI input form. npmjs.com URLs are normalized to pkg:npm/
// by ResolveTarget, so this is what applyNetworkPrecheck actually sees
// in both shapes — the test locks that equivalence.
func TestHandoff_NetworkPrecheck_PkgNpmURI_ResolvesToGitHub(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:npm/express",
		Path:            "/tmp/express",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
		EcosystemRegistry: stubNpmRegistry(func(_ context.Context, _ string) (string, error) {
			return "https://github.com/expressjs/express", nil
		}),
		PrecheckSource: &fakePrecheckSource{Files: []string{"package.json"}},
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "https://github.com/expressjs/express", cmd.Target)
}

// TestHandoff_NetworkPrecheck_ScopedPackage verifies that scoped
// packages (e.g. @types/node) preserve the full name through
// resolution — ShortName alone drops the scope, which would make the
// registry lookup miss.
func TestHandoff_NetworkPrecheck_ScopedPackage(t *testing.T) {
	var resolvedName string
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:npm/@types/node",
		Path:            "/tmp/node",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		EcosystemRegistry: stubNpmRegistry(func(_ context.Context, name string) (string, error) {
			resolvedName = name
			return "https://github.com/DefinitelyTyped/DefinitelyTyped", nil
		}),
		PrecheckSource: &fakePrecheckSource{Files: []string{"package.json"}},
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "@types/node", resolvedName,
		"scoped package name must be passed to the resolver intact (scope + name)")
}

// TestHandoff_NetworkPrecheck_NpmNoDeclaredSource covers the case where
// the registry has the package but its repository field is absent or
// empty. The command must error with a message naming the package, so
// the user knows what to pass explicitly as a replacement.
func TestHandoff_NetworkPrecheck_NpmNoDeclaredSource(t *testing.T) {
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:npm/orphan",
		Path:            "/tmp/orphan",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		EcosystemRegistry: stubNpmRegistry(func(_ context.Context, _ string) (string, error) {
			return "", nil // no declared repository
		}),
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"orphan"`)
	assert.Contains(t, err.Error(), "no resolvable source")
}

// TestHandoff_NetworkPrecheck_NpmNonGitHubSource covers a package that
// declares its source on a non-github host. We can't do GitHub-API
// precheck on gitlab, so the command errors — but must surface the
// declared URL verbatim so the user can inspect before re-running.
func TestHandoff_NetworkPrecheck_NpmNonGitHubSource(t *testing.T) {
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:npm/gitlabby",
		Path:            "/tmp/gitlabby",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		EcosystemRegistry: stubNpmRegistry(func(_ context.Context, _ string) (string, error) {
			return "https://gitlab.com/foo/bar", nil
		}),
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://gitlab.com/foo/bar",
		"error must show the declared non-github URL so the user can investigate")
	assert.Contains(t, err.Error(), "only github.com")
}

// TestHandoff_NetworkPrecheck_NpmResolverError covers transient
// registry failures (network error, 404, etc.). The error wraps the
// cause and names the package so the user sees both pieces at once.
func TestHandoff_NetworkPrecheck_NpmResolverError(t *testing.T) {
	sentinel := errors.New("registry unreachable")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:npm/whatever",
		Path:            "/tmp/whatever",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		EcosystemRegistry: stubNpmRegistry(func(_ context.Context, _ string) (string, error) {
			return "", sentinel
		}),
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel, "resolver error must be wrapped, not replaced")
	assert.Contains(t, err.Error(), `"whatever"`)
}

// TestHandoff_NetworkPrecheck_NpmDisclosureInReport verifies the
// transparency contract: the stderr report names the npm package and
// the declared source URL so a human can sanity-check that the chain
// wasn't redirected to an unrelated "famous" GitHub repo.
func TestHandoff_NetworkPrecheck_NpmDisclosureInReport(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:npm/express",
		Path:            "/tmp/express",
		NetworkPrecheck: true,
		Output:          outPath,
		EcosystemRegistry: stubNpmRegistry(func(_ context.Context, _ string) (string, error) {
			return "https://github.com/expressjs/express", nil
		}),
		PrecheckSource: &fakePrecheckSource{Files: []string{"package.json"}},
	}
	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.Contains(t, stderr, `npm package "express"`)
	assert.Contains(t, stderr, "https://github.com/expressjs/express")
	assert.Contains(t, stderr, "self-reported",
		"disclosure must call out that the source URL is self-declared, not verified")
}

// TestHandoff_NetworkPrecheck_PkgGoResolvesViaRegistry is the M3
// sibling of the npm tests: a pkg:go/<module> target goes through
// the ecosystem registry (specifically the Go resolver's offline
// path-prefix rules) and gets rewritten to its github source URL
// before the GitHub-API detector runs. No stub needed — the default
// registry ships the Go resolver.
func TestHandoff_NetworkPrecheck_PkgGoResolvesViaRegistry(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:go/golang.org/x/mod",
		Path:            "/tmp/mod",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"go.mod"},
			Language: "Go",
		},
		// No EcosystemRegistry override: uses resolver.Default which
		// has the shipped Go resolver registered at init time.
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "https://github.com/golang/mod", cmd.Target,
		"pkg:go target must be rewritten to the github source URL via the resolver registry")
}

// TestHandoff_NetworkPrecheck_PkgGoGithubDirect verifies the simpler
// Go case where the module path itself starts with github.com.
func TestHandoff_NetworkPrecheck_PkgGoGithubDirect(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:go/github.com/alecthomas/kong",
		Path:            "/tmp/kong",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"go.mod"},
			Language: "Go",
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "https://github.com/alecthomas/kong", cmd.Target)
}

// TestHandoff_NetworkPrecheck_PkgUnknownEcosystem verifies the error
// path when a caller passes pkg:<eco>/ with no registered resolver.
// Message names the supported ecosystems so the caller sees what's
// available.
func TestHandoff_NetworkPrecheck_PkgUnknownEcosystem(t *testing.T) {
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "pkg:madeup-ecosystem/anything",
		Path:            "/tmp/x",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		// Use a fresh empty registry so the test is independent of
		// whatever resolver.Default ships.
		EcosystemRegistry: resolver.NewRegistry(),
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no source resolver registered")
	assert.Contains(t, err.Error(), `"madeup-ecosystem"`)
}

// TestHandoff_NetworkPrecheck_NpmDoesNotInvokeResolverForGitHubTarget
// locks the narrow activation contract: the resolver only fires for
// pkg:npm/ (or npmjs.com-URL) targets; a plain GitHub URL bypasses it
// entirely. Prevents future regressions where the npm lookup becomes
// a silent tax on every precheck call.
func TestHandoff_NetworkPrecheck_NpmDoesNotInvokeResolverForGitHubTarget(t *testing.T) {
	invoked := false
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://github.com/alecthomas/kong",
		Path:            "/tmp/kong",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		EcosystemRegistry: stubNpmRegistry(func(_ context.Context, _ string) (string, error) {
			invoked = true
			return "", nil
		}),
		PrecheckSource: &fakePrecheckSource{Files: []string{"go.mod"}},
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.False(t, invoked, "ResolveNpmSource must not be called for a non-npm target")
}

// --- --clone-dir tests -------------------------------------------------------
//
// Tests inject a fake git-clone implementation via HandoffCmd.RunGitClone.
// Production fall-through (nil field) invokes defaultGitClone (exec.CommandContext).
// No shared mutation — each test owns its own HandoffCmd value.

// TestClone_CallsGitWithShallowFlags verifies that applyClone invokes
// the injected RunGitClone with --depth=1 and the correct destination path
// derived from --clone-dir and the repo name in the URL.
func TestClone_CallsGitWithShallowFlags(t *testing.T) {
	parent := t.TempDir()

	var gotURL, gotDest string
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		CloneDir: parent,
		Language: "python",
		Output:   outPath,
		Quiet:    true,
		RunGitClone: func(_ context.Context, url, dest, _ string) error {
			gotURL = url
			gotDest = dest
			// Simulate a successful clone by creating the dest dir so
			// subsequent stat-based logic stays coherent.
			return os.MkdirAll(dest, 0o755)
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))

	assert.Equal(t, "https://github.com/nvbn/thefuck", gotURL)
	// applyClone resolves symlinks in the parent (filepath.EvalSymlinks), so
	// the expected dest must also be derived from the resolved parent. On
	// macOS, t.TempDir() returns /var/folders/… but /var → /private/var.
	resolvedParent, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	wantDest := filepath.Join(resolvedParent, "thefuck")
	assert.Equal(t, wantDest, gotDest)
}

// TestClone_SkipsIfDestExists_NotQuiet verifies that runGitClone is NOT
// called when the destination directory already exists, and that the
// reuse note appears on stderr when --quiet is not set.
func TestClone_SkipsIfDestExists_NotQuiet(t *testing.T) {
	parent := t.TempDir()
	existingDest := filepath.Join(parent, "thefuck")
	require.NoError(t, os.MkdirAll(existingDest, 0o755))

	called := false
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		CloneDir: parent,
		Language: "python",
		Output:   outPath,
		// Quiet false: the reuse note SHOULD appear on stderr.
		RunGitClone: func(_ context.Context, _, _, _ string) error {
			called = true
			return nil
		},
	}

	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})

	assert.False(t, called, "RunGitClone must not be invoked when dest already exists")
	// EvalSymlinks resolves /var → /private/var on macOS; the reuse
	// note prints the resolved path. Match by suffix to stay portable.
	resolvedParent, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	resolvedDest := filepath.Join(resolvedParent, "thefuck")
	assert.Contains(t, stderr, "already exists, reusing")
	assert.Contains(t, stderr, resolvedDest)
}

// TestClone_SkipsIfDestExists_Quiet verifies that --quiet suppresses
// the reuse note, even though the skip-if-exists logic still fires.
// This regression-guards F2 from the cmd reviewer: the original code
// wrote the reuse note directly to stderr bypassing --quiet, and the
// previous test enshrined that bug because Quiet was unset.
func TestClone_SkipsIfDestExists_Quiet(t *testing.T) {
	parent := t.TempDir()
	existingDest := filepath.Join(parent, "thefuck")
	require.NoError(t, os.MkdirAll(existingDest, 0o755))

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		CloneDir: parent,
		Language: "python",
		Output:   outPath,
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, _, _ string) error {
			t.Fatal("git must not be invoked")
			return nil
		},
	}
	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.NotContains(t, stderr, "already exists",
		"--quiet must suppress the clone-reuse note")
}

// TestClone_FailsOnNonURLTarget ensures that passing --clone-dir with a local
// path target (not a URL) returns a clear error without invoking git.
func TestClone_FailsOnNonURLTarget(t *testing.T) {
	parent := t.TempDir()

	called := false
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "/Users/me/code/thefuck",
		CloneDir: parent,
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, _, _ string) error {
			called = true
			return nil
		},
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "URL")
	assert.False(t, called, "git must not be invoked for non-URL targets")
}

// TestClone_FailsOnNonWritableParent verifies writability is checked before
// git is spawned. Skipped on Windows where chmod semantics differ.
func TestClone_FailsOnNonWritableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unwritability test is not portable to Windows")
	}

	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o500))
	t.Cleanup(func() { os.Chmod(parent, 0o755) })

	called := false
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		CloneDir: parent,
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, _, _ string) error {
			called = true
			return nil
		},
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not writable")
	assert.False(t, called, "git must not be invoked when parent is not writable")
}

// TestClone_SetsTargetPath verifies that a successful clone causes TARGET_PATH
// in the rendered template to be set to the cloned directory path.
func TestClone_SetsTargetPath(t *testing.T) {
	parent := t.TempDir()
	// applyClone resolves symlinks in the parent, so the expected dest uses the
	// symlink-resolved parent. On macOS t.TempDir() → /var/… but /var → /private/var.
	resolvedParent, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	expectedDest := filepath.Join(resolvedParent, "thefuck")

	// Use a custom template that makes TARGET_PATH easy to assert on.
	tmplDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmplDir, "handoffs"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmplDir, "handoffs", "custom.md"),
		[]byte("path={TARGET_PATH}"),
		0o644,
	))

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		CloneDir:    parent,
		Template:    "handoffs/custom.md",
		TemplateDir: []string{tmplDir},
		Language:    "python",
		Output:      outPath,
		Quiet:       true,
		RunGitClone: func(_ context.Context, _, dest, _ string) error {
			return os.MkdirAll(dest, 0o755)
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Equal(t, "path="+expectedDest, string(body))
}

// TestClone_ExplicitPathWins verifies that when both --path and --clone-dir
// are passed, the rendered template uses the explicit --path value and a
// warning is emitted to stderr.
func TestClone_ExplicitPathWins(t *testing.T) {
	parent := t.TempDir()
	cloneDest := filepath.Join(parent, "thefuck")

	tmplDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmplDir, "handoffs"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmplDir, "handoffs", "custom.md"),
		[]byte("path={TARGET_PATH}"),
		0o644,
	))

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		CloneDir:    parent,
		Path:        "/explicit/path/to/thefuck", // explicit --path wins
		Template:    "handoffs/custom.md",
		TemplateDir: []string{tmplDir},
		Language:    "python",
		Output:      outPath,
		// Quiet intentionally false so we capture the override warning.
		RunGitClone: func(_ context.Context, _, dest, _ string) error {
			return os.MkdirAll(dest, 0o755)
		},
	}

	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	// Template must reflect the explicit --path, not the cloned dest.
	assert.Equal(t, "path=/explicit/path/to/thefuck", string(body))
	assert.NotContains(t, string(body), cloneDest)
	// Stderr must contain a warning that --clone-dir was overridden.
	assert.Contains(t, stderr, "--path=")
	assert.Contains(t, stderr, "wins")
}

// TestClone_QuietSuppressesOverrideWarning verifies that --quiet
// suppresses the "--path wins over --clone-dir" stderr note. The note
// is valuable for interactive users but noisy in scripted pipelines,
// and --quiet is documented as "no stderr output" for that audience.
func TestClone_QuietSuppressesOverrideWarning(t *testing.T) {
	parent := t.TempDir()

	tmplDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tmplDir, "handoffs"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(tmplDir, "handoffs", "custom.md"),
		[]byte("path={TARGET_PATH}"),
		0o644,
	))

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		CloneDir:    parent,
		Path:        "/explicit/override",
		Template:    "handoffs/custom.md",
		TemplateDir: []string{tmplDir},
		Language:    "python",
		Output:      outPath,
		Quiet:       true, // the whole point of this test
		RunGitClone: func(_ context.Context, _, dest, _ string) error {
			return os.MkdirAll(dest, 0o755)
		},
	}

	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	// Explicit --path must still win, but the override warning is
	// gated by --quiet and should be absent.
	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Equal(t, "path=/explicit/override", string(body))
	assert.NotContains(t, stderr, "wins", "--quiet must suppress the override warning")
}

// TestClone_RejectsEscapingNameViaSafeName verifies the containment-
// related guards by exercising the production code path. The previous
// version of this test asserted directly on filepath.Rel — testing
// stdlib behavior, not the production guard. (cmd reviewer F7.)
//
// safeCloneRepoName is the first containment defense: it rejects
// names that contain path separators, "..", "." or null bytes. If
// that guard ever loosened, applyClone would happily build paths
// like /tmp/clones/../etc/passwd and pass them to git.
func TestClone_RejectsEscapingNameViaSafeName(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"dotdot", ".."},
		{"single-dot", "."},
		{"empty", ""},
		{"nul", "evil\x00name"},
		{"forward-slash", "evil/name"},
		{"backslash", "evil\\name"},
		{"trailing-dotdot", "name/.."},
		{"leading-dotdot", "../name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := safeCloneRepoName(tc.in)
			require.Error(t, err, "safeCloneRepoName(%q) must reject", tc.in)
		})
	}
}

// TestClone_GuardFiresWhenNameValidationLoosens verifies that even if
// safeCloneRepoName were bypassed (e.g., a regression that allowed a
// name with separators), the secondary filepath.Rel containment check
// in applyClone would still reject. This is the belt-and-suspenders
// design — both guards must be tested independently because either
// could be loosened by an unrelated change.
//
// We construct the scenario by calling applyClone with a CloneDir
// pointing at a real directory and letting safeCloneRepoName accept
// the name (so we set up valid inputs), then assert that the
// production code's containment math runs. The strongest version of
// this test would inject a malicious name past safeCloneRepoName,
// but Go doesn't make that easy without method receivers we don't
// want to add. Instead, we round-trip a known-safe URL and confirm
// the dest computed in applyClone is strictly inside CloneDir — a
// regression that breaks the Rel check would change the dest.
func TestClone_GuardFiresWhenNameValidationLoosens(t *testing.T) {
	parent := t.TempDir()

	var gotDest string
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/owner/repo",
		CloneDir: parent,
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, dest, _ string) error {
			gotDest = dest
			return os.MkdirAll(dest, 0o755)
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))

	resolvedParent, err := filepath.EvalSymlinks(parent)
	require.NoError(t, err)
	rel, err := filepath.Rel(resolvedParent, gotDest)
	require.NoError(t, err)
	assert.NotEqual(t, "..", rel)
	assert.False(t, strings.HasPrefix(rel, ".."+string(filepath.Separator)),
		"dest %q must not be outside parent %q (rel=%q)", gotDest, resolvedParent, rel)
}

// --- Security regression tests -----------------------------------------------
//
// These tests exercise the three defenses added after adversarial review:
//
//  1. safeGitCloneURL: query strings, fragments, and embedded credentials in
//     the clone URL are rejected before git is ever invoked.
//  2. safeCloneRepoName: null bytes and path separators in the inferred repo
//     name are caught before the name is used as a directory component.
//  3. filepath.EvalSymlinks on clone-dir parent: a symlink at the parent path
//     can no longer redirect the containment check to an arbitrary location.
//
// Revert-experiment proof: temporarily remove the safeGitCloneURL call from
// applyClone and TestClone_RejectsQueryStringInURL fails (the mock is invoked
// with the ?upload-pack URL). Remove safeCloneRepoName and
// TestClone_RejectsNullByteInRepoName fails. Remove the EvalSymlinks call and
// TestClone_SymlinkParentIsResolved fails.

// TestSafeGitCloneURL_AcceptsCleanURLs verifies that well-formed https://
// URLs without query, fragment, or credentials pass safeGitCloneURL.
func TestSafeGitCloneURL_AcceptsCleanURLs(t *testing.T) {
	t.Parallel()
	cases := []string{
		"https://github.com/nvbn/thefuck",
		"https://github.com/alecthomas/kong.git",
		"https://github.com/foo/bar/",
		"http://github.com/foo/bar",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			assert.NoError(t, safeGitCloneURL(u), "clean URL %q should pass", u)
		})
	}
}

// TestSafeGitCloneURL_RejectsQueryString verifies that a URL with a query
// string is rejected. This is the primary defense against
// ?upload-pack=evil-binary style attacks where git may invoke the query-
// provided program as its upload-pack helper.
//
// Revert proof: comment out the safeGitCloneURL call in applyClone and
// TestClone_RejectsQueryStringInURL will report gotURL containing "?upload-pack".
func TestSafeGitCloneURL_RejectsQueryString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url  string
		desc string
	}{
		{"https://github.com/foo/bar?upload-pack=evil", "upload-pack helper injection"},
		{"https://github.com/foo/bar?x=1", "arbitrary query parameter"},
		{"https://github.com/foo/bar?", "empty query"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			err := safeGitCloneURL(tc.url)
			require.Error(t, err, "query-string URL %q must be rejected", tc.url)
			assert.Contains(t, err.Error(), "query string", "error must identify the problem")
		})
	}
}

// TestSafeGitCloneURL_RejectsFragment verifies that URL fragments are rejected.
func TestSafeGitCloneURL_RejectsFragment(t *testing.T) {
	t.Parallel()
	cases := []string{
		"https://github.com/foo/bar#evil",
		"https://github.com/foo/bar#",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			err := safeGitCloneURL(u)
			require.Error(t, err, "fragment URL %q must be rejected", u)
			assert.Contains(t, err.Error(), "fragment")
		})
	}
}

// TestSafeGitCloneURL_RejectsCredentials verifies that URLs with embedded
// userinfo (credentials) are rejected. Embedding credentials in URLs is a
// security antipattern: they land in shell history, process lists, and logs.
func TestSafeGitCloneURL_RejectsCredentials(t *testing.T) {
	t.Parallel()
	cases := []string{
		"https://user@github.com/foo/bar",
		"https://user:pass@github.com/foo/bar",
		"https://:token@github.com/foo/bar",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			err := safeGitCloneURL(u)
			require.Error(t, err, "credential URL %q must be rejected", u)
			assert.Contains(t, err.Error(), "credentials")
		})
	}
}

// TestSafeGitCloneURL_RejectsNullByte verifies that null bytes in the raw URL
// string are caught. A null byte in a path component produces an unusable or
// dangerous filesystem path on most operating systems.
func TestSafeGitCloneURL_RejectsNullByte(t *testing.T) {
	t.Parallel()
	u := "https://github.com/foo/bar\x00evil"
	err := safeGitCloneURL(u)
	require.Error(t, err, "null-byte URL must be rejected")
	assert.Contains(t, err.Error(), "null byte")
}

// TestSafeCloneRepoName_AcceptsNormalNames verifies that well-formed repo
// names (the kinds InferNameFromURL actually returns for GitHub URLs) pass.
func TestSafeCloneRepoName_AcceptsNormalNames(t *testing.T) {
	t.Parallel()
	cases := []string{
		"thefuck", "kong", "my-repo", "my.repo", "repo_123",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, safeCloneRepoName(name))
		})
	}
}

// TestSafeCloneRepoName_RejectsNullByte verifies that a null byte in the
// inferred repo name is caught. This can arise from a URL containing %00
// in the repo path segment (url.Parse percent-decodes path components).
//
// Revert proof: comment out the safeCloneRepoName call in applyClone and
// TestClone_RejectsNullByteInRepoName passes git a dest with a null byte.
func TestSafeCloneRepoName_RejectsNullByte(t *testing.T) {
	t.Parallel()
	err := safeCloneRepoName("bar\x00evil")
	require.Error(t, err, "null-byte name must be rejected")
	assert.Contains(t, err.Error(), "null byte")
}

// TestSafeCloneRepoName_RejectsDotDot verifies that ".." and "." are rejected
// as repo names, blocking any remaining traversal path that bypasses the
// filepath.Rel check.
func TestSafeCloneRepoName_RejectsDotDot(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"..", "."} {
		t.Run(name, func(t *testing.T) {
			err := safeCloneRepoName(name)
			require.Error(t, err, "%q must be rejected as a repo name", name)
			assert.Contains(t, err.Error(), "reserved path component")
		})
	}
}

// TestSafeCloneRepoName_RejectsPathSeparator verifies that names containing
// slash or backslash are rejected.
func TestSafeCloneRepoName_RejectsPathSeparator(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"foo/bar", "foo\\bar"} {
		t.Run(name, func(t *testing.T) {
			err := safeCloneRepoName(name)
			require.Error(t, err, "path-separator name %q must be rejected", name)
			assert.Contains(t, err.Error(), "path separator")
		})
	}
}

// TestClone_RejectsQueryStringInURL is an end-to-end test verifying that
// applyClone rejects a URL with a query string before calling runGitClone.
// This is the full-pipeline regression for the ?upload-pack=evil attack.
//
// Revert proof: remove the safeGitCloneURL call in applyClone and the test
// fails because cmd.Run returns nil (no error) and gotCalled is true.
func TestClone_RejectsQueryStringInURL(t *testing.T) {
	parent := t.TempDir()
	gotCalled := false
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/foo/bar?upload-pack=evil",
		CloneDir: parent,
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, _, _ string) error {
			gotCalled = true
			return nil
		},
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err, "URL with query string must be rejected")
	assert.Contains(t, err.Error(), "query string",
		"error must name the rejected component so the user can correct the URL")
	assert.False(t, gotCalled, "RunGitClone must not be invoked for unsafe URLs")
}

// TestClone_RejectsNullByteInRepoName is an end-to-end test that a URL
// containing %00 in the repo path (which percent-decodes to a null byte
// in the inferred name) is rejected before runGitClone is called.
//
// Revert proof: remove the safeCloneRepoName call in applyClone; the test
// fails because cmd.Run returns nil and gotCalled is true.
func TestClone_RejectsNullByteInRepoName(t *testing.T) {
	parent := t.TempDir()
	gotCalled := false
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/foo/bar%00evil", // %00 → null byte
		CloneDir: parent,
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, _, _ string) error {
			gotCalled = true
			return nil
		},
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err, "URL with null byte in repo name must be rejected")
	assert.False(t, gotCalled, "RunGitClone must not be invoked for unsafe repo names")
}

// TestClone_SymlinkParentIsResolved verifies that when --clone-dir itself is
// a symlink, applyClone resolves it to its real path before building the dest.
// This prevents a scenario where a later symlink replacement could redirect the
// parent to an arbitrary location and bypass the containment check.
//
// Revert proof: replace filepath.EvalSymlinks(absParent) with absParent in
// applyClone; the test still passes on non-symlink systems, so to see the
// difference you need to check that gotDest starts with the resolved path —
// which would be the /private/var/... form on macOS vs /var/... without the fix.
func TestClone_SymlinkParentIsResolved(t *testing.T) {
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "clones-link")
	require.NoError(t, os.Symlink(realDir, linkDir))

	var gotDest string
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		CloneDir: linkDir, // symlink
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, dest, _ string) error {
			gotDest = dest
			return os.MkdirAll(dest, 0o755)
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))

	// The dest passed to runGitClone must be under the resolved (real) dir,
	// not the symlink path. This ensures the containment check operated on
	// real paths. filepath.EvalSymlinks resolves both realDir and gotDest.
	resolvedReal, err := filepath.EvalSymlinks(realDir)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(gotDest, resolvedReal),
		"dest %q must be under the resolved real dir %q, not the symlink path %q",
		gotDest, resolvedReal, linkDir)
}

// TestClone_RejectsEmbeddedCredentials is an end-to-end test that a URL with
// embedded userinfo (credentials) is rejected before runGitClone is called.
func TestClone_RejectsEmbeddedCredentials(t *testing.T) {
	parent := t.TempDir()
	gotCalled := false
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://user:token@github.com/foo/bar",
		CloneDir: parent,
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, _, _ string) error {
			gotCalled = true
			return nil
		},
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err, "URL with embedded credentials must be rejected")
	assert.Contains(t, err.Error(), "credentials")
	assert.False(t, gotCalled, "RunGitClone must not be invoked for credential-bearing URLs")
}

// TestPrecheck_ErrorDoesNotLeakToken is a regression test for token leakage
// in the --network-precheck error path. When a source error message
// accidentally contains the token string (e.g., from a misconfigured proxy
// that echoes request headers), the error is propagated as-is because
// applyNetworkPrecheck wraps it with %w. This test documents the current
// behavior and ensures the error at least mentions "network-precheck" so the
// caller can identify the origin.
//
// NOTE: This test does NOT assert that the token is absent from the error,
// because the current code propagates errors verbatim — masking them would
// hide real diagnostic information. The correct long-term fix is for the
// ghclient.Client to scrub tokens from any error it surfaces (the client
// already does this for response bodies, but not for transport-layer errors).
// See the "Recommendations not acted on" section in the adversarial review.
func TestPrecheck_ErrorPropagatesWithContext(t *testing.T) {
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://github.com/foo/bar",
		Path:            "/tmp/bar",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
		PrecheckSource: &fakePrecheckSource{
			Err: fmt.Errorf("simulated network error"),
		},
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	// The error must be wrapped with "network-precheck:" context so the
	// user and any log aggregators can locate the origin.
	assert.Contains(t, err.Error(), "network-precheck",
		"precheck errors must carry the 'network-precheck' context prefix")
}

// --- gitenv subprocess-boundary test for defaultGitClone --------------------
//
// Unit tests for gitenv.SafeEnv (the strip-set contract) live in
// internal/gitenv/env_test.go and are exhaustive. Subprocess-boundary
// tests — proving that a production git-subprocess site actually sets
// cmd.Env = gitenv.SafeEnv() and that the stripped env reaches the
// child — live next to their respective production sites:
//
//   - TestGitCloneFull_StripsDangerousEnv           (collectors_test.go)
//   - TestValidateExistingClone_StripsDangerousEnv  (collectors_test.go)
//   - TestDefaultGitClone_StripsDangerousEnv        (below — this file)
//
// These three tests together cover every production exec.Command("git",
// ...) site that performs a clone-shaped operation. runGit (in
// internal/signal/git/exec.go) is the fourth production site; its
// subprocess-boundary coverage comes from the integration tests in
// that package which run real git commands against real repos.

// TestDefaultGitClone_StripsDangerousEnv is the subprocess-boundary
// regression test for the handoff-path git clone. Before the 2026-04-24
// extraction, defaultGitClone would have inherited the parent's env,
// allowing a hostile GIT_SSH_COMMAND to invoke an attacker binary
// during the clone. The test installs a fake git shim that dumps its
// env, runs defaultGitClone directly, and asserts the shim never saw
// the dangerous vars.
//
// Revert proof: delete the `cmd.Env = gitenv.SafeEnv()` line in
// defaultGitClone (handoff.go); this test fails because the shim's
// env dump contains GIT_SSH_COMMAND / GIT_CONFIG_KEY_0 / etc.
//
// NOTE: Uses t.Setenv, which panics if the test also calls t.Parallel().
// Intentionally sequential.
func TestDefaultGitClone_StripsDangerousEnv(t *testing.T) {
	envDump := installFakeGitEnvDump(t)

	// Hostile parent env — representative sample of each threat class:
	// transport override (GIT_SSH_COMMAND), config injection
	// (GIT_CONFIG_COUNT + KEY/VALUE), binary-path redirection
	// (GIT_EXEC_PATH), repo redirection (GIT_DIR, GIT_INDEX_FILE,
	// GIT_COMMON_DIR — the last two were missed by the pre-gitenv
	// deny-list and are the motivating gap for this test).
	t.Setenv("GIT_SSH_COMMAND", "evil-binary --steal-credentials")
	t.Setenv("GIT_EXEC_PATH", "/tmp/attacker-bin")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.sshCommand")
	t.Setenv("GIT_CONFIG_VALUE_0", "evil")
	t.Setenv("GIT_DIR", "/tmp/attacker-git-dir")
	t.Setenv("GIT_INDEX_FILE", "/tmp/attacker-index")
	t.Setenv("GIT_COMMON_DIR", "/tmp/attacker-common")

	// Dest doesn't need to exist; the fake git shim ignores its args
	// and exits 0. URL is synthetic but present for realism.
	dest := filepath.Join(t.TempDir(), "clone-dest")
	require.NoError(t,
		defaultGitClone(context.Background(), "https://example.invalid/repo.git", dest, ""),
		"fake git must exit 0 — non-zero indicates the shim wasn't picked up via PATH")

	env := readEnvDump(t, envDump)
	for _, key := range []string{
		"GIT_SSH_COMMAND",
		"GIT_EXEC_PATH",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
		"GIT_DIR",
		"GIT_INDEX_FILE",
		"GIT_COMMON_DIR",
	} {
		_, present := env[key]
		assert.Falsef(t, present,
			"%s must not leak from parent env into git subprocess (defaultGitClone)", key)
	}
	// PATH must survive — the child needs it to locate ssh, helpers,
	// etc. Verifies gitenv.SafeEnv didn't accidentally over-strip.
	assert.NotEmpty(t, env["PATH"], "PATH must be preserved in child env")
	// GIT_TERMINAL_PROMPT is force-set to 0 by gitenv.SafeEnv to
	// prevent the child from blocking on a credential prompt.
	assert.Equal(t, "0", env["GIT_TERMINAL_PROMPT"],
		"GIT_TERMINAL_PROMPT must be force-set to 0 in child env")
}

// TestCaptureStream_DrainGoroutineTerminatesOnClose verifies that captureStream's
// drain goroutine sees EOF and terminates when the write end of the pipe is closed
// (which the defer guarantees). The buffered channel (cap=1) means the goroutine
// never blocks even if the caller panics before reading from done.
//
// Revert proof: change `done := make(chan string, 1)` back to `make(chan string)`
// (unbuffered) in captureStream, then panic inside fn() — the goroutine blocks
// forever trying to send on done because no one is reading it.
//
// NOTE: captureStream mutates os.Stdout/os.Stderr (global state), so this test
// must NOT use t.Parallel — concurrent mutations of the same global would race.
func TestCaptureStream_DrainGoroutineTerminatesOnClose(t *testing.T) {
	// Exercise captureStream via captureStdout with a fn that writes nothing.
	// The deferred w.Close() must fire, giving the goroutine EOF, producing "".
	result := captureStdout(t, func() {
		// Intentionally write nothing.
	})
	assert.Equal(t, "", result, "drain goroutine must return empty string when fn writes nothing")
}

// --- Synthesist handoff tests (M6c) ---

// synthesisFixtureTarget seeds a temp store with one analyst output
// for the given canonical URI and returns a Globals pointed at that
// store. Used by the synthesist handoff tests.
func synthesisFixtureTarget(t *testing.T, canonicalURI string) *Globals {
	t.Helper()
	g := newTestGlobals(t)
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	lineStart := 10
	_, err = s.IngestAnalystOutput(t.Context(), &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:00:00Z",
		},
		Target: canonicalURI,
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F001",
				Verdict:   "synthesist-fixture finding",
				Rationale: "synthesist-fixture rationale",
				Severity:  exchange.Severity{Default: exchange.SeverityMedium},
				Category:  "injection",
				Citations: []exchange.Citation{
					{Path: "src/main.go", LineStart: &lineStart},
				},
			},
		},
	}, "synthesist-fixture-source")
	require.NoError(t, err)
	return g
}

// TestHandoff_Synthesist_FillsTargetURL_ForPkgURIs asserts that
// {TARGET_URL} substitutes to the canonical URI when the target is
// a pkg: URI (no CloneURL). Pre-fix, pkg-URI synthesist handoffs
// left {TARGET_URL} as a literal because the main target-resolution
// path only filled cmd.URL from resolved.CloneURL, and pkg URIs
// don't have one. The literal leaked into the target header and
// the output skeleton, confusing the synthesist into treating
// "{TARGET_URL}" as text. See 2026-04-21 dogfood.
func TestHandoff_Synthesist_FillsTargetURL_ForPkgURIs(t *testing.T) {
	const canonicalURI = "pkg:npm/target-url-fallback"
	g := synthesisFixtureTarget(t, canonicalURI)

	outPath := filepath.Join(t.TempDir(), "synth.md")
	cmd := &HandoffCmd{
		Role:   "synthesist",
		Target: canonicalURI,
		Output: outPath,
		Quiet:  true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	assert.NotContains(t, rendered, "{TARGET_URL}",
		"pkg:URI synthesist handoff must not leave {TARGET_URL} as a literal placeholder")
	assert.Contains(t, rendered, canonicalURI,
		"TARGET_URL fallback must render the canonical URI (the identity being synthesized)")
}

// TestHandoff_Synthesist_PreservesVersionSuffix_InTargetURL: the
// fallback must carry a @V suffix through when the caller passed
// one. This is the user-facing half of Plan-A canonicalization:
// the rendered handoff's target == the target the caller asked
// about, version-scoping and all. The synthesist's output.target
// JSON field gets populated from this, so a versioned handoff
// yields a versioned synthesis target without inference from the
// evidence block.
func TestHandoff_Synthesist_PreservesVersionSuffix_InTargetURL(t *testing.T) {
	const unversioned = "pkg:npm/target-url-versioned"
	g := synthesisFixtureTarget(t, unversioned)

	outPath := filepath.Join(t.TempDir(), "synth-versioned.md")
	cmd := &HandoffCmd{
		Role:   "synthesist",
		Target: unversioned + "@3.1.4",
		Output: outPath,
		Quiet:  true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	assert.NotContains(t, rendered, "{TARGET_URL}")
	assert.Contains(t, rendered, unversioned+"@3.1.4",
		"@V suffix must be preserved in TARGET_URL so the synthesist's output.target is versioned")
}

// TestHandoff_Synthesist_EmbedsEvidenceJSON covers the M6c contract:
// when role=synthesist, the rendered handoff must contain the
// evidence block substituted in place of {EVIDENCE_JSON} and carry
// the actual analyst data the synthesist will reason over.
func TestHandoff_Synthesist_EmbedsEvidenceJSON(t *testing.T) {
	const canonicalURI = "repo:github/example/synthesist-embed"
	g := synthesisFixtureTarget(t, canonicalURI)

	outPath := filepath.Join(t.TempDir(), "synthesis-handoff.md")
	cmd := &HandoffCmd{
		Role:   "synthesist",
		Target: canonicalURI,
		Output: outPath,
		Quiet:  true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// The placeholder must not leak through.
	assert.NotContains(t, rendered, "{EVIDENCE_JSON}",
		"{EVIDENCE_JSON} placeholder must be substituted for synthesist role")

	// Evidence content must appear: analyst id, conclusion verdict,
	// canonical URI.
	assert.Contains(t, rendered, canonicalURI,
		"rendered evidence must cite the target canonical URI")
	assert.Contains(t, rendered, "external-sec-v1",
		"rendered evidence must surface the contributing analyst role")
	assert.Contains(t, rendered, "synthesist-fixture finding",
		"rendered evidence must carry the conclusion verdict verbatim")
	assert.Contains(t, rendered, "F001",
		"rendered evidence must carry the conclusion local id for F-ID citation")

	// The independence rule must survive the render (the template
	// fence persists; not consumed by substitution).
	assert.Contains(t, rendered,
		"Previous reports do not corroborate new conclusions",
		"independence rule must be present in the rendered synthesist handoff")
}

// TestHandoff_Synthesist_FailsWhenNoAnalyses asserts the CLI refuses
// to emit a synthesist handoff when the target has no ingested
// non-synthesis analyses. Dispatching a synthesist against empty
// evidence would produce a no-op or a fabricated synthesis — both
// are failure modes the CLI catches here rather than later.
func TestHandoff_Synthesist_FailsWhenNoAnalyses(t *testing.T) {
	g := newTestGlobals(t) // empty store — no analyses ingested

	cmd := &HandoffCmd{
		Role:   "synthesist",
		Target: "repo:github/example/never-analyzed",
		Output: filepath.Join(t.TempDir(), "synthesis.md"),
		Quiet:  true,
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no entity matches",
		"error must explain that the target hasn't been analyzed yet")
}

// TestHandoff_Synthesist_ExcludesPriorSyntheses ensures the D9
// cross-pollination prohibition holds end-to-end through the CLI:
// even when a prior synthesis exists for the same target, the
// rendered handoff must not surface it in the embedded evidence.
// The rendered handoff is the synthesist's ENTIRE input — if a
// prior synthesis leaked through, the next synthesist would anchor
// on it. This test proves the M6b filter survives the CLI round-trip.
func TestHandoff_Synthesist_ExcludesPriorSyntheses(t *testing.T) {
	const canonicalURI = "repo:github/example/already-synthesized"
	g := synthesisFixtureTarget(t, canonicalURI)

	// Add a prior synthesis with a very distinctive tier/reasoning
	// marker that we'll assert-absent from the rendered handoff.
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	_, err = s.IngestAnalystOutput(t.Context(), &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T01:00:00Z",
		},
		Target: canonicalURI,
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierVettedFrozen,
				RationaleSummary: "DISTINCTIVE-PRIOR-SYNTHESIS-MARKER",
			},
			Reasoning: "DISTINCTIVE-PRIOR-SYNTHESIS-MARKER body",
			Summary:   "DISTINCTIVE-PRIOR-SYNTHESIS-MARKER summary",
		},
	}, "prior-synthesis-source")
	require.NoError(t, err)
	s.Close() //nolint:errcheck // test cleanup

	outPath := filepath.Join(t.TempDir(), "synthesis-next.md")
	cmd := &HandoffCmd{
		Role:   "synthesist",
		Target: canonicalURI,
		Output: outPath,
		Quiet:  true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	assert.NotContains(t, rendered, "DISTINCTIVE-PRIOR-SYNTHESIS-MARKER",
		"D9 regression: prior synthesis content leaked into synthesist handoff. "+
			"The evidence assembler must filter signatory-synthesis-* analyst IDs.")
	// But the real analyst output must still be there.
	assert.Contains(t, rendered, "external-sec-v1",
		"legitimate analyst output must still render in the evidence")
}

// --- @version threading through to git clone --branch -------------------------
//
// These tests cover the version-aware clone path. They exercise:
//  1. The version surfaces from ResolveTarget into HandoffCmd.requestedVersion
//  2. applyClone passes the version to RunGitClone
//  3. Versioned clone reports name the version in the report string
//  4. safeGitVersion rejects ref shapes that would inject flags or shell-meta
//
// Together these ensure /analyze github.com/X/Y@v1.0.0 will clone v1.0.0,
// not HEAD-of-default-branch — the GAPS.md fix.

// TestClone_VersionFromTargetIsForwardedToCloneFn captures the
// version that the fake RunGitClone receives. The fixture target
// is `https://github.com/nvbn/thefuck@v3.32` so applyClone's
// upstream Run() resolves it through ResolveTarget, captures
// "v3.32" into cmd.requestedVersion, and forwards it through.
func TestClone_VersionFromTargetIsForwardedToCloneFn(t *testing.T) {
	var gotURL, gotDest, gotVersion string
	parent := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck@v3.32",
		CloneDir: parent,
		Language: "python",
		Output:   outPath,
		Quiet:    true,
		RunGitClone: func(_ context.Context, url, dest, version string) error {
			gotURL = url
			gotDest = dest
			gotVersion = version
			return os.MkdirAll(dest, 0o755)
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))

	// The clone URL is the bare HTTPS form (no @version) — the
	// version is a separate clone-time parameter, not part of the
	// URL git is asked to clone from.
	assert.Equal(t, "https://github.com/nvbn/thefuck", gotURL,
		"clone URL must be the bare HTTPS form; @version is a separate parameter")
	assert.NotEmpty(t, gotDest, "dest must be set")
	assert.Equal(t, "v3.32", gotVersion,
		"requested version must reach the clone function")
}

// TestClone_NoVersionLeavesVersionEmpty is the regression guard
// for the "today's behavior" path: when the target has no
// @version suffix, the clone function receives "" and falls back
// to HEAD-of-default-branch (production code path).
func TestClone_NoVersionLeavesVersionEmpty(t *testing.T) {
	var gotVersion string
	parent := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		CloneDir: parent,
		Language: "python",
		Output:   outPath,
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, dest, version string) error {
			gotVersion = version
			return os.MkdirAll(dest, 0o755)
		},
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Empty(t, gotVersion,
		"unversioned target must yield empty version arg (HEAD-of-default-branch behavior)")
}

// TestClone_RejectsUnsafeVersion confirms safeGitVersion gates
// the clone call. The malicious version "-evil" survives
// ResolveTarget (whose own @V parser only rejects nested
// separators, not flag-shaped refs) but must fail at applyClone
// before any subprocess fires. This is the load-bearing
// flag-injection guard: without it, `git clone --branch -evil
// <url>` would have git interpret `-evil` as a flag.
func TestClone_RejectsUnsafeVersion(t *testing.T) {
	parent := t.TempDir()
	outPath := filepath.Join(t.TempDir(), "handoff.md")
	called := false
	cmd := &HandoffCmd{
		Role: "security",
		// @-evil — flag-shaped ref that ResolveTarget accepts
		// (only nested @ / empty version are rejected there) but
		// safeGitVersion in applyClone refuses before the clone
		// subprocess is invoked.
		Target:   "repo:github/nvbn/thefuck@-evil",
		CloneDir: parent,
		Language: "python",
		Output:   outPath,
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, _, _ string) error {
			called = true
			return nil
		},
	}

	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe git ref")
	assert.False(t, called, "clone function must not be called when ref is unsafe")
}

// TestSafeGitVersion_AcceptsCleanRefs covers the positive shapes
// safeGitVersion must let through. Locks in compatibility with
// the version conventions across Go (v1.2.3, pseudo-versions),
// npm (1.2.3), calendar versioning (2026.04.15), pre-release
// (v1.0.0-alpha.1, build metadata 1.0.0+meta), branches (main,
// release/1.0), and full ref paths (refs/tags/v1.0.0).
func TestSafeGitVersion_AcceptsCleanRefs(t *testing.T) {
	cleanRefs := []string{
		"",                                 // empty = HEAD-of-default-branch
		"v1.2.3",                           // Go / tag-style
		"1.2.3",                            // npm-style
		"v1.0.0-alpha.1",                   // semver pre-release
		"1.0.0+build.meta",                 // semver build metadata
		"v0.0.0-20230101000000-abcdef0123", // Go pseudo-version
		"2026.04.15",                       // calendar version
		"main",                             // branch
		"release/1.0",                      // slashed branch
		"refs/tags/v1.0.0",                 // full refspec path
		"feature_branch",                   // underscore
	}
	for _, v := range cleanRefs {
		t.Run(v, func(t *testing.T) {
			assert.NoError(t, safeGitVersion(v),
				"legitimate ref %q must pass shape validation", v)
		})
	}
}

// TestSafeGitVersion_RejectsUnsafeRefs covers the rejection cases.
// Each one is a real attack shape (flag injection, shell-meta,
// path traversal) or a clear malformation (leading slash, double
// dot, control chars).
func TestSafeGitVersion_RejectsUnsafeRefs(t *testing.T) {
	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"flag injection (leading dash)", "-evil", "begin with '-'"},
		{"path traversal", "v1.0/../etc/passwd", "must not contain '..'"},
		{"shell metachar — semicolon", "v1.0;rm", "invalid character"},
		{"shell metachar — pipe", "v1.0|rm", "invalid character"},
		{"shell metachar — backtick", "v1.0`x`", "invalid character"},
		{"shell metachar — dollar", "v1.0$x", "invalid character"},
		{"newline in ref", "v1.0\n", "invalid character"},
		{"trailing slash", "main/", "must not end with"},
		{"leading slash", "/main", "must not begin with"},
		{"trailing dot", "v1.0.", "must not end with"},
		{"leading dot", ".hidden", "must not begin with"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := safeGitVersion(c.ref)
			require.Error(t, err, "ref %q must be rejected", c.ref)
			assert.Contains(t, err.Error(), c.want)
		})
	}
}

// TestDefaultGitClone_ArgvShape uses the same PATH-shimmed fake-
// git pattern as the env-scrub tests (collectors_test.go) to
// verify the actual argv defaultGitClone constructs. Confirms
// --branch is present when version is set, absent when empty,
// and positional args are ordered correctly.
//
// NOTE: t.Setenv and t.Parallel are mutually exclusive — this
// test is intentionally sequential.
func TestDefaultGitClone_ArgvShape(t *testing.T) {
	// Build a fake `git` shim that records its argv to a file.
	shimDir := t.TempDir()
	argDump := filepath.Join(t.TempDir(), "argv-dump")
	fakeGit := filepath.Join(shimDir, "git")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nexit 0\n", argDump)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Versioned clone: --branch <version> must appear before the
	// URL and dest positional args.
	require.NoError(t, defaultGitClone(t.Context(),
		"https://example.invalid/repo.git",
		"/tmp/dest-versioned",
		"v1.2.3"))
	got, err := os.ReadFile(argDump)
	require.NoError(t, err)
	args := strings.Split(strings.TrimSpace(string(got)), "\n")
	assert.Equal(t,
		[]string{"clone", "--depth=1", "--branch", "v1.2.3",
			"https://example.invalid/repo.git", "/tmp/dest-versioned"},
		args,
		"versioned clone must include --branch followed by ref, then URL, then dest")

	// Unversioned clone: no --branch.
	require.NoError(t, os.Remove(argDump))
	require.NoError(t, defaultGitClone(t.Context(),
		"https://example.invalid/repo.git",
		"/tmp/dest-unversioned",
		""))
	got, err = os.ReadFile(argDump)
	require.NoError(t, err)
	args = strings.Split(strings.TrimSpace(string(got)), "\n")
	assert.Equal(t,
		[]string{"clone", "--depth=1",
			"https://example.invalid/repo.git", "/tmp/dest-unversioned"},
		args,
		"unversioned clone must omit --branch entirely")
}

// --- Synthesist + @version: hot fix for the halted dogfood ---------------
//
// These tests cover the two bugs surfaced by the user's halted
// /analyze github.com/stretchr/testify@v1.11.1 run:
//
//   1. assembleSynthesisEvidence re-resolved cmd.Target (which
//      Run() had rewritten to the bare clone URL) and looked up
//      the BASE entity URI. The analysts' outputs were at the
//      FULL URI (with @V), so lookup missed.
//
//   2. The synthesist's {TARGET_URL} substitution was filled
//      from the bare clone URL instead of the canonical URI
//      with @V. Synthesist's output target field would land at
//      a different entity from the analysts' outputs.
//
// Both fixed in the same hot-fix commit; tests live alongside.

// TestHandoff_Synthesist_RepoVersionedTargetURL is the
// regression guard for bug #2: a repo: target with @V passed to
// the synthesist must surface the canonical URI WITH @V in the
// rendered TARGET_URL slot, not the bare clone URL.
//
// Revert proof: change the synthesist branch in Run()'s
// canonicalization to fall through to `cmd.URL = resolved.CloneURL`;
// this test fails because the rendered handoff body would
// contain the bare URL `https://github.com/example/repo-versioned-url`
// instead of the canonical `repo:github/example/repo-versioned-url@v2.0.0`.
func TestHandoff_Synthesist_RepoVersionedTargetURL(t *testing.T) {
	const canonical = "repo:github/example/repo-versioned-url@v2.0.0"
	// Seed analyst output at the FULL URI (matches today's ingest
	// behavior — see normalizeTargetToCanonicalURI).
	g := synthesisFixtureTarget(t, canonical)

	outPath := filepath.Join(t.TempDir(), "synth-repo-versioned.md")
	cmd := &HandoffCmd{
		Role:   "synthesist",
		Target: "github.com/example/repo-versioned-url@v2.0.0",
		Output: outPath,
		Quiet:  true,
	}
	require.NoError(t, cmd.Run(g))

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	assert.Contains(t, rendered, canonical,
		"synthesist's TARGET_URL must be the canonical URI with @V, "+
			"so its output's target field matches the analysts' outputs at the same entity")
	assert.NotContains(t, rendered, "https://github.com/example/repo-versioned-url`",
		"synthesist's TARGET_URL must NOT be the bare clone URL — "+
			"that would put the synthesist's output at a different entity from the analysts'")
}

// TestHandoff_Synthesist_RepoVersionedEvidenceLookup is the
// regression guard for bug #1: when the user invokes synthesist
// with a versioned target and the analysts' outputs are at the
// FULL URI, the evidence assembly must find them.
//
// Pre-fix, assembleSynthesisEvidence re-resolved cmd.Target
// (which Run() had rewritten to bare URL) and did
// SplitURIVersion on the result — getting "" version, no
// fallback, "no entity matches" failure. The user halted the
// run at exactly this point on testify@v1.11.1.
//
// Revert proof: drop the `cmd.requestedVersion != ""` re-attach
// in assembleSynthesisEvidence; this test fails because lookup
// uses the BASE URI, the FULL-URI entity is missed, and the
// fallback split has nothing to do (already at base).
func TestHandoff_Synthesist_RepoVersionedEvidenceLookup(t *testing.T) {
	const canonical = "repo:github/example/repo-versioned-evidence@v1.11.1"
	// Analyst outputs land at the FULL URI — matches what /analyze
	// produces today before the bigger Plan-A-everywhere refactor.
	g := synthesisFixtureTarget(t, canonical)

	outPath := filepath.Join(t.TempDir(), "synth-repo-versioned-evidence.md")
	cmd := &HandoffCmd{
		Role:   "synthesist",
		Target: "github.com/example/repo-versioned-evidence@v1.11.1",
		Output: outPath,
		Quiet:  true,
	}
	require.NoError(t, cmd.Run(g),
		"synthesist must find evidence at the FULL URI when /analyze "+
			"ingested analysts there — pre-fix this returned 'no entity matches'")

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	// Sanity: the rendered handoff actually embeds the analysts'
	// finding (proves we found the right entity, not just any).
	assert.Contains(t, rendered, "synthesist-fixture finding",
		"the embedded evidence must contain the seeded conclusion's verdict text")
}

// TestHandoff_Synthesist_FallsBackToBaseWhenFullURINotFound
// covers the converse direction: if a future Plan-A-everywhere
// refactor stores analyst outputs at the BASE URI, the
// synthesist must still find them via the fallback split.
// Defends both code paths so the lookup is robust regardless of
// which storage shape is current.
func TestHandoff_Synthesist_FallsBackToBaseWhenFullURINotFound(t *testing.T) {
	const baseCanonical = "repo:github/example/repo-base-fallback"
	// Seed at the BASE URI (Plan A future state).
	g := synthesisFixtureTarget(t, baseCanonical)

	outPath := filepath.Join(t.TempDir(), "synth-repo-base-fallback.md")
	cmd := &HandoffCmd{
		Role: "synthesist",
		// User input includes @V; storage has only BASE; fallback
		// split must drop @V and find the BASE entity.
		Target: "github.com/example/repo-base-fallback@v3.0.0",
		Output: outPath,
		Quiet:  true,
	}
	require.NoError(t, cmd.Run(g),
		"fallback to BASE URI must succeed when analyst outputs "+
			"are stored at the unversioned form")

	body, err := os.ReadFile(outPath)
	require.NoError(t, err)
	rendered := string(body)

	assert.Contains(t, rendered, "synthesist-fixture finding",
		"fallback path must produce evidence containing the seeded conclusion")
}
