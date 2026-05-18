// Package python is the Python source-evolution analyzer. It mirrors
// the golang package's role: turn a stream of source files into an
// astfeature.Counts per version.
//
// It uses the hand-written lexer+parser in this package (no external
// dependency — a stale third-party Python parser is itself a
// supply-chain risk in a supply-chain tool) to count the
// security-relevant constructs that map to astfeature.Counts plus the
// two Python-shaped cross-language fields (DynamicEvalCalls,
// ImportTimeCallSites). It deliberately preserves the golang.Analyzer
// error/ctx contract so the Assembler treats both analyzers
// identically: a mid-stream provider error aborts the row rather than
// producing a misleading all-zero matrix entry.
package python

import (
	"context"
	"iter"
	"strings"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// Analyzer is the Python source analyzer. Stateless across calls;
// safe to reuse. The constructor exists for symmetry with
// golang.NewAnalyzer and so item #4 can add options (parser choice,
// pattern catalogs) without breaking the collector's call site.
type Analyzer struct{}

// NewAnalyzer returns a ready Python Analyzer.
func NewAnalyzer() *Analyzer { return &Analyzer{} }

// Language names the language this analyzer handles. Feeds
// MatrixValue.Language/Ecosystem ("python" → pypi ecosystem).
func (a *Analyzer) Language() string { return "python" }

// Analyze parses each file and accumulates astfeature.Counts across
// the whole source tree presented in one call.
//
// Errors yielded by the upstream iterator (e.g. the BlobStreamer
// reporting a blob fetch failure mid-stream) are returned to the
// caller — identical to golang.Analyzer, so a partial stream never
// becomes a deceptively clean all-zero row. Context cancellation is
// honored between files.
func (a *Analyzer) Analyze(ctx context.Context, files iter.Seq2[astfeature.SourceFile, error]) (astfeature.Counts, error) {
	var c astfeature.Counts
	for f, err := range files {
		if err != nil {
			return astfeature.Counts{}, err
		}
		if err := ctx.Err(); err != nil {
			return astfeature.Counts{}, err
		}
		mod, perr := Parse(f.Content)
		if perr != nil {
			// Lenient: a file the parser can't handle contributes
			// nothing rather than aborting the version's whole tree.
			continue
		}
		accumulate(&c, mod)
	}
	return c, nil
}

// accumulate folds one parsed module's constructs into c.
func accumulate(c *astfeature.Counts, mod *Module) {
	c.XORAssignments += mod.XorAssigns
	for _, call := range mod.Calls {
		if call.ModuleScope {
			c.ImportTimeCallSites++
		}
		switch {
		case isDynamicEval(call.Callee):
			c.DynamicEvalCalls++
		case matchesCatalog(call.Callee, processExecCallees):
			c.ExecCalls++
		case matchesCatalog(call.Callee, networkCallees):
			c.NetworkCallSites++
		case matchesCatalog(call.Callee, base64DecodeCallees):
			c.Base64DecodeCalls++
		}
	}
}

// isDynamicEval matches the code-from-data primitives — but only the
// bare builtin or an explicit builtins.<name>. Matching by last
// segment would count re.compile (regex), obj.eval (ORM/NumPy),
// self.exec (any method) as attacks: a huge false-positive surface
// that would spike on the first regex a package adds. The builtins
// are global, unqualified names; that specificity is the signal.
func isDynamicEval(callee string) bool {
	switch callee {
	case "eval", "exec", "compile", "__import__",
		"builtins.eval", "builtins.exec", "builtins.compile",
		"builtins.__import__", "importlib.import_module":
		return true
	}
	return false
}

// The catalogs match by dotted suffix so an aliased import path
// (urllib.request.urlopen vs request.urlopen) still resolves while
// staying specific enough to avoid matching unrelated .get/.post.
var (
	processExecCallees = []string{
		"os.system", "os.popen", "os.execv", "os.execve", "os.execl",
		"subprocess.run", "subprocess.Popen", "subprocess.call",
		"subprocess.check_call", "subprocess.check_output",
		"subprocess.getoutput", "subprocess.getstatusoutput",
	}
	networkCallees = []string{
		"request.urlopen", "urllib.urlopen", "requests.get",
		"requests.post", "requests.put", "requests.request",
		"requests.head", "requests.patch", "requests.delete",
		"socket.socket", "socket.create_connection", "httpx.get",
		"httpx.post", "httpx.request", "http.client.HTTPConnection",
		"http.client.HTTPSConnection", "ftplib.FTP", "smtplib.SMTP",
	}
	base64DecodeCallees = []string{
		"base64.b64decode", "base64.b32decode", "base64.b16decode",
		"base64.a85decode", "base64.b85decode", "base64.decodebytes",
		"base64.urlsafe_b64decode", "base64.standard_b64decode",
		"binascii.a2b_base64", "codecs.decode",
	}
)

// matchesCatalog reports whether callee equals or ends with a "."
// boundary + a catalog entry, so dotted suffixes match but
// substrings don't (foo_requests.get won't match requests.get).
func matchesCatalog(callee string, catalog []string) bool {
	for _, entry := range catalog {
		if callee == entry || strings.HasSuffix(callee, "."+entry) {
			return true
		}
	}
	return false
}
