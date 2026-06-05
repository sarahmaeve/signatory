package node

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Fuzz targets for the JS/TS source-evolution parser ---
//
// signatory ingests untrusted package source by design: a malicious
// publisher controls every .js/.ts byte the lexer/parser sees, up to the
// BlobStreamer's 10 MiB per-file cap. AST.md §4 makes the contract
// explicit — "Malformed/adversarial input must yield a best-effort
// partial result, never abort the file."
//
// The hand-enumerated suites in robustness_test.go pin the *known*
// super-linear / unbounded-recursion / panic cases with large crafted
// inputs and a per-input deadline (nested templates that would overflow
// scanTemplate, unterminated interpolations, unbalanced punctuation).
// These fuzz targets are the complement: native Go fuzzing detects
// panics and hangs for free across the input space the hand-written
// cases don't reach, and the bodies assert the structural and leniency
// invariants the lexer/parser guarantee on ANY input. The node analog of
// python/fuzz_test.go — see that file's header for the layer split.
//
// JS is brace-scoped (no significant indentation), so unlike the python
// lexer there are no INDENT/DEDENT tokens to balance; the structural
// lexer invariant is the single trailing EOF plus monotonic lines.

// addNodeSeeds adds the shared seed corpus: benign code, the dominant
// real npm attack shapes (so the fuzzer starts near security-relevant
// token sequences), and small adversarial / malformed fragments that
// exercise the lenient EOF paths, the regex-vs-division disambiguation,
// the template-interpolation scanner, and the empty-specifier edge. The
// large nesting bombs deliberately live in robustness_test.go.
func addNodeSeeds(f *testing.F) {
	seeds := [][]byte{
		// --- benign / structural ---
		[]byte(""),
		[]byte("function add(a, b) { return a + b; }\nmodule.exports = { add };\n"),
		[]byte("import { readFile } from 'fs/promises'\nawait readFile('p')\n"),
		[]byte("let key = 0;\nkey ^= 1;\n"),
		[]byte("const a = b / c / d;\n"), // division, not regex
		[]byte("a?.b?.c();\n"),           // optional chaining
		[]byte("// comment only\n"),
		[]byte("/* block\n comment */\nx;\n"),

		// --- real attack shapes (mirror the analyze_test fixtures) ---
		[]byte("const cp = require('child_process');\ncp.execSync('id');\n"),
		[]byte("eval(Buffer.from('cmd', 'base64').toString());\n"),
		[]byte("new Function('return process.env')();\n"),
		[]byte("fetch('http://169.254.169.254/latest/meta-data/');\n"),
		[]byte("const t = process.env.AWS_SECRET_ACCESS_KEY;\n"),
		[]byte("const { NPM_TOKEN } = process.env;\n"),
		[]byte("const fs = require('fs');\nfs.readFileSync(os.homedir() + '/.ssh/id_rsa');\n"),

		// --- small adversarial / malformed ---
		[]byte("x = /regex/gi;\n"),                     // regex literal after '='
		[]byte("return /re[/]gex/;\n"),                 // regex with '/' in char class
		[]byte("`template ${ `nested ${1}` } end`;\n"), // shallow nested template
		[]byte("`unterminated ${\n"),                   // unterminated interpolation
		[]byte("'\\"),                                  // escape at EOF in string
		[]byte("`\\"),                                  // escape at EOF in template
		[]byte("/unterminated regex\n"),
		[]byte(")(}{][=>"),                           // unbalanced punctuation
		[]byte(">>>= === !== **= ??= ?. ... =>"),     // maximal-munch operator soup
		[]byte("0x1f + 0b101 + 1_000n + 1.5e10;\n"),  // numeric literal forms
		[]byte("require('');\nx();\n"),               // empty specifier → empty-alias call edge
		[]byte("import x from '';\nx.y();\n"),        // empty specifier import
		{'x', '=', '"', 0x00, 0xff, 0x80, '"', '\n'}, // NUL + high bytes inside a string
		{0xef, 0xbb, 0xbf, 'x', '(', ')', '\n'},      // UTF-8 BOM prefix
	}
	for _, s := range seeds {
		f.Add(s)
	}
}

// FuzzLex asserts the tokenizer's structural guarantees on arbitrary
// bytes. The parser layer relies on the single trailing EOF.
func FuzzLex(f *testing.F) {
	addNodeSeeds(f)

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
// Note on what is NOT asserted: Callee/Import/EnvReads may legitimately
// be empty. `require(”)` records an empty import spec, and a bare call
// through an empty-spec alias resolves to an empty Callee — both are
// real, lenient outcomes, not bugs. Only Call.Line (stamped from a
// token's 1-based line) is structurally guaranteed non-trivial.
func FuzzParse(f *testing.F) {
	addNodeSeeds(f)

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

// FuzzAnalyze drives the full file→Counts pipeline (catalog matching,
// alias resolution, resolveFirstArg / path folding, Buffer.from
// second-arg resolution — the surface the recent CloudMetadataCalls /
// SensitivePathWrites work landed in). It reuses the analyze_test
// `counts` helper, which fails the test on any error — a clean
// single-file stream has none — and asserts non-negative, deterministic
// counts.
func FuzzAnalyze(f *testing.F) {
	addNodeSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		c := counts(t, string(data))

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
		c2 := counts(t, string(data))
		assert.True(t, reflect.DeepEqual(c, c2), "Analyze must be deterministic")
	})
}
