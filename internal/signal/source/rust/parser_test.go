package rust

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parse is the test-helper analog of tokenize — wraps Parse and
// asserts the lenient-no-error contract so per-test fixtures don't
// repeat it.
func parse(t *testing.T, src string) *Module {
	t.Helper()
	m, err := Parse([]byte(src))
	require.NoError(t, err, "Parse must be lenient — AST.md §4")
	require.NotNil(t, m, "Parse must always return a Module")
	return m
}

// ============================================================
// Use statements
// ============================================================

func TestParse_Use_SinglePath(t *testing.T) {
	t.Parallel()
	m := parse(t, `use std::env::var;`)
	require.Len(t, m.Uses, 1)
	assert.Equal(t, []string{"std", "env", "var"}, m.Uses[0].Path)
	assert.Equal(t, "var", m.Uses[0].Alias,
		"the default alias is the last path segment")
}

func TestParse_Use_AsClause(t *testing.T) {
	t.Parallel()
	m := parse(t, `use std::process::Command as Cmd;`)
	require.Len(t, m.Uses, 1)
	assert.Equal(t, []string{"std", "process", "Command"}, m.Uses[0].Path)
	assert.Equal(t, "Cmd", m.Uses[0].Alias,
		"the `as` clause overrides the default last-segment alias")
}

func TestParse_Use_TwoLevel(t *testing.T) {
	t.Parallel()
	m := parse(t, `use std::env;`)
	require.Len(t, m.Uses, 1)
	assert.Equal(t, []string{"std", "env"}, m.Uses[0].Path)
	assert.Equal(t, "env", m.Uses[0].Alias)
}

func TestParse_Use_Grouped(t *testing.T) {
	t.Parallel()
	m := parse(t, `use std::env::{var, set_var, vars};`)
	require.Len(t, m.Uses, 3,
		"each entry in a grouped import must become its own Use")
	wantPaths := [][]string{
		{"std", "env", "var"},
		{"std", "env", "set_var"},
		{"std", "env", "vars"},
	}
	for i, want := range wantPaths {
		assert.Equal(t, want, m.Uses[i].Path)
	}
	assert.Equal(t, []string{"var", "set_var", "vars"}, []string{
		m.Uses[0].Alias, m.Uses[1].Alias, m.Uses[2].Alias,
	})
}

func TestParse_Use_GroupedWithAsClause(t *testing.T) {
	t.Parallel()
	m := parse(t, `use std::process::{Command, exit as quit};`)
	require.Len(t, m.Uses, 2)
	assert.Equal(t, "Command", m.Uses[0].Alias)
	assert.Equal(t, "quit", m.Uses[1].Alias,
		"the `as` clause works inside a grouped import")
}

// ============================================================
// Call extraction
// ============================================================

func TestParse_Call_BareIdentifier(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn main() { println("hi"); }`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "println", m.Calls[0].Callee)
	assert.Equal(t, "main", m.Calls[0].InFn)
	assert.Equal(t, "hi", m.Calls[0].FirstArg)
}

func TestParse_Call_PathSeparators(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn main() { std::env::var("AWS_SECRET_ACCESS_KEY"); }`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "std::env::var", m.Calls[0].Callee,
		"path callees are preserved with :: separators")
	assert.Equal(t, "AWS_SECRET_ACCESS_KEY", m.Calls[0].FirstArg)
	assert.Equal(t, "main", m.Calls[0].InFn)
}

// TestParse_Call_AliasResolution_AsClause is the load-bearing
// resolver test. `use std::env::var as v;` followed by `v("X")` must
// resolve the leading segment back to `std::env::var`, so the
// analyzer's catalog matcher sees the full path.
func TestParse_Call_AliasResolution_AsClause(t *testing.T) {
	t.Parallel()
	m := parse(t, `
use std::env::var as v;
fn main() { v("AWS_SECRET_ACCESS_KEY"); }
`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "std::env::var", m.Calls[0].Callee,
		"the as-aliased v call must resolve back to std::env::var")
	assert.Equal(t, "AWS_SECRET_ACCESS_KEY", m.Calls[0].FirstArg)
}

// TestParse_Call_AliasResolution_DefaultAlias covers
// `use std::env::var;` then `var("X")` — the default alias is
// the last segment, so the resolution still applies.
func TestParse_Call_AliasResolution_DefaultAlias(t *testing.T) {
	t.Parallel()
	m := parse(t, `
use std::env::var;
fn main() { var("GITHUB_TOKEN"); }
`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "std::env::var", m.Calls[0].Callee,
		"the bare `var` call after `use std::env::var;` must "+
			"resolve to the full std::env::var path")
}

// TestParse_Call_AliasResolution_PartialPath is the harder case:
// `use std::fs;` then `fs::read_to_string("path")` — the leading
// "fs" segment resolves to `std::fs`, the rest is appended.
func TestParse_Call_AliasResolution_PartialPath(t *testing.T) {
	t.Parallel()
	m := parse(t, `
use std::fs;
fn main() { fs::read_to_string("/home/user/.ssh/id_rsa"); }
`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "std::fs::read_to_string", m.Calls[0].Callee,
		"leading `fs` segment must resolve through `use std::fs;`")
	assert.Equal(t, "/home/user/.ssh/id_rsa", m.Calls[0].FirstArg)
}

func TestParse_Call_TwoArgs(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn main() { std::fs::write("/tmp/x", "data"); }`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "/tmp/x", m.Calls[0].FirstArg)
	assert.Equal(t, "data", m.Calls[0].SecondArg)
}

func TestParse_Call_MethodChainOnNameReceiver(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn main() { cmd.arg("-c"); }`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "cmd.arg", m.Calls[0].Callee,
		"name.method() must record the dotted form so the analyzer "+
			"can match it (or treat as a documented gap)")
	assert.Equal(t, "-c", m.Calls[0].FirstArg)
}

func TestParse_Call_MacroBracketStyles(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		`fn main() { println!("x"); }`,
		`fn main() { vec![1, 2, 3]; }`,
		`fn main() { thread_local! { static X: i32 = 0; } }`,
	} {
		m := parse(t, src)
		require.GreaterOrEqual(t, len(m.Calls), 1, "src=%q", src)
		c := m.Calls[0]
		assert.True(t, c.Macro,
			"macro invocations (%s) must have Macro=true (src=%q)", c.Callee, src)
	}
}

func TestParse_Call_MacroParenStyleResolvesFirstArg(t *testing.T) {
	t.Parallel()
	// println! with paren-style args — first arg is the format string.
	m := parse(t, `fn main() { println!("hello, {}", name); }`)
	require.Len(t, m.Calls, 1)
	assert.True(t, m.Calls[0].Macro)
	assert.Equal(t, "hello, {}", m.Calls[0].FirstArg)
}

// TestParse_Call_RawStringArg confirms a raw-string literal flows
// through as the first arg's unquoted body. Trapdoor-style obfuscated
// names in raw strings still resolve for catalog matching.
func TestParse_Call_RawStringArg(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn main() { std::env::var(r#"AWS_SECRET_ACCESS_KEY"#); }`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "AWS_SECRET_ACCESS_KEY", m.Calls[0].FirstArg,
		"raw-string first arg must unquote to the byte payload")
}

func TestParse_Call_ConcatStringArgs(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn main() { std::env::var("AWS_" + "SECRET"); }`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "AWS_SECRET", m.Calls[0].FirstArg,
		"`+`-concatenation of string literals must resolve")
}

func TestParse_Call_NonLiteralArg_Unresolved(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn main() { std::env::var(name_var); }`)
	require.Len(t, m.Calls, 1)
	assert.Empty(t, m.Calls[0].FirstArg,
		"a name reference can't be statically resolved — "+
			"documented conservative gap (AST.md §4)")
}

// ============================================================
// Scope discrimination
// ============================================================

func TestParse_Call_TopLevel_InFnEmpty(t *testing.T) {
	t.Parallel()
	// Rust really doesn't permit calls at file top-level (only inside
	// fn bodies / static initializers / const expressions). But the
	// parser is lenient — if it encounters one in adversarial input
	// it records InFn="".
	m := parse(t, `let x = foo("y");`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "", m.Calls[0].InFn,
		"a call outside any fn must record InFn=\"\"")
}

func TestParse_Call_InsideMain(t *testing.T) {
	t.Parallel()
	m := parse(t, `
fn helper() {}
fn main() {
    foo("a");
    helper();
}
`)
	require.Len(t, m.Calls, 2)
	assert.Equal(t, "main", m.Calls[0].InFn)
	assert.Equal(t, "main", m.Calls[1].InFn)
}

func TestParse_Call_NestedFns_TrackInnermost(t *testing.T) {
	t.Parallel()
	m := parse(t, `
fn outer() {
    fn inner() {
        leaf();
    }
    inner();
}
`)
	require.Len(t, m.Calls, 2)
	leaf := m.Calls[0]
	innerCall := m.Calls[1]
	assert.Equal(t, "leaf", leaf.Callee)
	assert.Equal(t, "inner", leaf.InFn,
		"a call inside a nested fn must record the innermost enclosing fn")
	assert.Equal(t, "inner", innerCall.Callee)
	assert.Equal(t, "outer", innerCall.InFn,
		"after the nested fn closes, calls again record the outer fn")
}

// TestParse_Call_GenericFnHeader confirms `fn f<T: Trait>(...)` with
// nested-generic trailing `>>` doesn't confuse the fn-header scanner.
// The lexer emits `>>` as two `>` tokens (AST.md §4), so the header
// scanner's angle-bracket counter must close cleanly.
func TestParse_Call_GenericFnHeader(t *testing.T) {
	t.Parallel()
	m := parse(t, `
fn f<T: Iterator<Item = u32>>() {
    inner();
}
`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "inner", m.Calls[0].Callee)
	assert.Equal(t, "f", m.Calls[0].InFn,
		"nested generics with >> close must not break fn-frame tracking")
}

// TestParse_Call_FnWithReturnType confirms `fn f() -> T { ... }` parses.
func TestParse_Call_FnWithReturnType(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn f() -> String { call_me() }`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "f", m.Calls[0].InFn)
}

// TestParse_Call_TraitForwardDecl_NoFrame confirms a trait-method
// signature `fn foo(&self);` with no body doesn't push a fn frame
// (the next `{` from the enclosing trait body would otherwise be
// mis-claimed).
func TestParse_Call_TraitForwardDecl_NoFrame(t *testing.T) {
	t.Parallel()
	m := parse(t, `
trait T {
    fn forward(&self);
}
fn main() { actual(); }
`)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "actual", m.Calls[0].Callee)
	assert.Equal(t, "main", m.Calls[0].InFn,
		"the trait forward-decl must not push a fn frame — "+
			"otherwise actual() would record InFn=\"forward\"")
}

// ============================================================
// XOR assigns
// ============================================================

func TestParse_XorAssign_Counted(t *testing.T) {
	t.Parallel()
	m := parse(t, `
fn main() {
    let mut data = vec![1, 2, 3];
    for i in 0..data.len() {
        data[i] ^= 0x55;
    }
}
`)
	assert.Equal(t, 1, m.XorAssigns,
		"each ^= must increment XorAssigns once")
}

func TestParse_XorAssign_MultipleSites(t *testing.T) {
	t.Parallel()
	m := parse(t, `
fn main() {
    a ^= 1;
    b ^= 2;
    c ^= 3;
}
`)
	assert.Equal(t, 3, m.XorAssigns)
}

// TestParse_XorAssign_InString_NotCounted is the security property:
// `^=` inside a string literal must NOT count. AST.md §4 opaque-token
// discipline — same property the lexer test pins for catalog names.
func TestParse_XorAssign_InString_NotCounted(t *testing.T) {
	t.Parallel()
	m := parse(t, `fn main() { let s = "a ^= b"; }`)
	assert.Equal(t, 0, m.XorAssigns,
		"^= inside a string is a literal byte sequence, not an assignment")
}

// TestParse_XorAssign_InMacroRulesBody_NotCounted is the false-
// positive guard surfaced by the anyhow dogfood: anyhow's src/ensure.rs
// contains a `macro_rules!` arm whose pattern includes the literal
// `^=` token tree:
//
//	($bitxoreq:tt $($dup:tt)*) ^= $($rest:tt)*
//
// This is a token-tree pattern, NOT a real assignment expression —
// it lexes as `^=` but it lives inside a macro_rules! body that
// describes syntax to match, not code to execute. AST.md §4 opaque-
// token discipline: parser must skip macro_rules! bodies entirely so
// pattern syntax inside them never inflates XorAssigns.
//
// The fix is parallel to node's "code inside template-literal ${} is
// not tokenized" gap — a body whose contents are pattern syntax must
// not feed the call/assignment trackers.
func TestParse_XorAssign_InMacroRulesBody_NotCounted(t *testing.T) {
	t.Parallel()
	const src = `
macro_rules! ensure_bitxoreq {
    (atom ($($stack:tt)+) $bail:tt (~$($fuel:tt)*) {($($buf:tt)*) $($parse:tt)*} ($bitxoreq:tt $($dup:tt)*) ^= $($rest:tt)*) => {
        $crate::__private::stringify!()
    };
}
fn helper() {
    // Real assignment outside the macro — should still count.
    let mut a = 0;
    a ^= 1;
}
`
	m := parse(t, src)
	assert.Equal(t, 1, m.XorAssigns,
		"only the real `a ^= 1` outside the macro_rules! body counts; "+
			"the pattern `^=` inside macro_rules! is matcher syntax, not an assignment")
}

// TestParse_MacroRules_CallsInsideSkipped confirms call sites inside
// macro_rules! bodies aren't recorded as Calls either. The body is a
// pattern/template description — `$crate::process::Command::new` in
// a macro_rules! arm is syntax substitution, not an executable call.
func TestParse_MacroRules_CallsInsideSkipped(t *testing.T) {
	t.Parallel()
	const src = `
macro_rules! make_cmd {
    ($name:expr) => {
        std::process::Command::new($name)
    };
}
fn helper() {
    let real = std::env::var("AWS_SECRET_ACCESS_KEY");
}
`
	m := parse(t, src)
	require.Len(t, m.Calls, 1,
		"only the real std::env::var call should be recorded; "+
			"Command::new inside macro_rules! is syntax substitution")
	assert.Equal(t, "std::env::var", m.Calls[0].Callee)
}

// TestParse_MacroRulesEmptyBody confirms an empty macro_rules! body
// doesn't trip the parser's brace-skip logic.
func TestParse_MacroRulesEmptyBody(t *testing.T) {
	t.Parallel()
	const src = `
macro_rules! nothing {}
fn main() { foo(); }
`
	m := parse(t, src)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "foo", m.Calls[0].Callee)
	assert.Equal(t, "main", m.Calls[0].InFn,
		"the empty macro_rules! body must not leave the brace stack "+
			"in a state that mislabels subsequent fn frames")
}

// ============================================================
// Adversarial / leniency
// ============================================================

func TestParse_LenientOnGarbage(t *testing.T) {
	t.Parallel()
	for _, src := range []string{
		`fn`,
		`fn (`,
		`use ::`,
		`use std::{{`,
		`}`,
		`{{{{`,
		`)))))`,
		`fn f<<>>(x) { y(); }`, // double `<` is `<<` lexed as op
	} {
		_, err := Parse([]byte(src))
		assert.NoError(t, err, "Parse must never error on garbage (src=%q)", src)
	}
}
