package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tok is a compact (kind, value) pair for asserting token streams
// without position noise.
type tok struct {
	kind TokenKind
	val  string
}

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

func TestLex_SimpleConst(t *testing.T) {
	t.Parallel()
	got := lexAll(t, "const x = 1;\n")
	assert.Equal(t, []tok{
		{TokenName, "const"}, {TokenName, "x"}, {TokenOp, "="},
		{TokenNumber, "1"}, {TokenOp, ";"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_MultiCharOperators: ^= is the XOR-deobfuscation threat
// primitive and => marks a function body (scope discrimination). Both
// must lex as single ops, never split.
func TestLex_MultiCharOperators(t *testing.T) {
	t.Parallel()
	got := lexAll(t, "a ^= b; const f = () => c; x === y;\n")
	assert.Equal(t, []tok{
		{TokenName, "a"}, {TokenOp, "^="}, {TokenName, "b"}, {TokenOp, ";"},
		{TokenName, "const"}, {TokenName, "f"}, {TokenOp, "="},
		{TokenOp, "("}, {TokenOp, ")"}, {TokenOp, "=>"}, {TokenName, "c"}, {TokenOp, ";"},
		{TokenName, "x"}, {TokenOp, "==="}, {TokenName, "y"}, {TokenOp, ";"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_CommentsAreNotCode: a // line comment and a /* */ block
// comment containing eval( must emit no NAME/OP tokens — otherwise the
// extractor counts commented-out attacks.
func TestLex_CommentsAreNotCode(t *testing.T) {
	t.Parallel()
	got := lexAll(t, "a = 1; // eval('rm -rf /')\n/* child_process.execSync(x) */ b = 2;\n")
	assert.Equal(t, []tok{
		{TokenName, "a"}, {TokenOp, "="}, {TokenNumber, "1"}, {TokenOp, ";"},
		{TokenName, "b"}, {TokenOp, "="}, {TokenNumber, "2"}, {TokenOp, ";"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_StringsOpaque: eval( inside a string literal is not a call.
// Single, double, and an escaped-quote string each collapse to one
// opaque STRING token carrying the raw source slice.
func TestLex_StringsOpaque(t *testing.T) {
	t.Parallel()
	got := lexAll(t, `a = 'eval(1)'; b = "x"; c = 'it\'s ok';`+"\n")
	assert.Equal(t, []tok{
		{TokenName, "a"}, {TokenOp, "="}, {TokenString, `'eval(1)'`}, {TokenOp, ";"},
		{TokenName, "b"}, {TokenOp, "="}, {TokenString, `"x"`}, {TokenOp, ";"},
		{TokenName, "c"}, {TokenOp, "="}, {TokenString, `'it\'s ok'`}, {TokenOp, ";"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_TemplateLiteralOpaque: a template literal — including a
// ${ ... } interpolation that itself contains a call and nested
// braces — is one opaque STRING token. Code inside ${} is a
// documented conservative miss (AST.md §4: a false negative is
// acceptable, a benign-construct false spike is not).
func TestLex_TemplateLiteralOpaque(t *testing.T) {
	t.Parallel()
	src := "x = `pre ${ eval(JSON.stringify({a:1})) } post`;\n"
	got := lexAll(t, src)
	assert.Equal(t, []tok{
		{TokenName, "x"}, {TokenOp, "="},
		{TokenString, "`pre ${ eval(JSON.stringify({a:1})) } post`"},
		{TokenOp, ";"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_NestedTemplateLiteral: a template nested inside another
// template's interpolation must still resolve to a single outer
// STRING token (matching backtick tracking through ${}).
func TestLex_NestedTemplateLiteral(t *testing.T) {
	t.Parallel()
	src := "y = `a${ `b${c}d` }e`;\n"
	got := lexAll(t, src)
	assert.Equal(t, []tok{
		{TokenName, "y"}, {TokenOp, "="},
		{TokenString, "`a${ `b${c}d` }e`"},
		{TokenOp, ";"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_RegexLiteralOpaque: a regex literal whose body spells eval(
// must not tokenize as a call. The body (including an escaped slash
// and a char class containing '/') and the flags are one opaque
// STRING token.
func TestLex_RegexLiteralOpaque(t *testing.T) {
	t.Parallel()
	got := lexAll(t, `const re = /eval\(x\)[/]/gi; return /a/;`+"\n")
	assert.Equal(t, []tok{
		{TokenName, "const"}, {TokenName, "re"}, {TokenOp, "="},
		{TokenString, `/eval\(x\)[/]/gi`}, {TokenOp, ";"},
		{TokenName, "return"}, {TokenString, "/a/"}, {TokenOp, ";"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_DivisionNotRegex: bare division must lex as '/' ops, not
// swallow the rest of the line as a regex. The standard heuristic:
// after a value (Name/Number/String/')'), '/' is division.
func TestLex_DivisionNotRegex(t *testing.T) {
	t.Parallel()
	got := lexAll(t, "z = a / b / c;\n")
	assert.Equal(t, []tok{
		{TokenName, "z"}, {TokenOp, "="},
		{TokenName, "a"}, {TokenOp, "/"}, {TokenName, "b"}, {TokenOp, "/"}, {TokenName, "c"},
		{TokenOp, ";"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_PrivateField: a #private member name lexes as one NAME so a
// class body parses cleanly.
func TestLex_PrivateField(t *testing.T) {
	t.Parallel()
	got := lexAll(t, "class C { #secret = 1; }\n")
	assert.Equal(t, []tok{
		{TokenName, "class"}, {TokenName, "C"}, {TokenOp, "{"},
		{TokenName, "#secret"}, {TokenOp, "="}, {TokenNumber, "1"}, {TokenOp, ";"},
		{TokenOp, "}"},
		{TokenEOF, ""},
	}, got)
}

// TestLex_TypeScriptLenient: TS type syntax (annotations, generics,
// as, decorators) must lex without error. We don't model types; the
// parser ignores them. The point is no crash and that real calls
// elsewhere stay visible.
func TestLex_TypeScriptLenient(t *testing.T) {
	t.Parallel()
	src := "@dec\nfunction f<T>(x: string): Promise<T> { return x as any; }\n"
	toks, err := Lex([]byte(src))
	require.NoError(t, err)
	// Spot-check the security-relevant anchors survived the type noise.
	var names []string
	for _, tk := range toks {
		if tk.Kind == TokenName {
			names = append(names, tk.Value)
		}
	}
	assert.Contains(t, names, "function")
	assert.Contains(t, names, "f")
	assert.Contains(t, names, "return")
}

// TestLex_UnterminatedStringLenient: a trust extractor must make
// progress on malformed/adversarial input, never abort the file. An
// unterminated string ends at newline (or EOF) without error.
func TestLex_UnterminatedStringLenient(t *testing.T) {
	t.Parallel()
	toks, err := Lex([]byte("s = 'oops\nb = 2;\n"))
	require.NoError(t, err)
	require.NotEmpty(t, toks)
	assert.Equal(t, TokenEOF, toks[len(toks)-1].Kind)
}
