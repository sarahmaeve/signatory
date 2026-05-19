package node

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestParse_AdversarialInput_TerminatesAndDoesNotPanic is the
// adversarial-robustness regression test, the node analog of the
// python package's equivalent.
//
// signatory ingests untrusted package source by design; a malicious
// publisher controls the .js/.ts bytes the parser sees, up to the
// BlobStreamer's 10 MiB per-file cap. AST.md §4: "Malformed/
// adversarial input must yield a best-effort partial result, never
// abort the file." A parser/lexer that hangs, stack-overflows, or
// panics on a crafted file blinds the analyzer for that version and
// aborts the whole collection — a successful evasion and a DoS.
//
// Each input is small (well under the cap) but targets a specific
// super-linear / unbounded-recursion / panic risk in the
// implementation. The deadline is enormous relative to a correct
// linear pass, so this only fires on a genuine blowup, not timing
// flake.
func TestParse_AdversarialInput_TerminatesAndDoesNotPanic(t *testing.T) {
	t.Parallel()

	const deadline = 5 * time.Second
	const n = 20000

	cases := []struct {
		name string
		src  string
	}{
		{
			// Generic nested calls: exercises the parser's
			// resume-at-'(' path where splitCallArgs rescans the tail
			// once per nested site. maxArgScanTokens must bound it.
			name: "nested generic calls",
			src:  strings.Repeat("f(", n) + "1" + strings.Repeat(")", n) + ";\n",
		},
		{
			// Nested path.join: resolveExpr recurses one frame per
			// level; maxResolveDepth must cap it (conservative miss,
			// not a stack overflow).
			name: "nested path.join",
			src:  "fs.readFileSync(" + strings.Repeat("path.join(", n) + "'/x'" + strings.Repeat(")", n) + ");\n",
		},
		{
			// Deeply nested template literals: scanTemplate recurses
			// through a nested backtick inside ${ }. The lexer must
			// bound this rather than recurse to a stack overflow.
			name: "deeply nested template literals",
			src:  "x = " + strings.Repeat("`${", n) + "1" + strings.Repeat("}`", n) + ";\n",
		},
		{
			// Unterminated nested interpolations: every ${ opens, none
			// closes; scanTemplate must reach EOF leniently.
			name: "unterminated nested interpolation",
			src:  "y = " + strings.Repeat("`${", n) + "\n",
		},
		{
			// Pathological deeply-nested templates large enough to
			// stack-overflow scanTemplate WITHOUT the maxTemplateDepth
			// cap (~300k levels, <1 MiB, well under the 10 MiB
			// BlobStreamer cap a malicious file could reach). With the
			// cap this terminates fast; this case is the regression
			// guard for that bound.
			name: "stack-overflow-scale nested templates",
			src:  "z = " + strings.Repeat("`${", 300000) + "1" + strings.Repeat("}`", 300000) + ";\n",
		},
		{
			// Pathological unbalanced punctuation: brace/paren stacks
			// must never underflow-panic and the walk must terminate.
			name: "unbalanced punctuation soup",
			src:  strings.Repeat(")(}{][=>", n) + "\n",
		},
		{
			// Huge flat statement stream: linear sanity bound.
			name: "huge flat statement stream",
			src:  strings.Repeat("a.b(c);", n) + "\n",
		},
	}

	// Result carries the goroutine's outcome back to the test goroutine.
	// require.* must only be called from the test goroutine itself
	// (testing package contract); ferrying (m, err, panicked) out via
	// a channel keeps the assertions where t.FailNow is allowed and
	// avoids the subtle bug where a future Parse-returns-error change
	// would Goexit the inner goroutine and silently pass the outer
	// select.
	type result struct {
		m        *Module
		err      error
		panicked any
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			done := make(chan result, 1)
			go func() {
				var r result
				defer func() {
					r.panicked = recover()
					done <- r
				}()
				r.m, r.err = Parse([]byte(tc.src))
			}()
			select {
			case r := <-done:
				if r.panicked != nil {
					t.Fatalf("Parse panicked on %d-byte adversarial input (%s): %v — "+
						"violates the AST.md §4 leniency contract",
						len(tc.src), tc.name, r.panicked)
				}
				// Lenient contract: never error, always a partial Module.
				assert.NoError(t, r.err,
					"Parse must be lenient on adversarial input — AST.md §4")
				assert.NotNil(t, r.m,
					"Parse must return a best-effort partial Module — AST.md §4")
			case <-time.After(deadline):
				t.Fatalf("Parse did not terminate within %s on %d-byte "+
					"adversarial input (%s) — super-linear blowup / unbounded "+
					"recursion violates AST.md §4", deadline, len(tc.src), tc.name)
			}
		})
	}
}

// TestAnalyze_AdversarialInput_NoFalsePositive pins the other half of
// the contract: adversarial input is allowed to be a conservative
// MISS, but must never spike a catalog feature it doesn't genuinely
// contain. A benign construct that trips a false anomaly is the one
// unacceptable error (AST.md §4).
func TestAnalyze_AdversarialInput_NoFalsePositive(t *testing.T) {
	t.Parallel()
	const n = 5000
	// Catalog tokens spelled only inside strings/regex/templates plus
	// a pathological nest — none of it is real code.
	src := "const s = '" + strings.Repeat("child_process.execSync(eval(", n) + "';\n" +
		"const re = /" + strings.Repeat("eval\\(", 200) + "/;\n" +
		"const t = `" + strings.Repeat("${ require('child_process') }", 100) + "`;\n"
	c := counts(t, src)
	assert.Equal(t, 0, c.ExecCalls, "catalog tokens inside literals are not calls")
	assert.Equal(t, 0, c.DynamicEvalCalls, "eval inside string/regex/template is not execution")
	assert.Equal(t, 0, c.NetworkCallSites)
	assert.Equal(t, 0, c.Base64DecodeCalls)
	assert.Equal(t, 0, c.SensitivePathReads)
}
