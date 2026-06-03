package buildscript

import (
	"bufio"
	"bytes"
	"strings"
)

// Severity grades a finding. Only strong findings are meant to move a
// gate; informational findings are context for an analyst.
type Severity string

const (
	SeverityInformational Severity = "informational"
	SeverityStrong        Severity = "strong"
)

// Kind names the behaviour class a finding belongs to.
type Kind string

const (
	KindDecode       Kind = "decode"               // base64/hex/xor/decompress
	KindEvalExec     Kind = "eval_exec"            // eval/exec/shell-out
	KindNetworkFetch Kind = "network_fetch"        // curl/wget/urllib in a build
	KindHighEntropy  Kind = "high_entropy_literal" // long base64-charset run
)

// Finding is one suspicious occurrence in a build script.
type Finding struct {
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Kind     Kind     `json:"kind"`
	Severity Severity `json:"severity"`
	Snippet  string   `json:"snippet"`
}

// maxSnippet bounds the snippet length echoed into a finding so a
// minified or padded line can't bloat the signal payload.
const maxSnippet = 160

// Token catalogs — case-insensitive substring match per line. Tuned so
// that lone hits are merely informational (these tokens appear in benign
// build scripts too); the strong signal is co-occurrence, computed
// below. Deliberately NOT including bare backtick: m4 uses it as its
// open-quote, so it would fire on every m4 file.
var (
	decodeTokens = []string{
		"base64", "b64decode", "atob(", "frombase64", "decodebytes",
		"unhexlify", "fromhex", "a2b_hex", "a2b_base64",
		"zlib.decompress", "gzip.decompress", "lzma.decompress", "codecs.decode",
	}
	evalExecTokens = []string{
		"eval(", " eval ", "eval ", "exec(", "os.system", "subprocess",
		"popen(", ".popen", "sh -c", "bash -c", "invoke-expression", "iex ",
		"esyscmd", "m4_esyscmd", "check_output", "getoutput", "os.exec",
	}
	networkFetchTokens = []string{
		"curl ", "wget ", "invoke-webrequest", "urllib", "requests.get",
		"requests.post", "urlopen(", "httpx.", "net/http",
	}
)

// behaviourCatalogs pairs each behaviour kind with its token list, in a
// fixed order so a line matching multiple kinds emits findings
// deterministically (stable JSON across runs).
var behaviourCatalogs = []struct {
	kind   Kind
	tokens []string
}{
	{KindDecode, decodeTokens},
	{KindEvalExec, evalExecTokens},
	{KindNetworkFetch, networkFetchTokens},
}

// Scan returns the suspicious occurrences in one build-script file,
// attributing each to rel. Reads bytes as data only — never executes.
//
// Line scanning uses bufio.Reader (not Scanner) so an over-long
// minified/obfuscated line cannot silently halt the scan and hide a
// later hit — the same defense exfilwatch applies.
func Scan(rel string, content []byte) []Finding {
	var findings []Finding
	seen := map[Kind]bool{}

	br := bufio.NewReader(bytes.NewReader(content))
	for line := 1; ; line++ {
		text, err := br.ReadString('\n')
		if len(text) > 0 {
			lower := strings.ToLower(text)
			for _, cat := range behaviourCatalogs {
				if matchesAny(lower, cat.tokens) {
					findings = append(findings, Finding{
						File: rel, Line: line, Kind: cat.kind,
						Severity: SeverityInformational, Snippet: snippet(text),
					})
					seen[cat.kind] = true
				}
			}
			// High-entropy embedded literal — always strong on its own.
			if hasHighEntropyRun(text) {
				findings = append(findings, Finding{
					File: rel, Line: line, Kind: KindHighEntropy,
					Severity: SeverityStrong, Snippet: snippet(text),
				})
			}
		}
		if err != nil {
			break
		}
	}

	// Co-occurrence escalation: a build script that exhibits two or more
	// distinct behaviour classes (decode + exec, fetch + exec, …) is the
	// malware shape. Mark those findings strong. A single class alone
	// (a configure.ac that just shells out) stays informational.
	if distinctBehaviours(seen) >= 2 {
		for i := range findings {
			switch findings[i].Kind {
			case KindDecode, KindEvalExec, KindNetworkFetch:
				findings[i].Severity = SeverityStrong
			}
		}
	}
	return findings
}

func distinctBehaviours(seen map[Kind]bool) int {
	n := 0
	for _, k := range []Kind{KindDecode, KindEvalExec, KindNetworkFetch} {
		if seen[k] {
			n++
		}
	}
	return n
}

func matchesAny(lowerLine string, tokens []string) bool {
	for _, t := range tokens {
		if strings.Contains(lowerLine, t) {
			return true
		}
	}
	return false
}

func snippet(line string) string {
	s := strings.TrimSpace(line)
	if len(s) > maxSnippet {
		s = s[:maxSnippet]
	}
	return s
}
