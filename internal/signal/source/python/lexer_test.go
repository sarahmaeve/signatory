package python

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tok is a compact (kind, value) pair for asserting token streams
// without the position noise.
type tok struct {
	kind TokenKind
	val  string
}

// lexAll tokenizes src and returns the compact stream, failing the
// test on any lexer error. EOF is included so tests can assert the
// stream terminates cleanly.
func lexAll(t *testing.T, src string) []tok {
	t.Helper()
	toks, err := Lex([]byte(src))
	require.NoError(t, err)
	out := make([]tok, len(toks))
	for i, tk := range toks {
		out[i] = tok{tk.Kind, tk.Value}
	}
	return out
}

func TestLex_SimpleAssignment(t *testing.T) {
	t.Parallel()
	got := lexAll(t, "x = 1\n")
	assert.Equal(t, []tok{
		{TokenName, "x"},
		{TokenOp, "="},
		{TokenNumber, "1"},
		{TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}

func TestLex_MultiCharOperators(t *testing.T) {
	t.Parallel()
	// ^= is the named XOR-obfuscation threat; == and ** must not be
	// split into two single-char ops or the parser miscounts.
	got := lexAll(t, "a ^= b == c ** d\n")
	assert.Equal(t, []tok{
		{TokenName, "a"}, {TokenOp, "^="},
		{TokenName, "b"}, {TokenOp, "=="},
		{TokenName, "c"}, {TokenOp, "**"},
		{TokenName, "d"},
		{TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}

func TestLex_CommentIsNotCode(t *testing.T) {
	t.Parallel()
	// A comment containing exec( must produce no NAME/OP tokens —
	// otherwise the extractor counts commented-out attacks.
	got := lexAll(t, "x = 1  # exec('rm -rf /')\ny = 2\n")
	assert.Equal(t, []tok{
		{TokenName, "x"}, {TokenOp, "="}, {TokenNumber, "1"}, {TokenNewline, ""},
		{TokenName, "y"}, {TokenOp, "="}, {TokenNumber, "2"}, {TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}

func TestLex_StringsOpaqueAndPrefixed(t *testing.T) {
	t.Parallel()
	// String content must be opaque: exec( inside a literal is not a
	// call. Single, double, triple-quoted, and f/r/b prefixes all
	// collapse to one STRING token carrying the raw source slice.
	got := lexAll(t, "a = 'exec(1)'\nb = \"x\"\nc = '''multi\nline'''\nd = f\"hi{n}\"\ne = rb'\\x00'\n")
	assert.Equal(t, []tok{
		{TokenName, "a"}, {TokenOp, "="}, {TokenString, "'exec(1)'"}, {TokenNewline, ""},
		{TokenName, "b"}, {TokenOp, "="}, {TokenString, `"x"`}, {TokenNewline, ""},
		{TokenName, "c"}, {TokenOp, "="}, {TokenString, "'''multi\nline'''"}, {TokenNewline, ""},
		{TokenName, "d"}, {TokenOp, "="}, {TokenString, `f"hi{n}"`}, {TokenNewline, ""},
		{TokenName, "e"}, {TokenOp, "="}, {TokenString, `rb'\x00'`}, {TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}

func TestLex_IndentDedent(t *testing.T) {
	t.Parallel()
	// INDENT/DEDENT bracket the suite so the parser can tell
	// module-scope (import-time) code from code inside def/class.
	src := "def f():\n    x = 1\ny = 2\n"
	got := lexAll(t, src)
	assert.Equal(t, []tok{
		{TokenName, "def"}, {TokenName, "f"}, {TokenOp, "("}, {TokenOp, ")"}, {TokenOp, ":"}, {TokenNewline, ""},
		{TokenIndent, ""},
		{TokenName, "x"}, {TokenOp, "="}, {TokenNumber, "1"}, {TokenNewline, ""},
		{TokenDedent, ""},
		{TokenName, "y"}, {TokenOp, "="}, {TokenNumber, "2"}, {TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}

func TestLex_BlankAndCommentLinesProduceNoNewline(t *testing.T) {
	t.Parallel()
	// Blank lines and comment-only lines must not emit NEWLINE/INDENT
	// or the parser sees phantom empty statements.
	got := lexAll(t, "a = 1\n\n   \n# just a comment\nb = 2\n")
	assert.Equal(t, []tok{
		{TokenName, "a"}, {TokenOp, "="}, {TokenNumber, "1"}, {TokenNewline, ""},
		{TokenName, "b"}, {TokenOp, "="}, {TokenNumber, "2"}, {TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}

func TestLex_ImplicitLineJoinInBrackets(t *testing.T) {
	t.Parallel()
	// A call spanning lines inside () is one logical statement: no
	// interior NEWLINE, no INDENT from the continuation indentation.
	src := "subprocess.run(\n    'ls',\n    shell=True,\n)\n"
	got := lexAll(t, src)
	assert.Equal(t, []tok{
		{TokenName, "subprocess"}, {TokenOp, "."}, {TokenName, "run"}, {TokenOp, "("},
		{TokenString, "'ls'"}, {TokenOp, ","},
		{TokenName, "shell"}, {TokenOp, "="}, {TokenName, "True"}, {TokenOp, ","},
		{TokenOp, ")"}, {TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}

func TestLex_BackslashLineContinuation(t *testing.T) {
	t.Parallel()
	// Explicit \ continuation joins physical lines into one logical
	// statement (no NEWLINE at the backslash).
	got := lexAll(t, "x = 1 + \\\n    2\n")
	assert.Equal(t, []tok{
		{TokenName, "x"}, {TokenOp, "="}, {TokenNumber, "1"}, {TokenOp, "+"}, {TokenNumber, "2"},
		{TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}

func TestLex_EscapedQuoteInString(t *testing.T) {
	t.Parallel()
	// A backslash-escaped quote does not end the string.
	got := lexAll(t, `s = 'it\'s ok'`+"\n")
	assert.Equal(t, []tok{
		{TokenName, "s"}, {TokenOp, "="}, {TokenString, `'it\'s ok'`}, {TokenNewline, ""},
		{TokenEOF, ""},
	}, got)
}
