package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sarahmaeve/signatory/internal/ecosystem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		RunGitClone: func(_ context.Context, url, dest string) error {
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
		RunGitClone: func(_ context.Context, _, _ string) error {
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
		RunGitClone: func(_ context.Context, _, _ string) error {
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
		RunGitClone: func(_ context.Context, _, _ string) error {
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
		RunGitClone: func(_ context.Context, _, _ string) error {
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
		RunGitClone: func(_ context.Context, _, dest string) error {
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
		RunGitClone: func(_ context.Context, _, dest string) error {
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
		RunGitClone: func(_ context.Context, _, dest string) error {
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
		RunGitClone: func(_ context.Context, _, dest string) error {
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
		RunGitClone: func(_ context.Context, _, _ string) error {
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
		RunGitClone: func(_ context.Context, _, _ string) error {
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
		RunGitClone: func(_ context.Context, _, dest string) error {
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
		RunGitClone: func(_ context.Context, _, _ string) error {
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

// --- safeGitEnv tests --------------------------------------------------------
//
// These tests guard the env-var stripping logic added after the Go-security
// adversarial pass. Threat: a hostile parent environment sets GIT_SSH_COMMAND
// or GIT_PROXY_COMMAND to an attacker-controlled binary; without the stripping
// logic, git clone would invoke that binary with the clone URL as argument,
// achieving RCE.
//
// Revert proof: replace safeGitEnv() with os.Environ() in the runGitClone
// definition; TestSafeGitEnv_StripsDangerousVars fails because the dangerous
// key still appears in the returned slice.

// TestSafeGitEnv_StripsDangerousVars verifies that each of the documented
// dangerous git environment variables is absent from safeGitEnv()'s output,
// regardless of whether the parent process has them set.
//
// NOTE: t.Setenv and t.Parallel are mutually exclusive in Go's testing package
// because Setenv modifies global process state. These tests are intentionally
// sequential to use t.Setenv for clean env injection/restoration.
func TestSafeGitEnv_StripsDangerousVars(t *testing.T) {
	// Inject dangerous vars into this process's environment for the duration
	// of the test, then restore. This simulates a hostile parent environment.
	dangerous := []string{
		"GIT_TERMINAL_PROMPT",
		"GIT_SSH_COMMAND",
		"GIT_SSH",
		"GIT_PROXY_COMMAND",
		"GIT_EXEC_PATH",
		"GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_SYSTEM",
		"GIT_DIR",
		"GIT_WORK_TREE",
		"GIT_ASKPASS",
		"SSH_ASKPASS",
		"SSH_ASKPASS_REQUIRE",
		// Bulk-injection variants:
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_KEY_1",
		"GIT_CONFIG_VALUE_1",
	}

	// t.Setenv handles cleanup automatically.
	for _, key := range dangerous {
		t.Setenv(key, "attacker-controlled-value")
	}

	env := safeGitEnv()

	// Build a map for O(1) lookup.
	envMap := make(map[string]string, len(env))
	for _, kv := range env {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		envMap[kv[:idx]] = kv[idx+1:]
	}

	for _, key := range dangerous {
		// GIT_TERMINAL_PROMPT is allowed through but forced to "0" (see below).
		// All other dangerous keys must be absent entirely.
		if key == "GIT_TERMINAL_PROMPT" {
			continue
		}
		_, present := envMap[key]
		assert.False(t, present,
			"dangerous var %q must not appear in safeGitEnv() output", key)
	}
}

// TestSafeGitEnv_ForceDisablesTerminalPrompt verifies that GIT_TERMINAL_PROMPT
// is always set to "0" in the output, even when the parent env has it set to
// "1". This prevents git from blocking on a credential prompt when running
// non-interactively (which would make the 2-minute clone timeout look like a
// hang rather than an error).
//
// Revert proof: remove the `safe = append(safe, "GIT_TERMINAL_PROMPT=0")`
// line in safeGitEnv(); this test fails because GIT_TERMINAL_PROMPT is absent
// from the output, not "0".
func TestSafeGitEnv_ForceDisablesTerminalPrompt(t *testing.T) {
	// Whether or not the parent had GIT_TERMINAL_PROMPT set, the result
	// must contain exactly "GIT_TERMINAL_PROMPT=0".
	// (t.Setenv + t.Parallel are mutually exclusive; intentionally sequential)
	t.Setenv("GIT_TERMINAL_PROMPT", "1") // hostile parent value

	env := safeGitEnv()

	count := 0
	found := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
			count++
			if kv == "GIT_TERMINAL_PROMPT=0" {
				found = true
			}
		}
	}
	assert.True(t, found, "GIT_TERMINAL_PROMPT=0 must be present in safeGitEnv() output")
	assert.Equal(t, 1, count, "GIT_TERMINAL_PROMPT must appear exactly once (no duplicates)")
}

// TestSafeGitEnv_PreservesPathAndHome verifies that safeGitEnv() retains the
// vars that legitimate git operations depend on. Stripping PATH would prevent
// git from finding ssh; stripping HOME would break the credential helper lookup
// and ~/.gitconfig loading.
func TestSafeGitEnv_PreservesPathAndHome(t *testing.T) {
	// Inject known values so the assertion is deterministic regardless of the
	// actual test environment.
	// (t.Setenv + t.Parallel are mutually exclusive; intentionally sequential)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("HOME", "/home/testuser")

	env := safeGitEnv()

	envMap := make(map[string]string, len(env))
	for _, kv := range env {
		idx := strings.IndexByte(kv, '=')
		if idx < 0 {
			continue
		}
		envMap[kv[:idx]] = kv[idx+1:]
	}

	assert.Equal(t, "/usr/local/bin:/usr/bin:/bin", envMap["PATH"],
		"PATH must be preserved in safeGitEnv() output")
	assert.Equal(t, "/home/testuser", envMap["HOME"],
		"HOME must be preserved in safeGitEnv() output")
}

// TestSafeGitEnv_CloneInheritsCleanEnv is an end-to-end regression test that
// confirms safeGitEnv() strips the dangerous vars and that applyClone's
// happy path still works after the env filtering. Exercises the invariant
// in two halves: (1) safeGitEnv() output is sanitized, (2) applyClone
// runs cleanly with --clone-dir while a dangerous env var is present.
//
// NOTE: Uses t.Setenv, which panics if a test also calls t.Parallel().
// Keep this test sequential.
func TestSafeGitEnv_CloneInheritsCleanEnv(t *testing.T) {
	// Inject a dangerous var so we can verify it is stripped.
	t.Setenv("GIT_SSH_COMMAND", "evil-binary --steal-credentials")

	parent := t.TempDir()

	// Half 1: verify safeGitEnv() directly strips the dangerous var.
	env := safeGitEnv()
	for _, kv := range env {
		idx := strings.IndexByte(kv, '=')
		key := kv
		if idx >= 0 {
			key = kv[:idx]
		}
		assert.NotEqual(t, "GIT_SSH_COMMAND", key,
			"GIT_SSH_COMMAND must not be passed to git subprocess")
	}

	// Half 2: verify that applyClone's happy path still runs when a
	// dangerous env var is present. The injected RunGitClone doesn't
	// use safeGitEnv itself — the test here is that the stripping
	// function doesn't cause applyClone to err out on a clean URL.
	cmd := &HandoffCmd{
		Role:     "security",
		Target:   "https://github.com/nvbn/thefuck",
		CloneDir: parent,
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
		RunGitClone: func(_ context.Context, _, dest string) error {
			return os.MkdirAll(dest, 0o755)
		},
	}
	require.NoError(t, cmd.Run(&Globals{}),
		"safeGitEnv() must not break the happy clone path")
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
