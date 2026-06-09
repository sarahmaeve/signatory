// Package pypiwheel inspects PyPI wheels (bdist) for the narrow class
// of non-build-artifact shapes that signal supply-chain malice — the
// surface the sdist-only artifact-vs-repo check deliberately never
// opens. v0.1 covers the .pth startup-hook vector the 2026-06
// Miasma/Hades campaign abused (see
// design/threat-landscape/2026-06-09-miasma-hades-pypi-wheel.md).
//
// This is a narrow carve-out from the "wheel-vs-repo is a category
// error" rule, NOT a wheel↔source diff: a .pth file is not a build
// output, and a .pth whose body executes code beyond legitimate path /
// namespace machinery is signal-bearing on its own.
package pypiwheel

import (
	"strings"
)

// PthFinding describes one suspicious executable line in a .pth file.
// Line is 1-indexed; Reasons names the dangerous primitives matched;
// Sample is a length-bounded excerpt for the analyst.
type PthFinding struct {
	Line    int      `json:"line"`
	Reasons []string `json:"reasons"`
	Sample  string   `json:"sample"`
}

// pthSampleCap bounds the excerpt carried per finding so an
// attacker-padded .pth line cannot bloat the signal payload.
const pthSampleCap = 200

// pthDangerToken pairs a case-sensitive substring with the reason label
// it contributes. The set is deliberately tight: every entry is a
// primitive that legitimate .pth content — bare path additions,
// setuptools *-nspkg.pth namespace shims, editable-install finder
// shims — never uses. __import__( is intentionally ABSENT: the
// setuptools nspkg template calls __import__('importlib.util') as
// normal namespace machinery, so flagging it would false-positive on
// the single most common code-bearing .pth in the wild.
var pthDangerTokens = []struct {
	token  string
	reason string
}{
	// Dynamic code execution.
	{"exec(", "exec"},
	{"eval(", "eval"},
	{"compile(", "compile"},
	// Process spawning (the campaign shells out to a JS runtime).
	{"subprocess", "subprocess"},
	{"os.system", "os.system"},
	{"os.popen", "os.popen"},
	{".Popen(", "subprocess"},
	{"pty.spawn", "subprocess"},
	// Network egress / staged download.
	{"urllib", "network"},
	{"urlopen", "network"},
	{"socket.", "network"},
	{"http.client", "network"},
	{"requests.", "network"},
	// Decode of an embedded blob.
	{"base64", "base64"},
	{"b64decode", "base64"},
	{".fromhex(", "decode"},
	{"codecs.decode", "decode"},
	// Foreign-runtime bootstrap: a .pth that runs a JS/WASM/shell
	// payload. These are the Bun/_index.js fingerprint.
	{".js", "foreign_runtime"},
	{".mjs", "foreign_runtime"},
	{".cjs", "foreign_runtime"},
	{".wasm", "foreign_runtime"},
	{"bun", "foreign_runtime"},
	{"deno", "foreign_runtime"},
}

// ScanPth examines one .pth file's bytes and returns a finding for each
// executable line carrying a dangerous primitive. An empty result means
// the .pth is benign: path-only, or a known-safe code shim whose lines
// match no danger token.
//
// Only lines Python's site module actually executes are scanned — those
// beginning with "import" (optionally after leading whitespace). Bare
// path lines, comments, and blank lines are never executed by site, so
// they are skipped entirely (a comment naming "subprocess" does not
// flag).
func ScanPth(content []byte) []PthFinding {
	var findings []PthFinding
	lines := strings.Split(string(content), "\n")
	for i, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// site.py executes a .pth line only when it begins with
		// "import " or "import\t". Everything else is a path entry.
		if !isImportExecLine(line) {
			continue
		}

		var reasons []string
		seen := make(map[string]struct{})
		for _, dt := range pthDangerTokens {
			if strings.Contains(line, dt.token) {
				if _, dup := seen[dt.reason]; !dup {
					seen[dt.reason] = struct{}{}
					reasons = append(reasons, dt.reason)
				}
			}
		}
		if len(reasons) == 0 {
			continue
		}
		findings = append(findings, PthFinding{
			Line:    i + 1,
			Reasons: reasons,
			Sample:  truncate(line, pthSampleCap),
		})
	}
	return findings
}

// isImportExecLine reports whether site.py would execute this line: it
// must start with the keyword "import" followed by whitespace. "importlib"
// as a leading token does not count (no .pth begins that way, and the
// keyword boundary keeps the check honest).
func isImportExecLine(line string) bool {
	const kw = "import"
	if !strings.HasPrefix(line, kw) {
		return false
	}
	rest := line[len(kw):]
	return strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, "\t")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
