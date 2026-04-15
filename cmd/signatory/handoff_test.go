package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

// runHandoff executes cmd.Run with stdout and stderr redirected to
// in-memory buffers so tests can assert on each stream independently.
// Returns (stdout, stderr, err).
func runHandoff(t *testing.T, cmd *HandoffCmd) (string, string, error) {
	t.Helper()
	// os.Stdout can't be swapped trivially; instead, route via a
	// temp file by setting --output, or capture via a pipe. For
	// tests that need stdout inspection, prefer --output and read
	// back the file. Tests here use that pattern.
	return "", "", cmd.Run(&Globals{})
}

// captureStderr redirects os.Stderr around fn and returns everything
// it emitted. Used to assert the informational report.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()

	fn()
	w.Close()
	return <-done
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
	_, _, err := runHandoff(t, cmd)
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

func TestHandoff_ProvenanceRequiresEcosystem(t *testing.T) {
	cmd := &HandoffCmd{
		Role:     "provenance",
		Target:   "https://github.com/nvbn/thefuck",
		Language: "python",
		Output:   filepath.Join(t.TempDir(), "out.md"),
		Quiet:    true,
	}
	_, _, err := runHandoff(t, cmd)
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
	_, _, err := runHandoff(t, cmd)
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
	_, _, err := runHandoff(t, cmd)
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
	_, _, err := runHandoff(t, cmd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--force")

	// File still has original content.
	body, _ := os.ReadFile(outPath)
	assert.Equal(t, "pre-existing", string(body))

	// With --force, overwrite succeeds.
	cmd.Force = true
	_, _, err = runHandoff(t, cmd)
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
	_, _, err := runHandoff(t, cmd)
	require.Error(t, err)
	// Error should name the missing substitution so the user knows
	// which flag to pass.
	assert.Contains(t, err.Error(), "TARGET_NAME")
}

func TestHandoff_InferTemplateName(t *testing.T) {
	cases := []struct {
		role, language, want string
	}{
		{"security", "python", "handoffs/security-review-v1.md"},
		{"security", "go", "handoffs/security-review-go-v1.md"},
		{"provenance", "python", "handoffs/provenance-review-v1.md"},
		{"provenance", "go", "handoffs/provenance-review-v1.md"},
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
	assert.True(t, looksLikeGitHubURL("https://github.com/foo/bar"))
}

func TestLooksLikeGitHubURL_HTTP(t *testing.T) {
	assert.True(t, looksLikeGitHubURL("http://github.com/foo/bar"))
}

func TestLooksLikeGitHubURL_CaseInsensitive(t *testing.T) {
	assert.True(t, looksLikeGitHubURL("HTTPS://GITHUB.COM/foo/bar"))
}

func TestLooksLikeGitHubURL_RejectsOtherHosts(t *testing.T) {
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
	cases := []struct {
		input, want string
	}{
		{"Go", "go"},
		{"go", "go"},
		{"Python", "python"},
		{"python", "python"},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, languageToFlavor(tc.input))
		})
	}
}

func TestLanguageToFlavor_UnknownReturnsEmpty(t *testing.T) {
	cases := []string{"Rust", "JavaScript", "", "Ruby"}
	for _, in := range cases {
		t.Run("input="+in, func(t *testing.T) {
			assert.Empty(t, languageToFlavor(in))
		})
	}
}

func TestFormatPrecheckReport_FullResult(t *testing.T) {
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

// --- --network-precheck path: end-to-end integration via injectable Source ---
//
// These tests exercise applyNetworkPrecheck by swapping newPrecheckSource
// for a factory that returns a fakePrecheckSource. That lets us drive
// the full pipeline (classify target → build detector → call Source →
// populate cmd.Ecosystem / cmd.Language) without an httptest.Server and
// without touching the real ghclient.Client.

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

// swapPrecheckSource replaces newPrecheckSource with a factory that
// returns src for the duration of the test. Uses t.Cleanup so the
// production factory is restored even when the test fails.
func swapPrecheckSource(t *testing.T, src ecosystem.Source) {
	t.Helper()
	orig := newPrecheckSource
	newPrecheckSource = func(string) ecosystem.Source { return src }
	t.Cleanup(func() { newPrecheckSource = orig })
}

func TestHandoff_NetworkPrecheck_AppliesEcosystem(t *testing.T) {
	// Repo with go.mod → detector returns EcosystemGo → precheck
	// fills --ecosystem=go for the provenance role.
	swapPrecheckSource(t, &fakePrecheckSource{
		Files:    []string{"go.mod", "README.md"},
		Language: "Go",
	})

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "provenance",
		Target:          "https://github.com/alecthomas/kong",
		Path:            "/tmp/kong",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
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
	swapPrecheckSource(t, &fakePrecheckSource{
		Files:    []string{"go.mod"},
		Language: "Go",
	})

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://github.com/alecthomas/kong",
		Path:            "/tmp/kong",
		TargetRole:      "validation",
		NetworkPrecheck: true,
		Output:          outPath,
		Quiet:           true,
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
	// yet — we should error before attempting any network call (the
	// fake shouldn't be consulted).
	called := false
	swapPrecheckSource(t, &fakePrecheckSource{
		Files: []string{"go.mod"},
		Err:   nil,
	})
	// Replace the swap with a version that records whether the
	// factory was invoked. If we wired the precheck correctly, a
	// non-GitHub host shouldn't even reach the factory.
	orig := newPrecheckSource
	newPrecheckSource = func(string) ecosystem.Source {
		called = true
		return &fakePrecheckSource{}
	}
	t.Cleanup(func() { newPrecheckSource = orig })

	cmd := &HandoffCmd{
		Role:            "security",
		Target:          "https://gitlab.com/foo/bar",
		Path:            "/tmp/bar",
		NetworkPrecheck: true,
		Output:          filepath.Join(t.TempDir(), "out.md"),
		Quiet:           true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "github.com URL")
	assert.False(t, called, "factory must not be invoked for non-GitHub targets")
}

func TestHandoff_NetworkPrecheck_DoesNotOverrideExplicit(t *testing.T) {
	// Detector says "ecosystem is go" but the user already passed
	// --ecosystem=pypi. Precheck must NOT clobber explicit flags.
	swapPrecheckSource(t, &fakePrecheckSource{
		Files:    []string{"go.mod"},
		Language: "Go",
	})

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
	}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Equal(t, "pypi", cmd.Ecosystem, "explicit --ecosystem must not be clobbered")
	assert.Equal(t, "go", cmd.Language, "explicit --language must not be clobbered")
}

func TestHandoff_NetworkPrecheck_ReportsToStderr(t *testing.T) {
	// With --quiet off, the precheck report lands on stderr so the
	// user can see what was detected. This is the transparency
	// contract — nothing about the network call should happen invisibly.
	swapPrecheckSource(t, &fakePrecheckSource{
		Files:    []string{"Cargo.toml"},
		Language: "Rust",
	})

	outPath := filepath.Join(t.TempDir(), "handoff.md")
	cmd := &HandoffCmd{
		Role:            "provenance",
		Target:          "https://github.com/atuinsh/atuin",
		Path:            "/tmp/atuin",
		NetworkPrecheck: true,
		Output:          outPath,
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
