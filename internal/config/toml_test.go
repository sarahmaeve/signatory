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

// ---------- adversarial / security tests added in security pass ----------

// TestDecodeTOML_InputSizeCap ensures the parser refuses inputs beyond
// the documented 1 MB limit so a symlinked /dev/zero or crafted
// multi-gigabyte config cannot OOM the process.  The cap is enforced
// via io.LimitReader inside decodeTOML — this test verifies that the
// (1MB+1) boundary triggers the "input too large" error rather than
// reading indefinitely.
func TestDecodeTOML_InputSizeCap(t *testing.T) {
	// 1 MB + 1 byte of data; the cap is exactly 1 MB, so this must error.
	// Use a strings.Reader over a large repeated comment so the data is
	// syntactically valid TOML — we want the size check, not a parse error.
	const megabyte = 1 << 20
	big := strings.Repeat("# padding\n", (megabyte/10)+1) // > 1MB of comment
	_, err := decodeTOML(strings.NewReader(big))
	require.Error(t, err, "parser must reject input larger than the size cap")
	assert.Contains(t, err.Error(), "too large",
		"error should explain the size cap, not produce a parse error from truncated data")
}

// TestDecodeTOML_DELByteInString verifies that the DEL control character
// (0x7F) is rejected when it appears inside a quoted string.  DEL is a
// non-printing control byte that has no business in a filesystem path
// and most commonly indicates binary content or adversarial input.
// The control-char check in parseString only tested c < 0x20; DEL (0x7F)
// slips through unless explicitly guarded.
func TestDecodeTOML_DELByteInString(t *testing.T) {
	// 0x7F embedded in the middle of an otherwise-valid string.
	src := "k = \"abc\x7fdef\""
	_, err := decodeTOML(strings.NewReader(src))
	require.Error(t, err, "DEL byte inside a string must be rejected")
	assert.Contains(t, err.Error(), "control character",
		"error should identify the DEL byte as a control character")
}

// TestDecodeTOML_NULByteInString confirms NUL (0x00) inside a quoted string
// is caught by the existing control-char guard (< 0x20).  NUL in a
// filesystem path would silently truncate the path on many OS interfaces;
// it must never pass through the parser.
func TestDecodeTOML_NULByteInString(t *testing.T) {
	src := "k = \"abc\x00def\""
	_, err := decodeTOML(strings.NewReader(src))
	require.Error(t, err, "NUL byte inside string must be rejected")
	assert.Contains(t, err.Error(), "control character")
}

// TestDecodeTOML_CROnlyLineEnding checks that a file using bare CR
// line endings (old Mac / some binary-edited files) is normalized and
// parsed correctly rather than returning a confusing "expected newline"
// error.  The existing CRLF normalization only replaces \r\n; a bare \r
// needs the same treatment.
func TestDecodeTOML_CROnlyLineEnding(t *testing.T) {
	// "\r" between two valid assignments — both must parse.
	src := "a = \"1\"\rb = \"2\"\r"
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err, "bare CR line endings should be normalized, not error")
	assert.Equal(t, "1", out["a"].String)
	assert.Equal(t, "2", out["b"].String)
}

// TestDecodeTOML_HashInsideStringNotComment verifies that a '#' character
// inside a double-quoted string is part of the value, not a comment
// delimiter.  This is the most important semantic invariant for the
// comment-injection attack class.
func TestDecodeTOML_HashInsideStringNotComment(t *testing.T) {
	// The '#' must survive verbatim; everything after the closing quote is
	// a comment and is discarded.
	src := `k = "abc # not a comment" # real comment`
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err)
	assert.Equal(t, "abc # not a comment", out["k"].String)
}

// TestDecodeTOML_CommentInjectionAfterValue confirms that a second key=value
// on the line after a trailing comment is correctly parsed as a separate
// statement.  This is not a bug — it documents the intended grammar: the
// comment runs to EOL, and the next line starts a new statement.
func TestDecodeTOML_CommentInjectionAfterValue(t *testing.T) {
	src := "good = \"ok\" # comment\nevil = \"also-ok\"\n"
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err,
		"second key on a separate line must parse (comment ends at newline)")
	assert.Equal(t, "ok", out["good"].String)
	assert.Equal(t, "also-ok", out["evil"].String)
}

// TestDecodeTOML_EscapeBoundary exercises the three tricky escape sequences
// that are easiest to mis-implement at string boundaries.
func TestDecodeTOML_EscapeBoundary(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		want  string
		isErr bool
	}{
		// "a\" — backslash-escaped quote then EOF: unterminated string.
		{"trailing-escaped-quote", `k = "a\"`, "", true},
		// "a\\" — two backslashes: yields one backslash, closing quote closes.
		{"double-backslash", `k = "a\\"`, `a\`, false},
		// "a\\\"" — backslash-backslash-escaped-quote: yields a\ then ".
		{"backslash-then-escaped-quote", `k = "a\\\""`, `a\"`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := decodeTOML(strings.NewReader(tc.src))
			if tc.isErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, out["k"].String)
			}
		})
	}
}

// TestDecodeTOML_WhitespaceTricksInKeys verifies that non-ASCII whitespace
// bytes (NBSP U+00A0, zero-width space U+200B) are NOT accepted as key
// characters.  They would make two visually-identical keys parse as
// different strings — a spoofing vector.
func TestDecodeTOML_WhitespaceTricksInKeys(t *testing.T) {
	// U+00A0 NO-BREAK SPACE (UTF-8: 0xC2 0xA0) where a space would be.
	// The parser should error trying to parse a key starting with 0xC2.
	nbspKey := "k\xc2\xa0= \"v\""
	_, err := decodeTOML(strings.NewReader(nbspKey))
	require.Error(t, err,
		"NBSP in a key name must be rejected (it is not an ASCII key char)")

	// U+200B ZERO-WIDTH SPACE (UTF-8: 0xE2 0x80 0x8B) inside a key.
	zwspKey := "k\xe2\x80\x8bey = \"v\""
	_, err = decodeTOML(strings.NewReader(zwspKey))
	require.Error(t, err,
		"zero-width space in a key name must be rejected")
}

// TestDecodeTOML_UnicodePseudoNewlinesInStrings confirms that Unicode line
// separators (U+2028 LS, U+2029 PS) inside a quoted string are treated as
// ordinary bytes and passed through, not as line terminators.  The grammar
// defines only \n (after CRLF normalization) as a line terminator; treating
// LS/PS as line terminators would be a divergence that breaks non-ASCII paths.
func TestDecodeTOML_UnicodePseudoNewlinesInStrings(t *testing.T) {
	// LS = 0xE2 0x80 0xA8, PS = 0xE2 0x80 0xA9 — multi-byte sequences,
	// all bytes are >= 0x80 so they pass the control-char guard.
	ls := "\xe2\x80\xa8"
	ps := "\xe2\x80\xa9"
	src := `k = "path` + ls + ps + `end"`
	out, err := decodeTOML(strings.NewReader(src))
	require.NoError(t, err, "LS/PS inside a string are ordinary bytes in our grammar")
	assert.Equal(t, "path"+ls+ps+"end", out["k"].String)
}

// TestDecodeTOML_DuplicateKeyErrorDoesNotLeakValue confirms that the
// duplicate-key error message includes the key name (useful for
// diagnostics) but NOT the conflicting value (which could be a path
// containing sensitive directory names or tokens).
func TestDecodeTOML_DuplicateKeyErrorDoesNotLeakValue(t *testing.T) {
	src := "secret = \"s3cr3t-p@ssword\"\nsecret = \"other\"\n"
	_, err := decodeTOML(strings.NewReader(src))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "secret", "key name should appear in error")
	assert.NotContains(t, err.Error(), "s3cr3t-p@ssword",
		"value must NOT appear in the duplicate-key error message")
}

// TestDecodeTOML_LargeSingleArrayIsRejected verifies that a single array
// with an absurd element count is caught by the input-size cap rather than
// allocating an unbounded slice.  A 1 M-element array of empty strings is
// ~8 MB of raw text, well over the 1 MB cap.
func TestDecodeTOML_LargeSingleArrayIsRejected(t *testing.T) {
	// Build an array whose text encoding exceeds 1 MB.
	// Each element "x", plus comma and space = 5 bytes; 300 000 elements ≈ 1.5 MB.
	const n = 300_000
	var b strings.Builder
	b.WriteString("k = [")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"x"`)
	}
	b.WriteString("]")
	_, err := decodeTOML(strings.NewReader(b.String()))
	require.Error(t, err, "array whose text representation exceeds the size cap must be rejected")
	assert.Contains(t, err.Error(), "too large")
}
