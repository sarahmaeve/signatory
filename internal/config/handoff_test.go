package config

import (
	"path/filepath"
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
	cases := map[string]string{
		"https://github.com/nvbn/thefuck":     "thefuck",
		"https://github.com/nvbn/thefuck.git": "thefuck",
		"https://github.com/nvbn/thefuck/":    "thefuck",
		"https://gitlab.com/cznic/sqlite":     "sqlite",
		"https://github.com/":                 "",
		"":                                    "",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, InferNameFromURL(in))
		})
	}
}

func TestInferNameFromPath(t *testing.T) {
	assert.Equal(t, "thefuck", InferNameFromPath("/Users/sarah/code/thefuck"))
	assert.Equal(t, "thefuck", InferNameFromPath("/Users/sarah/code/thefuck/"))
	assert.Equal(t, "code", InferNameFromPath("/code"))
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
