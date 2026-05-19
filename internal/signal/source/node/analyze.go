package node

import (
	"context"
	"iter"
	"strings"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// Analyzer is the JS/TS source analyzer. Stateless across calls; safe
// to reuse. The constructor exists for symmetry with golang.NewAnalyzer
// / python.NewAnalyzer and so options (catalogs) can be added later
// without breaking the collector's call site.
type Analyzer struct{}

// NewAnalyzer returns a ready JS/TS Analyzer.
func NewAnalyzer() *Analyzer { return &Analyzer{} }

// Language names the language this analyzer handles. Feeds
// MatrixValue.Language/Ecosystem — "javascript" maps to the npm
// ecosystem (covers TypeScript too; "javascript" is the runtime).
func (a *Analyzer) Language() string { return "javascript" }

// Analyze parses each file and accumulates astfeature.Counts across
// the whole source tree presented in one call.
//
// Errors yielded by the upstream iterator (e.g. the BlobStreamer
// reporting a blob fetch failure mid-stream) are returned to the
// caller — identical to golang.Analyzer and python.Analyzer, so a
// partial stream never becomes a deceptively clean all-zero row.
// Context cancellation is honored between files. A file the parser
// can't handle contributes nothing rather than aborting the version's
// whole tree (the parser is lenient, so this is belt-and-braces).
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
			continue
		}
		accumulate(&c, mod)
	}
	return c, nil
}

// accumulate folds one parsed module's constructs into c.
//
// InitCount and InstallHookOverrides stay 0 for JS/TS by design:
// there is no Go-style init, and npm install hooks live in
// package.json scripts (preinstall/install/postinstall) — detected by
// the npm registry collector's postinstall_present /
// postinstall_introduced signals, NOT in source. Counting a source
// construct as an install hook here would double-report and lie about
// where the vector is. This is a documented per-language gap, not an
// oversight (AST.md §"Known conservative gaps").
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
		case isBufferDecode(call):
			// Buffer.from(data, 'base64') is THE npm payload-decode
			// primitive — but the decode intent lives in the SECOND
			// arg, so it needs the resolved encoding, not just the
			// callee. Text encodings (utf8/ascii/latin1/…) are
			// ordinary buffer construction and must NOT count, so the
			// no-false-positive property holds.
			c.Base64DecodeCalls++
		case matchesCatalog(call.Callee, pathReadCallees) && isSensitivePath(call.FirstArg):
			c.SensitivePathReads++
		}
	}
}

// isDynamicEval matches the JS code-from-data primitives — but only
// the bare global or an explicit global.* / vm.* form. Method calls
// reach accumulate with a leading '.' (db.query(q).eval(), re.exec())
// so they can never equal a bare entry here: that specificity is the
// signal. Matching `.eval` / `.exec` by last segment would spike on
// the first regex .exec() or ORM .eval() a package adds — exactly the
// false-positive class AST.md §4 forbids.
func isDynamicEval(callee string) bool {
	switch callee {
	case "eval", "Function",
		"globalThis.eval", "global.eval", "window.eval", "self.eval",
		"globalThis.Function", "global.Function", "window.Function",
		"vm.runInThisContext", "vm.runInNewContext", "vm.runInContext",
		"vm.compileFunction", "vm.Script":
		return true
	}
	return false
}

// bufferDecodeEncodings are the Buffer.from second-arg values that
// mean "decode an opaque payload" rather than "construct a buffer
// from text". Text encodings (utf8/ascii/latin1/binary/utf16le/ucs2)
// are deliberately excluded so ordinary Buffer.from(str,'utf8') and
// Buffer.from(array) never count — the no-false-positive contract.
var bufferDecodeEncodings = map[string]struct{}{
	"base64": {}, "base64url": {}, "hex": {},
}

// isBufferDecode reports whether call is Buffer.from(data, <enc>)
// where <enc> statically resolves to an opaque-payload decode
// encoding. Scoped to the exact `Buffer.from` callee (Buffer is a
// global; a method merely named .from on something else reaches here
// as ".from" and never matches) so the specificity is the signal.
func isBufferDecode(call Call) bool {
	if call.Callee != "Buffer.from" {
		return false
	}
	_, ok := bufferDecodeEncodings[call.SecondArg]
	return ok
}

// The dotted-suffix catalogs. Module names are post-normalization
// (node: scheme stripped) and post-alias-resolution, so a
// require()/import binding still resolves to e.g. child_process.exec.
var (
	processExecCallees = []string{
		"child_process.exec", "child_process.execSync",
		"child_process.spawn", "child_process.spawnSync",
		"child_process.execFile", "child_process.execFileSync",
		"child_process.fork",
	}
	networkCallees = []string{
		"http.request", "http.get", "https.request", "https.get",
		"net.connect", "net.createConnection", "tls.connect",
		"dgram.createSocket",
		// Ubiquitous third-party / global HTTP clients. Bare entries
		// match the global (fetch) or the default-imported callable
		// (axios(...)); dotted entries match the method forms.
		"fetch", "axios", "axios.get", "axios.post", "axios.put",
		"axios.delete", "axios.patch", "axios.request",
		"got", "superagent",
	}
	base64DecodeCallees = []string{
		// Bare global base64 decode.
		"atob",
		// Opaque-payload decompression — as common in obfuscated
		// droppers as base64 (gzip/inflate/brotli stages). Parity with
		// the python analyzer's broadened "opaque payload decode"
		// intent for this field.
		"zlib.gunzipSync", "zlib.gunzip", "zlib.inflateSync",
		"zlib.inflate", "zlib.inflateRawSync", "zlib.brotliDecompressSync",
		"zlib.brotliDecompress", "zlib.unzipSync", "zlib.unzip",
	}
	// pathReadCallees are the file-open sinks whose first argument is a
	// path. Buffer.from(x,'base64') and fs.readFile(handle) where the
	// path is computed are documented conservative gaps (no
	// second-arg / receiver-flow resolution).
	pathReadCallees = []string{
		"fs.readFile", "fs.readFileSync", "fs.createReadStream",
		"fs.open", "fs.openSync", "fs.promises.readFile",
		"fs.promises.open",
	}
)

// matchesCatalog reports whether callee equals a catalog entry or ends
// with a "." boundary + an entry, so dotted suffixes match but
// substrings don't (foo_https.get won't match https.get). Identical
// semantics to the python analyzer's matcher.
func matchesCatalog(callee string, catalog []string) bool {
	for _, entry := range catalog {
		if callee == entry || strings.HasSuffix(callee, "."+entry) {
			return true
		}
	}
	return false
}

// sensitivePathPatterns are credential / secret-material fragments —
// the same language-neutral catalog the python analyzer uses (the
// material is OS/credential-store shaped, not language-shaped).
// Matched as substrings against the backslash-normalized resolved
// path, so "~/.ssh/id_rsa" and "/home/u/.ssh/id_rsa" both hit
// "/.ssh/".
var sensitivePathPatterns = []string{
	"/.ssh/", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	".aws/credentials", ".aws/config", "/.netrc", ".pypirc", ".npmrc",
	".git-credentials", "/.gnupg/", ".docker/config.json",
	"/.kube/config", "/.config/gcloud", "/.azure/", "/etc/shadow",
	"/etc/passwd", ".bash_history", ".zsh_history",
	// Browser / OS credential stores.
	"Login Data", "Cookies", "key4.db", "logins.json",
	"cookies.sqlite", "Local State", "Library/Keychains",
}

// isSensitivePath reports whether a statically-resolved path targets
// credential or secret material. The empty string (unresolved arg) is
// never sensitive — a runtime-built path is a conservative miss, not
// a guess.
func isSensitivePath(p string) bool {
	if p == "" {
		return false
	}
	norm := strings.ReplaceAll(p, "\\", "/")
	for _, pat := range sensitivePathPatterns {
		if strings.Contains(norm, pat) {
			return true
		}
	}
	// Bare dotenv file: basename exactly ".env" (avoid matching
	// "environment.cfg" or ".envrc").
	base := norm
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base == ".env"
}
