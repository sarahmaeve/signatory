package rust

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Fuzz targets for the Rust source-evolution parser ---
//
// signatory ingests untrusted crate source by design: a malicious crate
// publisher controls every .rs byte the lexer/parser sees, up to the
// BlobStreamer's 10 MiB per-file cap. AST.md §4 makes the contract
// explicit — "Malformed/adversarial input must yield a best-effort
// partial result, never abort the file."
//
// The hand-enumerated suites in robustness_test.go pin the *known*
// super-linear / unbounded-recursion / panic cases with large crafted
// inputs and a per-input deadline (the alias-bomb O(n²) guard, nested
// block comments / generics / braces, raw-string hash overflow,
// lifetime/char-literal ambiguity, range-op fences). These fuzz targets
// are the complement: native Go fuzzing detects panics and hangs for
// free across the input space the hand-written cases don't reach, and
// the bodies assert the structural and leniency invariants the
// lexer/parser guarantee on ANY input. The Rust analog of
// python/fuzz_test.go and node/fuzz_test.go.
//
// Rust is brace-scoped (no significant indentation), so — like node —
// there are no INDENT/DEDENT tokens to balance; the structural lexer
// invariant is the single trailing EOF plus monotonic lines.

// addRustSeeds adds the shared seed corpus: benign code, the dominant
// real crate attack shapes (so the fuzzer starts near security-relevant
// token sequences), and small adversarial / malformed fragments that
// exercise the lenient EOF paths and the Rust-specific lexer hot spots —
// nested block comments, raw strings, the char-vs-lifetime apostrophe
// disambiguation, and the number/range-op fence. The large bombs
// deliberately live in robustness_test.go.
func addRustSeeds(f *testing.F) {
	seeds := [][]byte{
		// --- benign / structural ---
		[]byte(""),
		[]byte("fn main() { println!(\"hi\"); }\n"),
		[]byte("use std::collections::HashMap;\nfn f() -> HashMap<u8, u8> { HashMap::new() }\n"),
		[]byte("let mut x = 0u8;\nx ^= 0x37;\n"),
		[]byte("for i in 0..10 { let _ = i; }\n"),        // range-op fence
		[]byte("fn f<'a>(x: &'a str) -> char { 'z' }\n"), // lifetime + char literal
		[]byte("foo::<Vec<u8>>();\n"),                    // turbofish + nested generics

		// --- real attack shapes (mirror the analyze_test fixtures) ---
		[]byte("use std::process::Command;\nfn main() { Command::new(\"sh\").arg(\"-c\").arg(\"id\").output(); }\n"),
		[]byte("use std::fs;\nfn main() { let _ = fs::read(\"/etc/passwd\"); }\n"),
		[]byte("use std::env::var;\nfn main() { let _ = var(\"AWS_SECRET_ACCESS_KEY\"); }\n"),
		[]byte("fn build() { std::fs::write(\"/home/u/.ssh/authorized_keys\", k); }\n"),

		// --- Rust-specific lexer hot spots ---
		[]byte("/* outer /* inner */ still outer */\nfn f() {}\n"), // nested block comment
		[]byte("let s = r#\"raw \" with quote\"#;\n"),              // raw string with embedded quote
		[]byte("let s = r###\"x\"###;\n"),                          // multi-hash raw string
		[]byte("let b = b\"bytes\";\nlet c = b'\\n';\n"),           // byte string + byte char
		[]byte("vec![1, 2, 3];\nprintln!(\"{}\", x);\n"),           // macro invocations
		[]byte("'a 'b 'static 'c 'd"),                              // lifetime soup

		// --- small adversarial / malformed (lenient-EOF paths) ---
		[]byte("\"unterminated string"),
		[]byte("r#\"unterminated raw"),
		[]byte("/* unterminated block comment"),
		[]byte("\"\\"),                                                        // quote + backslash at EOF (escape overrun)
		[]byte(")(}{][=>::->^="),                                              // unbalanced punctuation
		[]byte(">>= <<= ::-> ^= ..= ... <<<"),                                 // maximal-munch operator soup
		[]byte("0x1f + 0b1010 + 1_000u64 + 1.5e10;\n"),                        // numeric literal forms
		[]byte("'a'b'c'd'e "),                                                 // alternating lifetime / char apostrophes
		{'l', 'e', 't', ' ', 's', '=', '"', 0x00, 0xff, 0x80, '"', ';', '\n'}, // NUL + high bytes
		{0xef, 0xbb, 0xbf, 'f', 'n', ' ', 'f', '(', ')', '{', '}'},            // UTF-8 BOM prefix
	}
	for _, s := range seeds {
		f.Add(s)
	}
}

// FuzzLex asserts the tokenizer's structural guarantees on arbitrary
// bytes. The parser layer relies on the single trailing EOF; the
// robustness suite already fuzzes Lex with crafted large inputs, this
// broadens the input space.
func FuzzLex(f *testing.F) {
	addRustSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		toks, err := Lex(data)

		// Leniency (AST.md §4): the lexer favors forward progress over
		// rejecting input — it must never error on adversarial bytes.
		require.NoError(t, err, "Lex must be lenient on adversarial input")

		// Always terminates the stream with exactly one trailing EOF.
		require.NotEmpty(t, toks, "Lex must always emit at least TokenEOF")
		assert.Equal(t, TokenEOF, toks[len(toks)-1].Kind,
			"the final token must be TokenEOF")

		var eofs, prevLine int
		for i, tk := range toks {
			if tk.Kind == TokenEOF {
				eofs++
				assert.Equal(t, len(toks)-1, i, "EOF may only appear as the last token")
			}
			// Line numbers are stamped from a counter that only ever
			// advances, so they are 1-based and non-decreasing.
			assert.GreaterOrEqual(t, tk.Line, 1, "token line must be 1-based")
			assert.GreaterOrEqual(t, tk.Line, prevLine, "token lines must be non-decreasing")
			prevLine = tk.Line
		}
		assert.Equal(t, 1, eofs, "exactly one EOF token")

		// Determinism: a pure tokenizer must return identical output for
		// identical input (guards against a future map-ordered regression).
		toks2, err2 := Lex(data)
		require.NoError(t, err2)
		assert.True(t, reflect.DeepEqual(toks, toks2), "Lex must be deterministic")
	})
}

// FuzzParse asserts the parser's leniency contract and the
// well-formedness of every call it records.
//
// Note on what is NOT asserted: Callee may carry an alias-resolved head,
// and Use paths/aliases come from lenient scans, so only Call.Line
// (stamped from a token's 1-based line) is asserted — matching the
// conservative choice in node/fuzz_test.go.
func FuzzParse(f *testing.F) {
	addRustSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Parse(data)

		// Leniency (AST.md §4): malformed input yields a best-effort
		// partial Module, never an error abort.
		require.NoError(t, err, "Parse must be lenient on adversarial input")
		require.NotNil(t, m, "Parse must always return a non-nil Module")

		for _, c := range m.Calls {
			assert.GreaterOrEqual(t, c.Line, 1, "Call.Line must be 1-based")
		}
		assert.GreaterOrEqual(t, m.XorAssigns, 0)

		// Determinism over the same bytes.
		m2, err2 := Parse(data)
		require.NoError(t, err2)
		assert.True(t, reflect.DeepEqual(m, m2), "Parse must be deterministic")
	})
}

// FuzzAnalyze drives the full file→Counts pipeline (alias resolution via
// assembleCallee, catalog matching, resolveArgN). The path is "build.rs"
// so the ImportTimeCallSites branch (calls in a build script run at
// compile time) is exercised alongside the call catalogs. It reuses the
// analyze_test countsAtPath helper, which fails on any error — a clean
// single-file stream has none — and asserts non-negative, deterministic
// counts.
func FuzzAnalyze(f *testing.F) {
	addRustSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		c := countsAtPath(t, "build.rs", string(data))

		// No counted field may go negative — every accumulate path is an
		// increment, so a negative would mean wraparound or corruption.
		v := reflect.ValueOf(c)
		for i := range v.NumField() {
			if fld := v.Field(i); fld.CanInt() {
				assert.GreaterOrEqualf(t, fld.Int(), int64(0),
					"Counts.%s must be non-negative", v.Type().Field(i).Name)
			}
		}

		// Determinism over the same bytes.
		c2 := countsAtPath(t, "build.rs", string(data))
		assert.True(t, reflect.DeepEqual(c, c2), "Analyze must be deterministic")
	})
}
