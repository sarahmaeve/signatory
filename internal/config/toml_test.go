package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TOML subset parser is the narrowest piece of the config
// package's attack surface — it reads user-supplied bytes and turns
// them into structured data. Tests exercise each grammar production
// (scalar string, array, comments, whitespace) and each documented
// error path. A regression here could mean a config with malformed
// content is accepted and causes unexpected behavior; these tests
// keep that door closed.

func TestDecodeTOML_EmptyInput(t *testing.T) {
	t.Run("truly empty", func(t *testing.T) {
		out, err := decodeTOML(strings.NewReader(""))
		require.NoError(t, err)
		assert.Empty(t, out)
	})
	t.Run("only whitespace and comments", func(t *testing.T) {
		src := "# this is a comment\n\n\t\n# another\n"
		out, err := decodeTOML(strings.NewReader(src))
		require.NoError(t, err)
		assert.Empty(t, out)
	})
}

func TestDecodeTOML_ScalarString(t *testing.T) {
	out, err := decodeTOML(strings.NewReader(`name = "signatory"`))
	require.NoError(t, err)
	require.Contains(t, out, "name")
	v := out["name"]
	assert.False(t, v.IsArray)
	assert.Equal(t, "signatory", v.String)
	assert.Equal(t, 1, v.Line)
}

func TestDecodeTOML_Array(t *testing.T) {
	t.Run("single-line", func(t *testing.T) {
		src := `templates = ["a", "b", "c"]`
		out, err := decodeTOML(strings.NewReader(src))
		require.NoError(t, err)
		v := out["templates"]
		assert.True(t, v.IsArray)
		assert.Equal(t, []string{"a", "b", "c"}, v.Array)
	})
	t.Run("multi-line with trailing comma", func(t *testing.T) {
		src := "templates = [\n  \"a\",\n  \"b\",\n  \"c\",\n]\n"
		out, err := decodeTOML(strings.NewReader(src))
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "c"}, out["templates"].Array)
	})
	t.Run("empty", func(t *testing.T) {
		out, err := decodeTOML(strings.NewReader(`filestores = []`))
		require.NoError(t, err)
		v := out["filestores"]
		assert.True(t, v.IsArray)
		assert.Empty(t, v.Array)
	})
	t.Run("comments inside array", func(t *testing.T) {
		src := "templates = [\n  # first\n  \"a\",\n  # second\n  \"b\",\n]\n"
		out, err := decodeTOML(strings.NewReader(src))
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b"}, out["templates"].Array)
	})
}

func TestDecodeTOML_StringEscapes(t *testing.T) {
	src := `k = "a\"b\\c\nd\te\rf"`
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err)
	assert.Equal(t, "a\"b\\c\nd\te\rf", out["k"].String)
}

func TestDecodeTOML_Comments(t *testing.T) {
	t.Run("full-line", func(t *testing.T) {
		src := "# hello\nk = \"v\"\n"
		out, err := decodeTOML(strings.NewReader(src))
		require.NoError(t, err)
		assert.Equal(t, "v", out["k"].String)
		assert.Equal(t, 2, out["k"].Line)
	})
	t.Run("trailing after value", func(t *testing.T) {
		src := `k = "v"  # trailing comment`
		out, err := decodeTOML(strings.NewReader(src))
		require.NoError(t, err)
		assert.Equal(t, "v", out["k"].String)
	})
}

func TestDecodeTOML_CRLFNormalization(t *testing.T) {
	src := "a = \"1\"\r\nb = \"2\"\r\n"
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err)
	assert.Equal(t, "1", out["a"].String)
	assert.Equal(t, "2", out["b"].String)
}

func TestDecodeTOML_KeyCharacters(t *testing.T) {
	// Bare keys include letters, digits, underscore, hyphen.
	src := "abc_123 = \"ok\"\nx-y = \"yo\"\n"
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err)
	assert.Equal(t, "ok", out["abc_123"].String)
	assert.Equal(t, "yo", out["x-y"].String)
}

func TestDecodeTOML_LineNumbers(t *testing.T) {
	// Line numbers get used in Config's validation errors; make sure
	// they track across blank lines and comments.
	src := "# header\n\na = \"1\"\n# note\n\nb = [\"2\"]\n"
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err)
	assert.Equal(t, 3, out["a"].Line)
	assert.Equal(t, 6, out["b"].Line)
}

func TestDecodeTOML_Errors(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSub string
	}{
		{"missing equals", `name`, "expected '='"},
		{"missing value", `name = `, "unexpected end of file"},
		{"unquoted string", `name = hello`, "expected '\"' or '['"},
		{"unterminated string", `name = "hello`, "unterminated string"},
		{"newline in string", "name = \"hel\nlo\"", "unterminated string"},
		{"unknown escape", `name = "a\x"`, "unknown escape"},
		{"control char in string", "name = \"a\x01b\"", "control character"},
		{"backslash at EOF", `name = "a\`, "backslash at end of file"},
		{"unterminated array", `x = ["a"`, "unterminated array"},
		{"missing comma in array", `x = ["a" "b"]`, "expected ',' or ']'"},
		{"array with non-string", `x = [1]`, "expected '\"'"},
		{"duplicate key", "a = \"1\"\na = \"2\"\n", "duplicate key"},
		{"leading int value", `x = 42`, "expected '\"' or '['"},
		{"empty key", `= "v"`, "expected key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeTOML(strings.NewReader(tc.src))
			require.Error(t, err, "expected error for %q", tc.src)
			assert.Contains(t, err.Error(), tc.wantSub)
		})
	}
}

func TestDecodeTOML_UTF8InStrings(t *testing.T) {
	// Filesystem paths legitimately contain UTF-8 bytes; the parser
	// must pass them through untouched.
	src := `path = "/Users/café/proj"`
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err)
	assert.Equal(t, "/Users/café/proj", out["path"].String)
}
