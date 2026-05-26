package rust

import (
	"context"
	"iter"
	"path/filepath"
	"strings"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// Analyzer is the Rust source analyzer. Stateless across calls; safe
// to reuse. The constructor exists for symmetry with the other
// language analyzers and so options (custom catalogs) can be added
// later without breaking the collector's call site.
type Analyzer struct{}

// NewAnalyzer returns a ready Rust Analyzer.
func NewAnalyzer() *Analyzer { return &Analyzer{} }

// Language names the language this analyzer handles. Feeds
// MatrixValue.Language/Ecosystem — "rust" maps to the cargo ecosystem
// (see matrix.go's ecosystemForLanguage).
func (a *Analyzer) Language() string { return "rust" }

// Analyze parses each file and accumulates astfeature.Counts across
// the whole source tree presented in one call.
//
// Errors yielded by the upstream iterator (e.g. the BlobStreamer
// reporting a blob fetch failure mid-stream) are returned to the
// caller — identical to the golang / python / node analyzers, so a
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
		accumulate(&c, mod, f.Path)
	}
	return c, nil
}

// accumulate folds one parsed module's constructs into c.
//
// path is the source file's posix-style path (relative to repo
// root). It's used only to decide whether calls inside fn main()
// count as ImportTimeCallSites — a build.rs main() body runs at
// `cargo build` and is the dominant cargo supply-chain attack
// vector (2026-05-24 Trapdoor campaign — design/threat-landscape/
// 2026-05-24-trapdoor-crypto-stealer.md). Any other file's fn main()
// is binary-startup execution, not import-time, so does not
// contribute.
//
// InitCount and InstallHookOverrides stay 0 for Rust by design:
//   - Rust has no Go-style package init() function.
//   - There is no source-side install hook to subclass; cargo's
//     install/build lifecycle lives in build.rs (already covered by
//     the build_script_present / build_script_introduced registry
//     signals) and in [package.metadata] declarations, not as
//     subclassable source constructs. Counting a source construct
//     as an install hook here would double-report and lie about
//     where the vector is — same documented gap discipline node has
//     for npm scripts (AST.md §"Known conservative gaps").
//
// DynamicEvalCalls stays 0 for Rust by design: there is no
// language-level eval / exec / __import__ analog. The closest
// approximation — loading a shared object and calling into it via
// libloading or similar — is a generic library use that would
// false-positive on legitimate FFI. Real Rust supply-chain payloads
// (Trapdoor) reach for `std::process::Command` instead, which we
// catch via ExecCalls.
func accumulate(c *astfeature.Counts, mod *Module, path string) {
	c.XORAssignments += mod.XorAssigns

	isBuildRs := filepath.Base(path) == "build.rs"

	for _, call := range mod.Calls {
		// ImportTimeCallSites: any call inside build.rs's fn main()
		// body. The dispatch order with catalog matches doesn't
		// matter — a credential-read inside build.rs's main also
		// counts as an import-time call, just like Python's
		// equivalent.
		if isBuildRs && call.InFn == "main" {
			c.ImportTimeCallSites++
		}

		// Macro calls have their own catalog path; the fn-call
		// catalogs below are skipped for macros so e.g. `println!`
		// with a credential-shaped format string doesn't trip a
		// false EnvCredentialReads.
		if call.Macro {
			handleMacroCall(c, call)
			continue
		}

		switch {
		case matchesCatalog(call.Callee, processExecCallees):
			c.ExecCalls++
		case matchesCatalog(call.Callee, networkCallees) &&
			astfeature.IsCloudMetadataURL(call.FirstArg):
			// A metadata / SSRF-pivot destination is counted
			// distinctly from generic egress — the destination
			// class IS the signal (near-zero false positive).
			// Ordered before the generic network case so it wins.
			c.CloudMetadataCalls++
		case matchesCatalog(call.Callee, networkCallees):
			c.NetworkCallSites++
		case matchesCatalog(call.Callee, envReadCallees) &&
			astfeature.IsCredentialEnvName(call.FirstArg):
			// std::env::var("AWS_SECRET_ACCESS_KEY") shape — only
			// names from the credential catalog count, so
			// ordinary config reads like env::var("PROFILE") never
			// spike (no-false-positive baseline).
			c.EnvCredentialReads++
		case matchesCatalog(call.Callee, writeSinkCallees) &&
			astfeature.IsPersistencePath(call.FirstArg):
			c.SensitivePathWrites++
		case matchesCatalog(call.Callee, base64DecodeCallees):
			c.Base64DecodeCalls++
		case matchesCatalog(call.Callee, pathReadCallees) &&
			astfeature.IsSensitivePath(call.FirstArg):
			c.SensitivePathReads++
		}
	}
}

// handleMacroCall scores the macro analogues of named catalog
// reads. Rust's `env!` / `option_env!` are compile-time env reads —
// same intent as `std::env::var` but resolved at build time. The
// macro-side EnvCredentialReads counts these only when the first
// arg matches the credential catalog, mirroring the fn-side
// no-false-positive baseline.
//
// Other macro names (`println!`, `vec!`, etc.) are not catalog-
// matched: their first arg is a format string or a generic
// expression and matching them as code-from-data would false-
// positive ubiquitously.
func handleMacroCall(c *astfeature.Counts, call Call) {
	switch call.Callee {
	case "env", "option_env":
		if astfeature.IsCredentialEnvName(call.FirstArg) {
			c.EnvCredentialReads++
		}
	}
}

// envReadCallees are the runtime env-var-read API entry points. The
// macro forms (env!, option_env!) are handled separately in
// handleMacroCall — those are compile-time intrinsics whose Macro
// field is true and that take the macro path, not this catalog.
//
// var_os is included because `std::env::var_os` returns an OsString
// instead of String but has the same intent.
var envReadCallees = []string{
	"env::var", "env::var_os",
}

// pathReadCallees are the file-read entry points whose first
// argument is a source path the analyzer can statically resolve
// against IsSensitivePath.
//
// File::open and File::create are split: open here, create in
// writeSinkCallees, because they reflect read vs write intent.
// OpenOptions::new()... chains are a documented receiver-flow gap
// (the actual sensitive arg is on a downstream .open() that the
// parser records with no path context).
var pathReadCallees = []string{
	"fs::read", "fs::read_to_string", "fs::read_dir", "fs::metadata",
	"File::open",
}

// writeSinkCallees are the file-write entry points whose first
// argument is a destination path the analyzer can statically
// resolve against IsPersistencePath.
//
// `fs::create_dir_all` / `fs::create_dir` count because a malicious
// build.rs that mkdir's `~/.ssh/` to plant authorized_keys still
// hits the persistence-path catalog — the directory IS the target.
var writeSinkCallees = []string{
	"fs::write", "fs::create_dir", "fs::create_dir_all",
	"File::create",
}

// base64DecodeCallees are the Rust opaque-payload decode primitives.
//
// Both the deprecated free-function form (`base64::decode`) and the
// modern engine form (`STANDARD.decode`, `URL_SAFE.decode` — the
// `STANDARD` / `URL_SAFE` are constants imported from
// `base64::engine::general_purpose`) are included.
//
// Parity with node's broadened "opaque payload decode" intent for
// this field — flate2 (gzip/zlib) and brotli decoders are common
// stages in obfuscated droppers.
var base64DecodeCallees = []string{
	"base64::decode",
	"STANDARD.decode", "URL_SAFE.decode", "STANDARD_NO_PAD.decode",
	"URL_SAFE_NO_PAD.decode",
	// flate2 / brotli — opaque decompression alongside base64.
	"flate2::read::GzDecoder::new", "flate2::read::ZlibDecoder::new",
	"GzDecoder::new", "ZlibDecoder::new",
	"brotli::Decompressor::new",
}

// processExecCallees are the process-spawn entry points. The
// dominant Trapdoor cargo payload reaches for `std::process::Command::new`
// (and the matched `tokio::process::Command::new` for async crates).
//
// Catalog entry shape is the leading constructor — `Command::new` —
// not the chained `.output()` / `.spawn()` / `.status()` that
// follow it. The chained methods don't carry semantic information
// the catalog needs; what matters is the constructor naming a
// process to run.
var processExecCallees = []string{
	"std::process::Command::new", "Command::new",
	"tokio::process::Command::new",
}

// networkCallees are the HTTP / TCP egress entry points. Catalog
// matches the leading API call — `reqwest::get`, `ureq::get`,
// `TcpStream::connect`. Chained `.send()` after a builder pattern
// is a documented receiver-flow gap (the parser records it as
// `.send` with no useful URL arg).
//
// The catalog focuses on shapes whose first argument is the URL or
// host (so CloudMetadataCalls can resolve the destination class).
// `Client::new()` shapes (where the URL arrives later in the chain)
// hit no useful URL via the first-arg path; they're documented
// gaps with the same posture as node's `axios.create()`.
var networkCallees = []string{
	"reqwest::get", "reqwest::blocking::get",
	"reqwest::post", "reqwest::blocking::post",
	"ureq::get", "ureq::post", "ureq::request",
	"surf::get", "surf::post",
	"hyper::Client::new",
	"std::net::TcpStream::connect", "TcpStream::connect",
	"std::net::UdpSocket::bind", "UdpSocket::bind",
	"std::net::UdpSocket::connect", "UdpSocket::connect",
}

// matchesCatalog reports whether callee equals a catalog entry, or
// — for `::`-DOTTED catalog entries only — ends with a `::`
// boundary + the entry. The boundary split is the specificity
// contract AST.md §4 requires: a method merely named `.var` on
// something else (callee like "config.var") is not `env::var` and
// must not spike the catalog.
//
// Method-chain entries (e.g. `STANDARD.decode`) match exactly OR
// when callee has a `::` separator before the entry: parser
// alias-expansion turns `STANDARD.decode` into something like
// `base64::engine::general_purpose::STANDARD.decode`, and the
// `::STANDARD.decode` boundary fires the suffix match. A method
// chain on an unaliased name remains an exact-only match.
//
// Substrings without a `::` boundary still don't match
// (foo_env::var won't match env::var). Mirrors the python and node
// analyzers' matcher with the same specificity discipline.
func matchesCatalog(callee string, catalog []string) bool {
	for _, entry := range catalog {
		if callee == entry {
			return true
		}
		if strings.Contains(entry, "::") && strings.HasSuffix(callee, "::"+entry) {
			return true
		}
		// Method-chain catalog entry without `::` (e.g.
		// "STANDARD.decode"): still admit a `::` boundary match so
		// alias-expanded paths land. Without this admission, an
		// expanded callee like
		// "base64::engine::general_purpose::STANDARD.decode" would
		// not match the entry "STANDARD.decode" by exact, and the
		// `strings.Contains(entry, "::")` gate would reject it.
		if strings.HasSuffix(callee, "::"+entry) {
			return true
		}
	}
	return false
}
