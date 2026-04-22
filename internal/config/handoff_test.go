package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderTemplate_Substitution(t *testing.T) {
	src := []byte("Hello, {TARGET_NAME}! Repo: {TARGET_URL}.")
	rendered, unsub := RenderTemplate(src, map[string]string{
		"TARGET_NAME": "thefuck",
		"TARGET_URL":  "https://github.com/nvbn/thefuck",
	})
	assert.Equal(t, "Hello, thefuck! Repo: https://github.com/nvbn/thefuck.", string(rendered))
	assert.Empty(t, unsub)
}

func TestRenderTemplate_ReportsUnsubstituted(t *testing.T) {
	src := []byte("A: {TARGET_NAME}, B: {ECOSYSTEM}, C: {UNKNOWN_VAR}")
	rendered, unsub := RenderTemplate(src, map[string]string{
		"TARGET_NAME": "atuin",
	})
	// Unfilled placeholders remain literal so the author can see them.
	assert.Contains(t, string(rendered), "{ECOSYSTEM}")
	assert.Contains(t, string(rendered), "{UNKNOWN_VAR}")
	assert.Equal(t, []string{"ECOSYSTEM", "UNKNOWN_VAR"}, unsub)
}

func TestRenderTemplate_RepeatedPlaceholder(t *testing.T) {
	src := []byte("{NAME} says hi. {NAME} waves.")
	rendered, unsub := RenderTemplate(src, map[string]string{"NAME": "Alice"})
	assert.Equal(t, "Alice says hi. Alice waves.", string(rendered))
	assert.Empty(t, unsub)
}

func TestRenderTemplate_UnsubstitutedDeduped(t *testing.T) {
	src := []byte("{X} {Y} {X} {Y}")
	_, unsub := RenderTemplate(src, nil)
	assert.Equal(t, []string{"X", "Y"}, unsub, "each missing key reported once")
}

func TestRenderTemplate_DoesNotMatchLowercase(t *testing.T) {
	// Placeholders are ALL_CAPS; lowercase braces in the template
	// (e.g., JSON examples like `{"target": "..."}`) must not be
	// touched.
	src := []byte(`example: {"target": "{TARGET_URL}"}`)
	rendered, _ := RenderTemplate(src, map[string]string{"TARGET_URL": "https://x"})
	assert.Equal(t, `example: {"target": "https://x"}`, string(rendered))
}

func TestClassifyTarget(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    TargetKind
		wantStr string
	}{
		{"https URL", "https://github.com/foo/bar", TargetURL, "URL"},
		{"http URL", "http://example.com/proj", TargetURL, "URL"},
		{"absolute path", "/Users/sarah/code/proj", TargetPath, "path"},
		{"tilde path", "~/code/proj", TargetPath, "path"},
		{"relative dot-slash", "./thefuck", TargetPath, "path"},
		{"relative dotdot", "../thefuck", TargetPath, "path"},
		{"subpath with slash", "local/thefuck", TargetPath, "path"},
		{"bare name", "thefuck", TargetUnknown, "unknown"},
		{"git scheme rejected", "git://example.com/x", TargetUnknown, "unknown"},
		{"ssh form rejected", "git@github.com:foo/bar.git", TargetUnknown, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyTarget(tc.in), "classify %q", tc.in)
		})
	}
}

func TestInferNameFromURL(t *testing.T) {
	// Ordered slice (not map) so subtest output is deterministic.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"github repo", "https://github.com/nvbn/thefuck", "thefuck"},
		{"dot-git suffix stripped", "https://github.com/nvbn/thefuck.git", "thefuck"},
		{"trailing slash", "https://github.com/nvbn/thefuck/", "thefuck"},
		{"gitlab repo", "https://gitlab.com/cznic/sqlite", "sqlite"},
		{"no path", "https://github.com/", ""},
		{"empty string", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, InferNameFromURL(tc.in))
		})
	}
}

func TestInferNameFromPath(t *testing.T) {
	assert.Equal(t, "thefuck", InferNameFromPath("/Users/sarah/code/thefuck"))
	assert.Equal(t, "thefuck", InferNameFromPath("/Users/sarah/code/thefuck/"))
	assert.Equal(t, "code", InferNameFromPath("/code"))
}

// TestInferNameFromURL_RejectsUnsafeOutputs verifies that a URL whose
// derived name would be unsafe to use as a path component returns the
// empty string. This is the contract the helper now owns: callers
// can use the return value directly as a path component without an
// additional containment check (cmd reviewer F2).
func TestInferNameFromURL_RejectsUnsafeOutputs(t *testing.T) {
	// Ordered slice (not map) so subtest output is deterministic.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dotdot last segment", "https://github.com/foo/..", ""},
		{"dot last segment", "https://github.com/foo/.", ""},
		{"NUL in segment", "https://github.com/foo/bar%00evil", ""},
		// Embedded slash (URL-decoded by url.Parse) — strings.Split
		// already broke at the slash, so the last segment is "baz".
		// Document that the path segment is split AFTER decode.
		{"percent-encoded slash", "https://github.com/foo/bar%2fevil/baz", "baz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, InferNameFromURL(tc.in))
		})
	}
}

// TestInferNameFromPath_RejectsUnsafeOutputs verifies same contract
// for the path helper. Targets like "~/.." would decode to a basename
// of ".." and must be rejected.
func TestInferNameFromPath_RejectsUnsafeOutputs(t *testing.T) {
	// Ordered slice (not map) so subtest output is deterministic.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"dotdot basename", "/Users/sarah/..", ""},
		{"dot basename", "/Users/sarah/.", ""},
		{"NUL in path", "/foo/bar\x00", ""},
		{"root path", "/", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, InferNameFromPath(tc.in))
		})
	}
}

// TestIsSafePathComponent ensures the new shared helper matches the
// documented contract end-to-end.
func TestIsSafePathComponent(t *testing.T) {
	safe := []string{"foo", "foo.bar", "foo-bar", "foo_bar", "abc123", "name.git"}
	unsafe := []string{"", ".", "..", "foo/bar", "foo\\bar", "foo\x00bar"}
	for _, s := range safe {
		t.Run("safe-"+s, func(t *testing.T) {
			assert.True(t, isSafePathComponent(s), "%q should be considered safe", s)
		})
	}
	for _, u := range unsafe {
		// Include the value in the subtest name so failures identify which
		// input broke. Using the same name "unsafe" for every case makes
		// -run filtering useless and hides which input failed.
		t.Run("unsafe-"+fmt.Sprintf("%q", u), func(t *testing.T) {
			assert.False(t, isSafePathComponent(u), "%q must be rejected", u)
		})
	}
}

func TestHandoffSubstitutions_URLTarget(t *testing.T) {
	subs, err := HandoffSubstitutions("https://github.com/nvbn/thefuck", HandoffOverrides{})
	require.NoError(t, err)
	assert.Equal(t, "thefuck", subs["TARGET_NAME"])
	assert.Equal(t, "https://github.com/nvbn/thefuck", subs["TARGET_URL"])
	assert.Empty(t, subs["TARGET_PATH"])
}

func TestHandoffSubstitutions_PathTarget(t *testing.T) {
	subs, err := HandoffSubstitutions("/Users/sarah/code/thefuck", HandoffOverrides{})
	require.NoError(t, err)
	assert.Equal(t, "thefuck", subs["TARGET_NAME"])
	assert.Equal(t, "/Users/sarah/code/thefuck", subs["TARGET_PATH"])
	assert.Empty(t, subs["TARGET_URL"])
}

func TestHandoffSubstitutions_TildeExpands(t *testing.T) {
	// Agents running under user shells expect ~ expanded; the
	// template should receive a literal absolute path.
	subs, err := HandoffSubstitutions("~/code/thefuck", HandoffOverrides{})
	require.NoError(t, err)
	assert.NotContains(t, subs["TARGET_PATH"], "~")
	assert.True(t, filepath.IsAbs(subs["TARGET_PATH"]) || subs["TARGET_PATH"] == "~/code/thefuck",
		"expanded path should be absolute or (if $HOME unset) unchanged: got %q", subs["TARGET_PATH"])
}

func TestHandoffSubstitutions_OverridesApply(t *testing.T) {
	subs, err := HandoffSubstitutions("https://github.com/nvbn/thefuck", HandoffOverrides{
		Name:      "Thefuck",
		Path:      "/tmp/clone",
		Role:      "development",
		Ecosystem: "pypi",
		Intake:    "Could this leak credentials?",
	})
	require.NoError(t, err)
	assert.Equal(t, "Thefuck", subs["TARGET_NAME"])
	assert.Equal(t, "https://github.com/nvbn/thefuck", subs["TARGET_URL"])
	assert.Equal(t, "/tmp/clone", subs["TARGET_PATH"])
	assert.Equal(t, "development", subs["TARGET_ROLE"])
	assert.Equal(t, "pypi", subs["ECOSYSTEM"])
	assert.Equal(t, "Could this leak credentials?", subs["INTAKE_QUESTION"])
}

// TestHandoffSubstitutions_VersionPopulatedWhenSet covers the
// happy path: an explicit Version override surfaces verbatim as
// TARGET_VERSION. Templates use this to anchor analyst output
// (analyst_id attribution, synthesist version_scope) to the ref
// that was actually cloned.
func TestHandoffSubstitutions_VersionPopulatedWhenSet(t *testing.T) {
	subs, err := HandoffSubstitutions("https://github.com/X/Y", HandoffOverrides{
		Version: "v1.11.1",
	})
	require.NoError(t, err)
	assert.Equal(t, "v1.11.1", subs["TARGET_VERSION"])
}

// TestHandoffSubstitutions_VersionFallbackWhenEmpty covers the
// HEAD case: when no Version override is passed (the default for
// today's unversioned `signatory analyze X` flow), TARGET_VERSION
// renders as an explicit "(HEAD of default branch)" string. Avoids
// leaking literal `{TARGET_VERSION}` into the handoff body when
// the placeholder isn't filled — a footgun the existing
// substitution machinery handles for other placeholders by
// erroring out, but here we want the template to read cleanly
// for the common no-version case rather than refuse to render.
func TestHandoffSubstitutions_VersionFallbackWhenEmpty(t *testing.T) {
	subs, err := HandoffSubstitutions("https://github.com/X/Y", HandoffOverrides{})
	require.NoError(t, err)
	assert.Equal(t, "(HEAD of default branch)", subs["TARGET_VERSION"],
		"empty Version must produce an explicit fallback string, not literal {TARGET_VERSION}")
}

func TestHandoffSubstitutions_RejectsUninferrableName(t *testing.T) {
	// Bare string with no URL/path shape AND no --name override ⇒
	// error, because TARGET_NAME would land as the empty string and
	// the rendered handoff would be broken.
	_, err := HandoffSubstitutions("thefuck", HandoffOverrides{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TARGET_NAME")
	assert.Contains(t, err.Error(), "--name")
}

func TestHandoffSubstitutions_BareNameWithOverrideOK(t *testing.T) {
	subs, err := HandoffSubstitutions("thefuck", HandoffOverrides{Name: "thefuck", URL: "https://x"})
	require.NoError(t, err)
	assert.Equal(t, "thefuck", subs["TARGET_NAME"])
	assert.Equal(t, "https://x", subs["TARGET_URL"])
}

// ── adversarial tests ──────────────────────────────────────────────────────

// TestClassifyTarget_CaseInsensitiveScheme verifies that HTTPS:// and HTTP://
// (uppercase) are recognised as URLs. RFC 3986 §3.1 says scheme is
// case-insensitive; browsers and CLIs normalise case freely.
func TestClassifyTarget_CaseInsensitiveScheme(t *testing.T) {
	cases := []struct {
		in   string
		want TargetKind
	}{
		{"HTTPS://github.com/foo/bar", TargetURL},
		{"HTTP://example.com/proj", TargetURL},
		{"Https://example.com/proj", TargetURL},
		{"hTTPs://example.com/proj", TargetURL},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := ClassifyTarget(tc.in)
			assert.Equal(t, tc.want, got, "mixed-case scheme %q must be classified as URL", tc.in)
		})
	}
}

// TestExpandTilde_UserTildeUnchanged verifies that ~user paths are NOT
// expanded (Go's os.UserHomeDir doesn't support ~user syntax), and the
// input is returned unchanged so the caller can detect the unexpanded form
// rather than silently routing to the wrong path.
func TestExpandTilde_UserTildeUnchanged(t *testing.T) {
	got := expandTilde("~root/.ssh/id_rsa")
	assert.Equal(t, "~root/.ssh/id_rsa", got,
		"~user syntax must be returned unchanged (not expanded to current user's home)")
	// Critically, it must NOT expand to the current user's home directory.
	home, err := os.UserHomeDir()
	if err == nil {
		assert.False(t, strings.HasPrefix(got, home),
			"~root must not expand to current user's home %q", home)
	}
}

// TestRenderTemplate_ValueContainsPlaceholder verifies that substitution
// values containing {KEY} syntax are NOT recursively expanded. The renderer
// must be single-pass: a value of "{TARGET_URL}" does not trigger a second
// substitution round.
func TestRenderTemplate_ValueContainsPlaceholder(t *testing.T) {
	raw := []byte("name={TARGET_NAME} url={TARGET_URL}")
	subs := map[string]string{
		"TARGET_NAME": "{TARGET_URL}", // value looks like another placeholder
		"TARGET_URL":  "https://example.com",
	}
	rendered, unsub := RenderTemplate(raw, subs)
	// TARGET_NAME should expand to the literal string "{TARGET_URL}", not
	// to "https://example.com" (no recursive expansion).
	assert.Equal(t, "name={TARGET_URL} url=https://example.com", string(rendered))
	assert.Empty(t, unsub)
}

// TestRenderTemplate_ValueWithLiteralBraces verifies that a substitution
// value containing literal curly braces is written through intact and does
// not confuse the regex into a partial match.
func TestRenderTemplate_ValueWithLiteralBraces(t *testing.T) {
	raw := []byte("config={TARGET_NAME}")
	subs := map[string]string{"TARGET_NAME": `{"key":"value"}`}
	rendered, unsub := RenderTemplate(raw, subs)
	assert.Equal(t, `config={"key":"value"}`, string(rendered))
	assert.Empty(t, unsub)
}

// TestRenderTemplate_EmptyPlaceholder verifies that an empty brace pair {}
// is not matched by the placeholder regex (which requires at least one
// uppercase letter as the first character).
func TestRenderTemplate_EmptyPlaceholder(t *testing.T) {
	raw := []byte("empty={} normal={TARGET_NAME}")
	subs := map[string]string{"TARGET_NAME": "foo"}
	rendered, unsub := RenderTemplate(raw, subs)
	assert.Equal(t, "empty={} normal=foo", string(rendered))
	assert.Empty(t, unsub)
}

// TestRenderTemplate_VeryLongPlaceholderName verifies that a placeholder
// with a 10 000-character name does not cause a denial-of-service (hang,
// panic, or excessive allocation). The regex has no length bound, so we
// test that it terminates promptly.
func TestRenderTemplate_VeryLongPlaceholderName(t *testing.T) {
	longKey := strings.Repeat("A", 10_000)
	raw := []byte("{" + longKey + "}")
	// Must complete without panic or timeout.
	rendered, unsub := RenderTemplate(raw, nil)
	assert.Equal(t, raw, rendered, "unrecognised long placeholder must be left untouched")
	assert.Equal(t, []string{longKey}, unsub)
}

// TestHandoffSubstitutions_EmbeddedCredentialsInURL verifies that a URL
// containing embedded credentials (user:pass@) is passed through to
// TARGET_URL unchanged, but that the credentials do NOT leak into the
// inferred TARGET_NAME.
func TestHandoffSubstitutions_EmbeddedCredentialsInURL(t *testing.T) {
	target := "https://user:s3cr3t@github.com/org/repo"
	subs, err := HandoffSubstitutions(target, HandoffOverrides{})
	require.NoError(t, err)
	// TARGET_URL carries the full URL (credentials included — caller's
	// responsibility to audit templates).
	assert.Equal(t, target, subs["TARGET_URL"])
	// TARGET_NAME must be derived from the path, NOT from the userinfo.
	assert.Equal(t, "repo", subs["TARGET_NAME"])
	assert.NotContains(t, subs["TARGET_NAME"], "user")
	assert.NotContains(t, subs["TARGET_NAME"], "s3cr3t")
}
