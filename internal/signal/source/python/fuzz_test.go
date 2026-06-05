package python

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// --- Fuzz targets for the Python source-evolution parser ---
//
// signatory ingests untrusted package source by design: a malicious
// publisher controls every .py byte the lexer/parser sees, up to the
// BlobStreamer's 10 MiB per-file cap. AST.md §4 makes the contract
// explicit — "Malformed/adversarial input must yield a best-effort
// partial result, never abort the file."
//
// The hand-enumerated TestParse_AdversarialNestedInput suite in
// robustness_test.go pins the *known* super-linear / unbounded-recursion
// cases with large crafted inputs and a per-input deadline. These fuzz
// targets are the complement: native Go fuzzing detects panics and hangs
// for free across the input space the hand-written cases don't reach,
// and the bodies below assert the structural and leniency invariants the
// lexer/parser guarantee on ANY input. Algorithmic-complexity regressions
// (an O(n²) that still completes) stay the robustness suite's job — the
// fuzzer's generated inputs are small, where O(n²) is fast.
//
// Division of labor by layer:
//   - FuzzLex     — tokenizer structural invariants (EOF terminator,
//                   balanced INDENT/DEDENT, monotonic lines) + determinism.
//   - FuzzParse   — parser leniency (never error / always a Module) +
//                   Module well-formedness + determinism.
//   - FuzzAnalyze — the full file→Counts pipeline (catalog matching,
//                   resolveArgN / os.path.join folding — the surface the
//                   recent CloudMetadataCalls / SensitivePathWrites work
//                   landed in): no panic, no spurious error, non-negative
//                   counts, determinism.

// addPythonSeeds adds the shared seed corpus. Seeds span benign code,
// the dominant real PyPI attack shapes (so the fuzzer starts near
// security-relevant token sequences), and small adversarial / malformed
// fragments that exercise the lenient EOF and bounds paths. The large
// nesting bombs deliberately live in robustness_test.go, not here — a
// 400 KiB seed would only slow the corpus without adding reach the
// fuzzer can't discover by mutating the small ones.
func addPythonSeeds(f *testing.F) {
	seeds := [][]byte{
		// --- benign / structural ---
		[]byte(""),
		[]byte("x = 1\n"),
		[]byte("# just a comment, no tokens\n"),
		[]byte("import os.path as p\n"),
		[]byte("from base64 import b64decode, b32decode\n"),
		[]byte("from . import x\n"),
		[]byte("key = 0\nkey ^= 1\n"),
		[]byte("class A(B.C, metaclass=M):\n    pass\n"),
		[]byte("def f():\n\tx = 1\n        y = 2\n"), // mixed tab/space indent

		// --- real attack shapes (mirror the analyze_test fixtures) ---
		[]byte("import base64\nexec(base64.b64decode('aW1wb3J0IG9z'))\n"),
		[]byte("import urllib.request\n" +
			"urllib.request.urlopen('http://169.254.169.254/latest/meta-data/')\n"),
		[]byte("from setuptools.command.install import install\n" +
			"class P(install):\n    def run(self):\n        pass\n"),
		[]byte("import os\nopen(os.path.expanduser('~/.ssh/id_rsa'))\n"),
		[]byte("import os\n" +
			"open(os.path.join(os.getenv('LOCALAPPDATA'), 'Roblox', 'x.dat'))\n"),

		// --- small adversarial / malformed (lenient-EOF + bounds paths) ---
		[]byte("f(f(f(f(1))))\n"),                    // shallow nested generic call
		[]byte("open(str(str(str('/tmp/x'))))\n"),    // shallow nested path-builder
		[]byte("'''unterminated triple"),             // triple string to EOF
		[]byte("'unterminated single"),               // single string to EOF
		[]byte("'\\"),                                // quote + backslash at EOF (escape overrun)
		[]byte("x = \\"),                             // line-continuation backslash at EOF
		[]byte(")((()][}{"),                          // unbalanced punctuation
		[]byte("**= //= >>= <<= ... := -> ^="),       // maximal-munch operator soup
		[]byte("r\"\"\"raw triple\"\"\"\n"),          // prefixed triple string
		{'x', '=', '"', 0x00, 0xff, 0x80, '"', '\n'}, // NUL + high bytes inside a string
		{0xef, 0xbb, 0xbf, 'x', '=', '1', '\n'},      // UTF-8 BOM prefix
	}
	for _, s := range seeds {
		f.Add(s)
	}
}

// FuzzLex asserts the tokenizer's structural guarantees on arbitrary
// bytes. A violation of any of these is a real bug: the parser layer
// relies on the EOF terminator and on balanced indentation tokens.
func FuzzLex(f *testing.F) {
	addPythonSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		toks, err := Lex(data)

		// Leniency (AST.md §4): the lexer favors forward progress over
		// rejecting input — it must never error on adversarial bytes.
		require.NoError(t, err, "Lex must be lenient on adversarial input")

		// Always terminates the stream with exactly one trailing EOF.
		require.NotEmpty(t, toks, "Lex must always emit at least TokenEOF")
		assert.Equal(t, TokenEOF, toks[len(toks)-1].Kind,
			"the final token must be TokenEOF")

		var indents, dedents, eofs int
		prevLine := 0
		for i, tk := range toks {
			switch tk.Kind {
			case TokenIndent:
				indents++
			case TokenDedent:
				dedents++
			case TokenEOF:
				eofs++
				assert.Equal(t, len(toks)-1, i, "EOF may only appear as the last token")
			}
			// Line numbers are stamped from a counter that only ever
			// advances, so they are 1-based and non-decreasing.
			assert.GreaterOrEqual(t, tk.Line, 1, "token line must be 1-based")
			assert.GreaterOrEqual(t, tk.Line, prevLine, "token lines must be non-decreasing")
			prevLine = tk.Line
		}

		// The indent stack opens at [0] and is forced back to [0] at EOF,
		// so every INDENT is matched by a DEDENT.
		assert.Equal(t, indents, dedents,
			"INDENT/DEDENT must balance (the indent stack always unwinds to base)")
		assert.Equal(t, 1, eofs, "exactly one EOF token")

		// Determinism: a pure tokenizer must return identical output for
		// identical input (guards against a future map-ordered regression).
		toks2, err2 := Lex(data)
		require.NoError(t, err2)
		assert.True(t, reflect.DeepEqual(toks, toks2), "Lex must be deterministic")
	})
}

// FuzzParse asserts the parser's leniency contract and the
// well-formedness of every construct it records.
func FuzzParse(f *testing.F) {
	addPythonSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := Parse(data)

		// Leniency (AST.md §4): malformed input yields a best-effort
		// partial Module, never an error abort.
		require.NoError(t, err, "Parse must be lenient on adversarial input")
		require.NotNil(t, m, "Parse must always return a non-nil Module")

		// Every recorded call carries a non-empty dotted callee at a
		// real line — a call is only appended when scanDotted succeeded
		// (NAME present) and a '(' follows.
		for _, c := range m.Calls {
			assert.NotEmpty(t, c.Callee, "a recorded Call must have a callee")
			assert.GreaterOrEqual(t, c.Line, 1, "Call.Line must be 1-based")
		}
		for _, cd := range m.Classes {
			assert.NotEmpty(t, cd.Name, "a recorded ClassDef must have a name")
			assert.GreaterOrEqual(t, cd.Line, 1, "ClassDef.Line must be 1-based")
		}
		for _, imp := range m.Imports {
			assert.NotEmpty(t, imp, "a recorded Import must be non-empty")
		}
		assert.GreaterOrEqual(t, m.XorAssigns, 0)

		// Determinism over the same bytes.
		m2, err2 := Parse(data)
		require.NoError(t, err2)
		assert.True(t, reflect.DeepEqual(m, m2), "Parse must be deterministic")
	})
}

// FuzzAnalyze drives the full file→Counts pipeline. The Path is
// "setup.py" so the install-hook accumulate branch (class-base catalog
// matching) is exercised alongside the call catalogs and the
// resolveArgN / os.path.join path folding. The analyzer must never
// panic, must not surface a spurious error on a clean single-file
// stream, and must produce non-negative, deterministic counts.
func FuzzAnalyze(f *testing.F) {
	addPythonSeeds(f)

	f.Fuzz(func(t *testing.T, data []byte) {
		counts := analyzeFuzzBytes(t, data)

		// No counted field may go negative — every accumulate path is an
		// increment, so a negative here would mean integer wraparound or
		// a corrupted struct.
		v := reflect.ValueOf(counts)
		for i := range v.NumField() {
			if fld := v.Field(i); fld.CanInt() {
				assert.GreaterOrEqualf(t, fld.Int(), int64(0),
					"Counts.%s must be non-negative", v.Type().Field(i).Name)
			}
		}

		// Determinism over the same bytes.
		counts2 := analyzeFuzzBytes(t, data)
		assert.True(t, reflect.DeepEqual(counts, counts2), "Analyze must be deterministic")
	})
}

// analyzeFuzzBytes runs the Analyzer over data as a single setup.py file
// and fails the test on any error — a clean in-memory stream has no
// upstream provider error, so a non-nil error would be a real defect.
func analyzeFuzzBytes(t *testing.T, data []byte) astfeature.Counts {
	t.Helper()
	c, err := NewAnalyzer().Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "setup.py", Content: data}},
	))
	require.NoError(t, err, "Analyze must not error on a clean single-file stream")
	return c
}
