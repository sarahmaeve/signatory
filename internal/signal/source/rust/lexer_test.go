package rust

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenize runs Lex and strips the trailing EOF token, returning a
// flat slice of (Kind, Value) pairs convenient for assertion. The
// lexer's nil-error contract is asserted here so per-test fixtures
// don't repeat it.
func tokenize(t *testing.T, src string) []Token {
	t.Helper()
	toks, err := Lex([]byte(src))
	require.NoError(t, err, "Lex must never error on adversarial-or-not input — AST.md §4")
	require.NotEmpty(t, toks)
	last := toks[len(toks)-1]
	assert.Equal(t, TokenEOF, last.Kind, "lexer must terminate with TokenEOF")
	return toks[:len(toks)-1]
}

func TestLex_EmptyInput_OnlyEOF(t *testing.T) {
	t.Parallel()
	toks, err := Lex(nil)
	require.NoError(t, err)
	require.Len(t, toks, 1)
	assert.Equal(t, TokenEOF, toks[0].Kind)
}

func TestLex_BasicIdentifiers(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, "fn main() { let x = 1; }")
	want := []Token{
		{Kind: TokenName, Value: "fn"},
		{Kind: TokenName, Value: "main"},
		{Kind: TokenOp, Value: "("},
		{Kind: TokenOp, Value: ")"},
		{Kind: TokenOp, Value: "{"},
		{Kind: TokenName, Value: "let"},
		{Kind: TokenName, Value: "x"},
		{Kind: TokenOp, Value: "="},
		{Kind: TokenNumber, Value: "1"},
		{Kind: TokenOp, Value: ";"},
		{Kind: TokenOp, Value: "}"},
	}
	assertTokens(t, want, toks)
}

func TestLex_OrdinaryString(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `let s = "hello world";`)
	assertContainsString(t, toks, `"hello world"`)
}

// TestLex_OrdinaryString_NewlinesEmbedded confirms Rust strings can
// span newlines without terminating early. (Distinguishes Rust from
// JS/TS, where a newline in a non-template string terminates it.)
func TestLex_OrdinaryString_NewlinesEmbedded(t *testing.T) {
	t.Parallel()
	src := "let s = \"line1\nline2\";"
	toks := tokenize(t, src)
	assertContainsString(t, toks, "\"line1\nline2\"")
	// Line number on the closing `;` token must reflect the embedded newline.
	last := toks[len(toks)-1]
	assert.Equal(t, ";", last.Value)
	assert.Equal(t, 2, last.Line, "newline inside string must still advance the line counter")
}

// TestLex_RawString_NoHashes covers r"..." with no `#` padding.
func TestLex_RawString_NoHashes(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `let s = r"raw \n still literal";`)
	assertContainsString(t, toks, `r"raw \n still literal"`)
}

// TestLex_RawString_WithHashes covers r#"..."# and r##"..."##. The
// closing hash run must match the opening — partial closes don't
// terminate.
func TestLex_RawString_WithHashes(t *testing.T) {
	t.Parallel()
	src := `let s = r#"has "quotes" inside"#;`
	toks := tokenize(t, src)
	assertContainsString(t, toks, `r#"has "quotes" inside"#`)
}

func TestLex_RawString_NestedHashCount(t *testing.T) {
	t.Parallel()
	// Inside the r##"..."## literal there's a `"#` sequence that
	// would close r#"..."# but NOT r##"..."##. The lexer must
	// match the hash counts exactly.
	src := `let s = r##"contains "# but not closed"##;`
	toks := tokenize(t, src)
	assertContainsString(t, toks, `r##"contains "# but not closed"##`)
}

// TestLex_RawString_CatalogTokenInside is the security property:
// catalog tokens spelled inside a raw string must not tokenize as
// code (AST.md §4 "opaque tokens" lesson). The let/s tokens BEFORE
// the raw string are legitimate; only the bytes inside the raw
// string's payload must stay opaque — checked by confirming the
// catalog-shaped names `std`, `env`, `var`, and the env-name literal
// don't appear as NAME tokens.
func TestLex_RawString_CatalogTokenInside(t *testing.T) {
	t.Parallel()
	src := `let s = r#"std::env::var("AWS_SECRET_ACCESS_KEY")"#;`
	toks := tokenize(t, src)
	forbidden := map[string]struct{}{
		"std": {}, "env": {}, "var": {}, "AWS_SECRET_ACCESS_KEY": {},
	}
	for _, tk := range toks {
		if _, banned := forbidden[tk.Value]; banned {
			t.Fatalf("catalog token %q leaked out of the raw string as a "+
				"%s token — would produce a false EnvCredentialReads spike",
				tk.Value, tk.Kind)
		}
	}
}

func TestLex_ByteString(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `let b = b"bytes";`)
	assertContainsString(t, toks, `b"bytes"`)
}

func TestLex_ByteRawString(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `let b = br#"raw "bytes""#;`)
	assertContainsString(t, toks, `br#"raw "bytes""#`)
}

func TestLex_CharLiteral_SimpleChar(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `let c = 'a';`)
	assertContainsString(t, toks, `'a'`)
}

func TestLex_CharLiteral_EscapeSequences(t *testing.T) {
	t.Parallel()
	for _, lit := range []string{`'\n'`, `'\\'`, `'\''`, `'\0'`} {
		toks := tokenize(t, "let c = "+lit+";")
		assertContainsString(t, toks, lit)
	}
}

func TestLex_ByteCharLiteral(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `let c = b'X';`)
	assertContainsString(t, toks, `b'X'`)
}

// TestLex_Lifetime_StaticAndNamed pins the char-vs-lifetime
// disambiguator. 'static and 'a are lifetimes (TokenName, leading
// apostrophe preserved); 'a' is a char literal.
func TestLex_Lifetime_StaticAndNamed(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `fn f<'a>(x: &'a str, y: &'static str) {}`)

	have := tokenSlice(toks)
	assert.Contains(t, have, Token{Kind: TokenName, Value: "'a", Line: 1},
		"'a in a generic position must lex as a lifetime")
	assert.Contains(t, have, Token{Kind: TokenName, Value: "'static", Line: 1},
		"'static must lex as a single name token")
}

func TestLex_Lifetime_VsCharLiteral(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `let x = 'a'; let r: &'a str = "x";`)
	have := tokenSlice(toks)
	assert.Contains(t, have, Token{Kind: TokenString, Value: "'a'", Line: 1},
		"'a' (closed apostrophe at offset 2) must lex as a char literal STRING token")
	assert.Contains(t, have, Token{Kind: TokenName, Value: "'a", Line: 1},
		"'a in a type-binding position must lex as a lifetime NAME token")
}

// TestLex_MacroBangAndBrackets pins the `!` after a name as a
// separate Op token. The parser uses (NAME, Op `!`, opener) to
// recognize macro calls; the lexer just emits the pieces.
func TestLex_MacroBangAndBrackets(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		`println!("hello");`,
		`vec![1, 2, 3];`,
		`thread_local! { static X: i32 = 0; };`,
	} {
		toks := tokenize(t, src)
		// First token is the macro name; second must be `!`.
		require.GreaterOrEqual(t, len(toks), 3, "src=%q", src)
		assert.Equal(t, TokenName, toks[0].Kind, "src=%q", src)
		assert.Equal(t, Token{Kind: TokenOp, Value: "!", Line: 1}, toks[1],
			"the `!` after a macro name must lex as a single-char Op (src=%q)", src)
	}
}

// TestLex_XorAssignOp is the load-bearing operator test for the
// XORAssignments Counts field: `data[i] ^= key[i % key.len()]` must
// produce a `^=` token exactly once.
func TestLex_XorAssignOp(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `data[i] ^= key[i % key.len()];`)
	have := tokenSlice(toks)
	count := 0
	for _, tk := range have {
		if tk.Kind == TokenOp && tk.Value == "^=" {
			count++
		}
	}
	assert.Equal(t, 1, count,
		"a single `^=` must lex as one TokenOp `^=`, not `^` + `=`")
}

// TestLex_PathSeparator pins `::` as a single Op. Critical for
// resolving std::env::var to a single path expression in the parser.
func TestLex_PathSeparator(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `use std::env;`)
	have := tokenSlice(toks)
	pathOps := 0
	for _, tk := range have {
		if tk.Kind == TokenOp && tk.Value == "::" {
			pathOps++
		}
	}
	assert.Equal(t, 1, pathOps, "`::` must lex as a single Op")
}

// TestLex_NestedGenericsCloseAsTwoTokens documents the deliberate
// choice to lex `>>` as two `>` tokens, not as a right-shift op.
// Nested-generic closings (Vec<HashMap<K, V>>) are pervasive and
// shifts are rare in trust-relevant code; emitting two `>` tokens
// keeps the parser's nesting tracker unconfused (AST.md §4).
func TestLex_NestedGenericsCloseAsTwoTokens(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `let v: Vec<Box<i32>> = vec![];`)
	have := tokenSlice(toks)
	closes := 0
	for _, tk := range have {
		if tk.Kind == TokenOp && tk.Value == ">" {
			closes++
		}
		assert.NotEqualf(t, ">>", tk.Value,
			"`>>` must NOT lex as a single op — Rust generics conflict makes it the parser's problem")
	}
	assert.Equal(t, 2, closes,
		"`>>` at the end of nested generics must produce exactly two `>` tokens")
}

// TestLex_LineComment_Skipped and TestLex_BlockComment_Skipped cover
// the two comment kinds. Block comments must nest (Rust-specific).
func TestLex_LineComment_Skipped(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, "let x = 1; // std::env::var(\"AWS_SECRET_ACCESS_KEY\")\nlet y = 2;")
	for _, tk := range toks {
		assert.NotEqualf(t, "AWS_SECRET_ACCESS_KEY", tk.Value,
			"line comments must hide catalog tokens from the lexer")
	}
}

func TestLex_BlockComment_NestedSkipped(t *testing.T) {
	t.Parallel()
	src := "let x = /* outer /* inner with std::env::var */ outer */ 1;"
	toks := tokenize(t, src)
	for _, tk := range toks {
		assert.NotEqualf(t, "env", tk.Value,
			"nested block comments must hide catalog tokens (Rust-specific nesting)")
	}
	// The lexer should produce `let x = 1 ;` (5 tokens) — proof
	// the whole nested comment was consumed as one.
	require.Len(t, toks, 5)
}

func TestLex_BlockComment_UnterminatedAtEOF(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, "let x = 1; /* unterminated")
	// Lenient: lexer must reach EOF without erroring.
	// Tokens before the comment must still emit.
	assert.NotEmpty(t, toks)
	assert.Equal(t, "let", toks[0].Value)
}

func TestLex_DocComment_Skipped(t *testing.T) {
	t.Parallel()
	// /// is a line doc comment, //! an inner-doc comment, /** */ block doc.
	src := "/// doc with std::env::var\nfn f() {}\n//! inner doc\n/** outer */"
	toks := tokenize(t, src)
	for _, tk := range toks {
		assert.NotEqualf(t, "env", tk.Value,
			"doc comments must hide catalog tokens just like ordinary comments")
	}
}

// TestLex_Number_TypeSuffixGreedy keeps `1u32` and `1.5f64` as one
// Number token rather than fragmenting. The parser doesn't model
// types but the non-merging matters for parse position tracking.
func TestLex_Number_TypeSuffixGreedy(t *testing.T) {
	t.Parallel()
	for _, lit := range []string{"1u32", "1.5f64", "0xff", "0b1010", "1_000_000", "1e10"} {
		toks := tokenize(t, "let n = "+lit+";")
		// The number must appear as a single Number token.
		found := false
		for _, tk := range toks {
			if tk.Kind == TokenNumber && tk.Value == lit {
				found = true
				break
			}
		}
		assert.Truef(t, found, "%q must lex as a single Number token", lit)
	}
}

// TestLex_AttributesTokenizeOpaquely confirms #[derive(Debug)] and
// #![allow(...)] lex into Op `#`, Op `[` or Op `!`+`[`, name, ... —
// the parser stage decides what to do with attribute content.
func TestLex_AttributesTokenizeOpaquely(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `#[derive(Debug)]`)
	require.GreaterOrEqual(t, len(toks), 5)
	assert.Equal(t, Token{Kind: TokenOp, Value: "#", Line: 1}, toks[0])
	assert.Equal(t, Token{Kind: TokenOp, Value: "[", Line: 1}, toks[1])
	assert.Equal(t, "derive", toks[2].Value)
}

func TestLex_InnerAttribute(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `#![allow(dead_code)]`)
	// #! is two tokens: # then !.
	require.GreaterOrEqual(t, len(toks), 6)
	assert.Equal(t, "#", toks[0].Value)
	assert.Equal(t, "!", toks[1].Value)
	assert.Equal(t, "[", toks[2].Value)
}

// TestLex_RangeOps confirms `..` and `..=` are both recognized — the
// XOR loop in the Trapdoor build.rs fixture uses `0..data.len()`.
func TestLex_RangeOps(t *testing.T) {
	t.Parallel()
	toks := tokenize(t, `for i in 0..data.len() { let _ = 0..=255; }`)
	have := tokenSlice(toks)
	exclCount, inclCount := 0, 0
	for _, tk := range have {
		switch {
		case tk.Kind == TokenOp && tk.Value == "..":
			exclCount++
		case tk.Kind == TokenOp && tk.Value == "..=":
			inclCount++
		}
	}
	assert.Equal(t, 1, exclCount, "`..` must lex once")
	assert.Equal(t, 1, inclCount, "`..=` must lex once")
}

// ============================================================
// helpers
// ============================================================

// assertTokens compares (Kind, Value) pairs only — Line is set to 1
// for the lexer's default single-line cases and is asserted
// separately where needed.
func assertTokens(t *testing.T, want, got []Token) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		assert.Equalf(t, want[i].Kind, got[i].Kind,
			"token %d kind mismatch (value=%q)", i, got[i].Value)
		assert.Equalf(t, want[i].Value, got[i].Value,
			"token %d value mismatch", i)
	}
}

// assertContainsString reports failure if no TokenString in got has
// the expected value. Used by the string-literal coverage tests
// where surrounding tokens vary by context.
func assertContainsString(t *testing.T, got []Token, want string) {
	t.Helper()
	for _, tk := range got {
		if tk.Kind == TokenString && tk.Value == want {
			return
		}
	}
	var vals []string
	for _, tk := range got {
		if tk.Kind == TokenString {
			vals = append(vals, tk.Value)
		}
	}
	t.Fatalf("expected TokenString %q in stream; got STRING tokens: %v", want, vals)
}

// tokenSlice strips Line back to 1 so equality assertions don't have
// to thread expected line numbers through.
func tokenSlice(toks []Token) []Token {
	out := make([]Token, len(toks))
	for i, tk := range toks {
		tk.Line = 1
		out[i] = tk
	}
	return out
}
