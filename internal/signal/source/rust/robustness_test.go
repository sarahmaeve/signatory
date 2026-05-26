package rust

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLex_AdversarialInput_TerminatesAndDoesNotPanic is the
// adversarial-robustness regression test, the Rust analog of
// node/robustness_test.go.
//
// signatory ingests untrusted package source by design; a malicious
// crate publisher controls the .rs bytes the lexer sees, up to the
// BlobStreamer's 10 MiB per-file cap. AST.md §4: "Malformed/
// adversarial input must yield a best-effort partial result, never
// abort the file." A lexer that hangs, stack-overflows, or panics on
// a crafted file blinds the analyzer for that version and aborts
// the whole collection — a successful evasion and a DoS.
//
// Each input is small (well under the cap) but targets a specific
// super-linear / unbounded-recursion / panic risk in the
// implementation. The deadline is large relative to a correct linear
// pass, so this only fires on a genuine blowup, not timing flake.
//
// Lives at the Lex level for now; once Parse is written it will get
// a parallel TestParse_AdversarialInput suite — same shape, same
// inputs, separate process-level isolation.
func TestLex_AdversarialInput_TerminatesAndDoesNotPanic(t *testing.T) {
	t.Parallel()

	const deadline = 5 * time.Second
	const n = 20000

	cases := []struct {
		name string
		src  string
	}{
		{
			// Deeply nested block comments: Rust permits nesting
			// (unlike C), and scanBlockComment recurses one Go frame
			// per level. maxBlockCommentDepth must bound it — the
			// regression guard for that cap.
			name: "deeply nested block comments",
			src:  strings.Repeat("/*", n) + "x" + strings.Repeat("*/", n) + "\n",
		},
		{
			// Pathological deeply-nested block comments large enough
			// to stack-overflow scanBlockComment WITHOUT
			// maxBlockCommentDepth (~300k levels, <1 MiB, well under
			// the 10 MiB BlobStreamer cap a malicious file could
			// reach). With the cap this terminates fast.
			name: "stack-overflow-scale nested block comments",
			src:  strings.Repeat("/*", 300000) + "x" + strings.Repeat("*/", 300000) + "\n",
		},
		{
			// Unterminated nested block comments: every /* opens,
			// none closes. scanBlockComment must reach EOF leniently
			// and not infinite-loop on the unclosed depth stack.
			name: "unterminated nested block comments",
			src:  strings.Repeat("/*", n) + "\n",
		},
		{
			// Raw string with extreme hash count: must hit the
			// maxRawStringHashes cap and fall back to identifier
			// scanning rather than scan the whole input as one literal
			// or hang counting hashes.
			name: "raw string hash overflow",
			src:  "let s = r" + strings.Repeat("#", n) + "\"x\"" + strings.Repeat("#", n) + ";\n",
		},
		{
			// Many small raw strings: linear sanity bound on the
			// per-literal scan path.
			name: "many small raw strings",
			src:  strings.Repeat(`r#"x"#;`, n) + "\n",
		},
		{
			// Unterminated raw string at EOF: lenient consume-to-EOF
			// rather than infinite loop on the missing close.
			name: "unterminated raw string",
			src:  "let s = r##\"unterminated\n" + strings.Repeat("filler\n", n),
		},
		{
			// Unbalanced punctuation: brace/paren stacks (here, just
			// the operator scanner) must never underflow-panic and the
			// scan must terminate.
			name: "unbalanced punctuation soup",
			src:  strings.Repeat(")(}{][=>::->^=", n) + "\n",
		},
		{
			// Huge flat statement stream: linear sanity bound on the
			// main loop.
			name: "huge flat statement stream",
			src:  strings.Repeat("a.b(c);", n) + "\n",
		},
		{
			// Lifetime/char-literal ambiguity stress: many apostrophes
			// in a row, alternating between lifetime-shaped and char-
			// shaped. The disambiguator must not recurse or loop.
			name: "alternating lifetime / char-literal apostrophes",
			src:  strings.Repeat("'a'b'c'd'e ", n) + "\n",
		},
		{
			// Number / range-op fence stress: many `0..1` ranges in a
			// row. scanNumber's `.` look-ahead must keep the number
			// from eating the `.` of the next range.
			name: "many ranges",
			src:  strings.Repeat("0..1, ", n) + "\n",
		},
	}

	type result struct {
		toks     []Token
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
				r.toks, r.err = Lex([]byte(tc.src))
			}()
			select {
			case r := <-done:
				if r.panicked != nil {
					t.Fatalf("Lex panicked on %d-byte adversarial input (%s): %v — "+
						"violates the AST.md §4 leniency contract",
						len(tc.src), tc.name, r.panicked)
				}
				// Lenient contract: never error, always return tokens.
				assert.NoError(t, r.err,
					"Lex must be lenient on adversarial input — AST.md §4")
				require.NotEmpty(t, r.toks,
					"Lex must always produce at least an EOF token")
				assert.Equal(t, TokenEOF, r.toks[len(r.toks)-1].Kind,
					"Lex must terminate with TokenEOF")
			case <-time.After(deadline):
				t.Fatalf("Lex did not terminate within %s on %d-byte "+
					"adversarial input (%s) — super-linear blowup / unbounded "+
					"recursion violates AST.md §4", deadline, len(tc.src), tc.name)
			}
		})
	}
}
