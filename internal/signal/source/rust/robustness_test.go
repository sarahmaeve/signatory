package rust

import (
	"fmt"
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
// Parallel parser-level suite lives below in
// TestParse_AdversarialInput_TerminatesAndDoesNotPanic — same shape,
// inputs targeted at parser-level super-linear / unbounded-recursion
// risks (alias resolution, generics, braces, method chains, paths).
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

// TestParse_AdversarialInput_TerminatesAndDoesNotPanic is the
// parser-layer adversarial-robustness suite — the parallel to
// TestLex_AdversarialInput. Same threat model: the parser ingests
// untrusted .rs source up to the BlobStreamer 10 MiB per-file cap,
// and a malicious crate publisher controls every byte. Per AST.md §4
// the parser must yield a best-effort partial Module, never panic /
// hang / O(n²) blow up.
//
// Each case targets a specific parser-level hot path:
//
//   - alias-bomb hits assembleCallee's alias scan against p.mod.Uses
//     (the documented superlinear regression risk — a linear scan
//     per call site is O(N*M) in N uses + M calls).
//   - nested-generics hits skipAngleBlock's depth counter.
//   - nested-braces hits the pushBrace / popBrace stack and the
//     maxBraceDepth cap.
//   - long path hits scanPath's maxPathSegments cap.
//   - long method chain hits tryParseCall's `.name` extension loop.
//   - nested macro_rules hits skipMacroRulesBody + matchBracket.
//   - turbofish soup hits the turbofish-skip branch in scanPath.
//   - unbalanced punctuation feeds the lexer's punctuation soup
//     case through the parser; matchBracket / parseUseGroup must
//     reach EOF leniently.
//   - huge use-group hits parseUseGroup's bounded walk.
//   - huge flat call stream is the linear-sanity bound for tryParseCall.
//
// Deadline is generous relative to a correct linear pass. Each test
// runs in its own goroutine with panic-recovery and a select-on-timeout
// — a hang in Parse becomes a test failure rather than a process hang.
func TestParse_AdversarialInput_TerminatesAndDoesNotPanic(t *testing.T) {
	t.Parallel()

	const deadline = 5 * time.Second

	cases := []struct {
		name string
		src  string
	}{
		{
			// O(N²) alias-scan regression guard. N use-aliases plus N
			// calls whose head matches the LAST alias forces
			// assembleCallee to scan the entire Uses slice on every
			// Call (the early-break only fires at the end). A linear
			// scan is N² ≈ 1e10 same-length byte compares — far over
			// the deadline. An indexed lookup is O(N).
			//
			// Same-length alias names + a same-length call head defeat
			// Go's length-mismatch short-circuit so every compare
			// touches the byte payload.
			name: "alias bomb (per-call linear use scan)",
			src:  buildAliasBomb(100000),
		},
		{
			// Deeply nested generics — `fn f<T<U<...>>>(...) {}`.
			// skipAngleBlock must bound depth and not stack-overflow
			// or wedge the angle-counter on the `>>` two-token
			// emission shape.
			name: "deeply nested generics",
			src:  "fn f" + strings.Repeat("<T", 20000) + strings.Repeat(">", 20000) + "(){}\n",
		},
		{
			// Deeply nested braces — pushBrace / popBrace stack must
			// honour maxBraceDepth without recursion or OOM.
			name: "deeply nested braces",
			src:  "fn main() {" + strings.Repeat("{", 20000) + strings.Repeat("}", 20000) + "}\n",
		},
		{
			// Long `::` path — scanPath / scanUsePath must respect
			// maxPathSegments and not walk all 20k segments per call.
			name: "very long :: path in call",
			src:  "fn main() { " + strings.Repeat("a::", 20000) + "f(); }\n",
		},
		{
			// Long method chain — tryParseCall's `.name` extension
			// loop has no cap analogous to maxPathSegments; even with
			// the alias-scan fix, the Callee string can grow
			// proportional to the chain length. Sanity: must still
			// terminate within the deadline.
			name: "very long method chain",
			src:  "fn main() { a" + strings.Repeat(".b", 20000) + "(); }\n",
		},
		{
			// Many `macro_rules!` definitions — skipMacroRulesBody +
			// matchBracket must each linearly skip their body and not
			// retain quadratic state across macro definitions.
			name: "many macro_rules bodies",
			src:  strings.Repeat("macro_rules! m { () => { ^= } }\n", 5000),
		},
		{
			// Turbofish soup — `foo::<T>()::<U>()...`. scanPath's
			// turbofish-stop branch must terminate per call without
			// re-entering the angle skipper unboundedly.
			name: "turbofish soup",
			src:  "fn main() { " + strings.Repeat("foo::<T>::", 5000) + "f(); }\n",
		},
		{
			// Unbalanced punctuation through the parser. Same input
			// as the lexer suite, now exercising parseUse /
			// tryParseCall / matchBracket lenient EOF handling.
			name: "unbalanced punctuation soup",
			src:  strings.Repeat(")(}{][=>::->^=", 20000) + "\n",
		},
		{
			// Huge `use a::{...};` group — parseUseGroup must reach
			// the matching `}` (or EOF) without quadratic behaviour.
			name: "huge use-group",
			src:  "use a::{" + strings.Repeat("x,", 20000) + "y};\n",
		},
		{
			// Huge flat call stream — linear sanity bound on the
			// main run() loop + tryParseCall.
			name: "huge flat call stream",
			src:  "fn main() {" + strings.Repeat("a();", 20000) + "}\n",
		},
		{
			// Many `^=` tokens (the XorAssigns hot-path). Lexer
			// suite covers tokenization; parser layer must increment
			// in O(1) per occurrence with no per-token Uses scan.
			name: "many xor-assignments",
			src:  "fn main() { let mut a = 0;" + strings.Repeat("a ^= 1;", 20000) + "}\n",
		},
	}

	type result struct {
		mod      *Module
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
				r.mod, r.err = Parse([]byte(tc.src))
			}()
			select {
			case r := <-done:
				if r.panicked != nil {
					t.Fatalf("Parse panicked on %d-byte adversarial input (%s): %v — "+
						"violates the AST.md §4 leniency contract",
						len(tc.src), tc.name, r.panicked)
				}
				// Lenient contract: Parse never errors, always
				// returns a Module (possibly partial).
				assert.NoError(t, r.err,
					"Parse must be lenient on adversarial input — AST.md §4")
				require.NotNil(t, r.mod,
					"Parse must always return a non-nil Module")
			case <-time.After(deadline):
				t.Fatalf("Parse did not terminate within %s on %d-byte "+
					"adversarial input (%s) — super-linear blowup / unbounded "+
					"recursion violates AST.md §4", deadline, len(tc.src), tc.name)
			}
		})
	}
}

// buildAliasBomb constructs a Rust source file with n distinct
// `use foo::aXXXXXXX as aXXXXXXX;` statements and n calls to the LAST
// alias. assembleCallee's alias scan against p.mod.Uses must be O(1)
// per call (indexed map lookup) — a linear scan walks all n entries
// per call (early-break only fires at the end), O(n²) total, tripping
// the suite's deadline.
//
// All alias names AND the call head are the same length (8 chars) so
// Go's length-mismatch short-circuit in string equality can't reduce
// the per-compare cost — every comparison touches the byte payload.
func buildAliasBomb(n int) string {
	var sb strings.Builder
	// Rough size estimate: ~32 B per use + ~12 B per call ≈ 44 B * n.
	sb.Grow(44 * n)
	for i := range n {
		fmt.Fprintf(&sb, "use foo::a%07d as a%07d;\n", i, i)
	}
	last := fmt.Sprintf("a%07d", n-1)
	sb.WriteString("fn main() {\n")
	for range n {
		sb.WriteString(last)
		sb.WriteString("();\n")
	}
	sb.WriteString("}\n")
	return sb.String()
}
