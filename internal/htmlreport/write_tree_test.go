package htmlreport

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// writeTreeFixture returns the synthesis + analyst outputs the
// directory-writer tests share. Mirrors linkPlanFixture* but lives
// here so the tests can mutate independently.
func writeTreeFixture() (synth *exchange.AnalystOutput, loaded map[string]*exchange.AnalystOutput) {
	signalReleaseCadence := "release-cadence"
	synth = &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			Model:     "claude-opus-4-7",
			InvokedAt: "2026-05-06T12:00:00Z",
		},
		Target: "pkg:npm/example",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierTrustedForNow,
				RationaleSummary: "narrow risk surface",
			},
			Reasoning: "single-paragraph reasoning.",
			Summary:   "two-sentence summary.",
			KeyConclusionRefs: []exchange.ConclusionRef{
				{OutputID: "out-sec-0001abcdef", ConclusionLocalID: "F003", Weight: 1},
				{OutputID: "out-prov-9876543210", ConclusionLocalID: "F012", Weight: 2},
				// Dangling — drives a stub page.
				{OutputID: "out-ghost", ConclusionLocalID: "F999", Weight: 3},
			},
		},
	}

	loaded = map[string]*exchange.AnalystOutput{
		"out-sec-0001abcdef": {
			Attribution: exchange.AgentAttribution{
				AnalystID: "signatory-security-v1",
				Round:     1,
			},
			Target: "pkg:npm/example",
			Conclusions: []exchange.Conclusion{
				{
					ID:         "F003",
					Verdict:    "Slowed cadence",
					Severity:   exchange.Severity{Default: exchange.SeverityHigh},
					Category:   "vitality",
					SignalType: &signalReleaseCadence,
				},
			},
		},
		"out-prov-9876543210": {
			Attribution: exchange.AgentAttribution{
				AnalystID: "signatory-provenance-v1",
				Round:     1,
			},
			Target: "pkg:npm/example",
			Conclusions: []exchange.Conclusion{
				{
					ID:       "F012",
					Verdict:  "Authors verified",
					Severity: exchange.Severity{Default: exchange.SeverityPositive},
					Category: "publication",
				},
			},
		},
	}
	return synth, loaded
}

// fixtureWriteTreeInput composes a complete WriteReportTreeInput from
// the shared fixture plus a parent directory. Tests vary parentDir
// to drive refusal paths.
func fixtureWriteTreeInput(parentDir string) WriteReportTreeInput {
	synth, loaded := writeTreeFixture()
	return WriteReportTreeInput{
		ParentDir:         parentDir,
		Synth:             synth,
		SynthesisOutputID: "synth-uuid-aaaaaaaa-bbbb-cccc-dddd",
		ShortName:         "example",
		Loaded:            loaded,
		GeneratedAt:       "2026-05-06T13:00:00Z",
		Version:           "v0.1.0-test",
	}
}

func TestWriteReportTree_RefuseOnMissingParent(t *testing.T) {
	in := fixtureWriteTreeInput(filepath.Join(t.TempDir(), "does-not-exist"))
	_, err := WriteReportTree(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}

func TestWriteReportTree_RefuseOnUnwriteableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics differ on Windows; skip")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses dir permissions; skip")
	}
	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o555))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	in := fixtureWriteTreeInput(parent)
	_, err := WriteReportTree(in)
	require.Error(t, err)
}

func TestWriteReportTree_RefuseIfSubdirExists(t *testing.T) {
	parent := t.TempDir()
	in := fixtureWriteTreeInput(parent)

	// First write succeeds, materializing the auto-named subdir.
	indexPath, err := WriteReportTree(in)
	require.NoError(t, err)
	subdir := filepath.Base(filepath.Dir(indexPath))

	// Second write to the same parent with the same SynthesisOutputID
	// must refuse — the auto-named subdir already exists.
	_, err = WriteReportTree(in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), subdir,
		"refusal error should name the colliding subdir for operator recovery")
}

func TestWriteReportTree_HappyPath(t *testing.T) {
	parent := t.TempDir()
	in := fixtureWriteTreeInput(parent)

	indexPath, err := WriteReportTree(in)
	require.NoError(t, err)

	t.Run("indexPath is absolute and exists", func(t *testing.T) {
		assert.True(t, filepath.IsAbs(indexPath), "indexPath should be absolute")
		fi, err := os.Stat(indexPath)
		require.NoError(t, err)
		require.False(t, fi.IsDir())
		assert.Equal(t, "index.html", filepath.Base(indexPath))
	})

	subdir := filepath.Dir(indexPath)

	t.Run("subdir name is <short>-<output-short>", func(t *testing.T) {
		base := filepath.Base(subdir)
		assert.Contains(t, base, "example-",
			"subdir should start with the short name slug")
	})

	t.Run("conclusions/ contains a page per resolvable KeyConclusionRef", func(t *testing.T) {
		// Slug is conclusions/<first-8-chars-of-output-id>-<local>.html.
		// Fixture output ids:
		//   "out-sec-0001abcdef"  → first 8 = "out-sec-" (trailing dash)
		//   "out-prov-9876543210" → first 8 = "out-prov"
		// Producing "out-sec--F003" (double dash) and "out-prov-F012".
		_, err := os.Stat(filepath.Join(subdir, "conclusions", "out-sec--F003.html"))
		assert.NoError(t, err)
		_, err = os.Stat(filepath.Join(subdir, "conclusions", "out-prov-F012.html"))
		assert.NoError(t, err)
	})

	t.Run("conclusions/ contains a stub page per Dangling ref", func(t *testing.T) {
		// "out-ghost" is 9 chars, so shortOutputID truncates to
		// "out-ghos".
		stub := filepath.Join(subdir, "conclusions", "out-ghos-F999.html")
		b, err := os.ReadFile(stub)
		require.NoError(t, err)
		assert.Contains(t, string(b), "missing-reference-banner")
		assert.Contains(t, string(b), "F999")
	})

	t.Run("analysts/ contains one page per loaded analyst output", func(t *testing.T) {
		_, err := os.Stat(filepath.Join(subdir, "analysts", "signatory-security-v1-r1.html"))
		assert.NoError(t, err)
		_, err = os.Stat(filepath.Join(subdir, "analysts", "signatory-provenance-v1-r1.html"))
		assert.NoError(t, err)
	})

	t.Run("assets/style.css matches embed byte-for-byte", func(t *testing.T) {
		got, err := os.ReadFile(filepath.Join(subdir, "assets", "style.css"))
		require.NoError(t, err)
		assert.True(t, bytes.Equal(got, StyleCSS()),
			"on-disk style.css should byte-equal the embedded source")
	})

	t.Run("index.html links resolve to files that exist", func(t *testing.T) {
		// Surface check: the index renders anchors that match the
		// conclusion-page filenames we just verified.
		idx, err := os.ReadFile(indexPath)
		require.NoError(t, err)
		assert.Contains(t, string(idx), "conclusions/out-sec--F003.html")
		assert.Contains(t, string(idx), "conclusions/out-prov-F012.html")
	})
}
