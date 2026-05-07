package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// TestShowSynthesis_HappyPath_RendersAllSections exercises the full
// synthesis-supplement shape: every optional array populated, every
// string non-empty. The rendered markdown should include the
// posture tier, reasoning, summary, a concordance entry, a
// contradiction entry, ranked key conclusions, gaps, action items,
// and notes.
func TestShowSynthesis_HappyPath_RendersAllSections(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: "pkg:npm/show-synthesis-example",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierVettedFrozen,
				VersionScope:     "2.2.4",
				RationaleSummary: "short rationale",
			},
			Reasoning: "multi-paragraph reasoning body",
			Summary:   "two-sentence summary",
			ConcordanceStrengths: []exchange.ConcordanceEntry{
				{
					Topic:         "minimal dependency surface",
					Description:   "both analysts arrived at zero-dep independently",
					AnalystRefs:   []string{"signatory-provenance", "external-sec-v1"},
					ConclusionIDs: []string{"F005", "O001"},
					Confidence:    "HIGH",
				},
			},
			ContradictionsDetected: []exchange.ContradictionEntry{
				{
					Topic:                "release cadence",
					Description:          "provenance healthy; security slow",
					SupportingAnalystA:   "signatory-provenance",
					SupportingAnalystB:   "external-sec-v1",
					ConclusionIDsA:       []string{"F003"},
					ConclusionIDsB:       []string{"F011"},
					ResolutionPreference: "prefer provenance's read",
				},
			},
			KeyConclusionRefs: []exchange.ConclusionRef{
				{
					OutputID:          "abcdef0123456789",
					ConclusionLocalID: "F002",
					Weight:            1,
					ForgeryResistance: "VERY HIGH",
					RelevanceNote:     "publication anchor is the load-bearing signal",
				},
				{
					OutputID:          "fedcba9876543210",
					ConclusionLocalID: "F001",
					Weight:            2,
					ForgeryResistance: "HIGH",
				},
			},
			Gaps:        []string{"no OSV cross-check", "transitives not audited"},
			ActionItems: []string{"pin in go.sum", "validate CreateFromVCS inputs"},
			Notes:       "confidence slightly shaded by mid-analysis upstream update",
		},
	})

	cmd := &ShowSynthesisCmd{OutputID: outputID}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "# Trust Assessment:",
		"render must carry the trust-assessment heading")
	assert.Contains(t, stdout, "show-synthesis-example",
		"render must identify the target")
	assert.Contains(t, stdout, "**Posture: vetted-frozen**",
		"render must surface the proposed tier")
	assert.Contains(t, stdout, "**Version scope: 2.2.4**",
		"render must surface the version scope when present")
	assert.Contains(t, stdout, "multi-paragraph reasoning body",
		"render must include the reasoning body verbatim")
	assert.Contains(t, stdout, "two-sentence summary",
		"render must include the summary")
	assert.Contains(t, stdout, "minimal dependency surface",
		"render must include concordance entries")
	assert.Contains(t, stdout, "release cadence",
		"render must include contradiction entries")
	assert.Contains(t, stdout, "F002",
		"render must name key conclusion local ids")
	assert.Contains(t, stdout, "VERY HIGH",
		"render must include forgery resistance labels")
	assert.Contains(t, stdout, "no OSV cross-check",
		"render must include gaps")
	assert.Contains(t, stdout, "pin in go.sum",
		"render must include action items")
	assert.Contains(t, stdout, "confidence slightly shaded",
		"render must include notes when present")
}

// TestShowSynthesis_MinimalSupplement_OmitsEmptySections asserts the
// render degrades gracefully when optional arrays are empty —
// produces the required sections (posture, reasoning, summary)
// without empty "Concordance", "Gaps", or "Action Items" headers.
func TestShowSynthesis_MinimalSupplement_OmitsEmptySections(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &ShowSynthesisCmd{OutputID: outputID}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "synthesis reasoning body")
	assert.Contains(t, stdout, "synthesis summary")
	assert.NotContains(t, stdout, "## Cross-analyst Concordance",
		"empty concordance + contradictions → omit the section")
	assert.NotContains(t, stdout, "## Key Conclusions",
		"empty key_conclusion_refs → omit the section")
	assert.NotContains(t, stdout, "## Gaps and Limitations",
		"empty gaps → omit the section")
	assert.NotContains(t, stdout, "## Action Items",
		"empty action items → omit the section")
	assert.NotContains(t, stdout, "## Notes",
		"empty notes → omit the section")
}

// TestShowSynthesis_UnknownOutputID_Errors asserts a clean error
// for a UUID that isn't in the store.
func TestShowSynthesis_UnknownOutputID_Errors(t *testing.T) {
	g := newTestGlobals(t)
	cmd := &ShowSynthesisCmd{OutputID: "00000000-0000-0000-0000-000000000000"}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no analyst output")
}

// TestShowSynthesis_NonSynthesisOutput_Errors asserts the command
// refuses to render a non-synthesis output (security/provenance
// outputs don't carry a synthesis_supplement and have a different
// render shape). The error points to the right alternative verb.
func TestShowSynthesis_NonSynthesisOutput_Errors(t *testing.T) {
	g := newTestGlobals(t)

	lineStart := 10
	analyst := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: "pkg:npm/non-synthesis",
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "c",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: &lineStart},
				},
			},
		},
	}
	outputID := ingestSynthesisForAccept(t, g, analyst)

	cmd := &ShowSynthesisCmd{OutputID: outputID}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a synthesis",
		"error must explain that the output isn't a synthesis")
	assert.Contains(t, err.Error(), "show-analyses",
		"error should point at the right alternative verb for non-synthesis outputs")
}

// TestShowSynthesis_HTML_HappyPath exercises the --html mode end to
// end against a real store. Two analyst outputs are ingested, then a
// synthesis whose KeyConclusionRefs point at specific (output_id,
// local_id) pairs across both. The command should produce a tree
// with index.html, per-conclusion pages for each referenced finding,
// per-analyst pages, and assets/style.css.
//
// Asserts on structural invariants (every conclusion-page link in
// the index resolves to an existing file; severity classes match the
// source data) — not on byte-level HTML, which the htmlreport
// package's own unit tests cover.
func TestShowSynthesis_HTML_HappyPath(t *testing.T) {
	g := newTestGlobals(t)

	// All three analyst outputs (security, provenance, synthesis)
	// belong to one entity and one analysis session. The shared
	// ingestSynthesisForAccept helper would create a fresh entity
	// each call and collide on the URI uniqueness constraint, so
	// this test sets up the entity+session once and ingests through
	// it.
	target := "pkg:npm/html-mode-example"
	sessionID := setupSharedAnalysisSession(t, g, target, "test")

	// 1. Ingest the security analyst output.
	securityOutputID := ingestIntoSharedSession(t, g, sessionID, &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-security-v1",
			Round:     1,
		},
		Target: "pkg:npm/html-mode-example",
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F001",
				Verdict:   "Authors verified via gpg signatures",
				Rationale: "Every release tag carries a maintainer-signed annotation.",
				Severity:  exchange.Severity{Default: exchange.SeverityPositive},
				Category:  "publication",
			},
			{
				ID:        "F003",
				Verdict:   "Release cadence has slowed materially since 2025-Q3",
				Rationale: "14 releases in 2024; 3 in 2025.",
				Severity:  exchange.Severity{Default: exchange.SeverityHigh},
				Category:  "vitality",
			},
		},
	})

	// 2. Ingest the provenance analyst output.
	provenanceOutputID := ingestIntoSharedSession(t, g, sessionID, &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-provenance-v1",
			Round:     1,
		},
		Target: "pkg:npm/html-mode-example",
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F010",
				Verdict:   "Single-maintainer ownership over 4-year window",
				Rationale: "No co-maintainer transitions in the audit window.",
				Severity:  exchange.Severity{Default: exchange.SeverityInformational},
				Category:  "personality",
			},
		},
	})

	// 3. Ingest the synthesis that references both. KeyConclusionRefs
	// drive both the link plan and the rendered key-conclusions list.
	synthesisOutputID := ingestIntoSharedSession(t, g, sessionID, &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{AnalystID: "signatory-synthesis-v1"},
		Target:      "pkg:npm/html-mode-example",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierTrustedForNow,
				RationaleSummary: "narrow surface, clean trajectory",
			},
			Reasoning: "single-paragraph reasoning body for the integration test.",
			Summary:   "two-sentence summary.",
			KeyConclusionRefs: []exchange.ConclusionRef{
				{OutputID: securityOutputID, ConclusionLocalID: "F003", Weight: 1, ForgeryResistance: "HIGH"},
				{OutputID: securityOutputID, ConclusionLocalID: "F001", Weight: 2, ForgeryResistance: "VERY HIGH"},
				{OutputID: provenanceOutputID, ConclusionLocalID: "F010", Weight: 3, ForgeryResistance: "MEDIUM"},
				// Dangling: output id that won't exist in the store.
				{OutputID: "00000000-0000-0000-0000-000000000000", ConclusionLocalID: "F999", Weight: 4, ForgeryResistance: "LOW"},
			},
		},
	})

	// 4. Run with --html.
	parent := t.TempDir()
	cmd := &ShowSynthesisCmd{
		OutputID: synthesisOutputID,
		HTMLDir:  parent,
	}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	t.Run("stdout is exactly the absolute index.html path", func(t *testing.T) {
		// The path must be on its own line, no surrounding prose,
		// so `open "$(signatory show-synthesis ID --html=DIR)"` works.
		line := strings.TrimSpace(stdout)
		require.NotEmpty(t, line)
		assert.True(t, filepath.IsAbs(line), "indexPath must be absolute: %q", line)
		assert.Equal(t, "index.html", filepath.Base(line))
		_, err := os.Stat(line)
		assert.NoError(t, err, "indexPath must point at a real file")
	})

	indexPath := strings.TrimSpace(stdout)
	subdir := filepath.Dir(indexPath)

	t.Run("subdir auto-named under the supplied parent", func(t *testing.T) {
		assert.Equal(t, parent, filepath.Dir(subdir),
			"subdir must sit directly inside the supplied parent")
	})

	t.Run("conclusions/ has a page for every referenced finding from a loaded output", func(t *testing.T) {
		entries, err := os.ReadDir(filepath.Join(subdir, "conclusions"))
		require.NoError(t, err)
		// 3 resolvable + 1 stub for the dangling ref.
		assert.Len(t, entries, 4)

		// Each resolvable page exists (slugs use the first 8 chars of
		// the output uuid, but we don't pin exact filenames here —
		// we assert via the link graph below).
	})

	t.Run("analysts/ has one page per loaded analyst output", func(t *testing.T) {
		_, err := os.Stat(filepath.Join(subdir, "analysts", "signatory-security-v1-r1.html"))
		assert.NoError(t, err)
		_, err = os.Stat(filepath.Join(subdir, "analysts", "signatory-provenance-v1-r1.html"))
		assert.NoError(t, err)
	})

	t.Run("assets/style.css present and non-empty", func(t *testing.T) {
		fi, err := os.Stat(filepath.Join(subdir, "assets", "style.css"))
		require.NoError(t, err)
		assert.Greater(t, fi.Size(), int64(100), "style.css should not be empty")
	})

	t.Run("every anchor in index.html resolves to a real file", func(t *testing.T) {
		// Cross-link integrity: parse out href="..." values from
		// the index and confirm each referenced relative path
		// exists on disk under subdir.
		idx, err := os.ReadFile(indexPath)
		require.NoError(t, err)
		hrefs := extractRelativeHrefs(string(idx))
		require.NotEmpty(t, hrefs, "index should contain anchors")
		for _, href := range hrefs {
			full := filepath.Join(subdir, href)
			_, err := os.Stat(full)
			assert.NoError(t, err, "anchor href %q must point at an existing file", href)
		}
	})

	t.Run("dangling KeyConclusionRef gets a stub page with the missing-reference banner", func(t *testing.T) {
		// Find the stub by walking conclusions/ and looking for the
		// banner class — the slug uses the first 8 chars of the
		// dangling output id, which is "00000000".
		matches, err := filepath.Glob(filepath.Join(subdir, "conclusions", "00000000-F999.html"))
		require.NoError(t, err)
		require.Len(t, matches, 1)
		b, err := os.ReadFile(matches[0])
		require.NoError(t, err)
		assert.Contains(t, string(b), "missing-reference-banner")
	})

	t.Run("severity classes flow from store data into rendered HTML", func(t *testing.T) {
		// F003 was high; F001 positive. Both should surface their
		// CSS class on their conclusion pages. Filename matches
		// exactly because we know the slug rule.
		entries, err := os.ReadDir(filepath.Join(subdir, "conclusions"))
		require.NoError(t, err)

		// Concatenate every conclusion page's bytes; assert the
		// classes appear somewhere across the set.
		var combined string
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(subdir, "conclusions", e.Name()))
			require.NoError(t, err)
			combined += string(b)
		}
		assert.Contains(t, combined, "severity-high",
			"F003's high severity must reach the rendered HTML")
		assert.Contains(t, combined, "severity-positive",
			"F001's positive severity must reach the rendered HTML")
		assert.Contains(t, combined, "severity-informational",
			"F010's informational severity must reach the rendered HTML")
	})
}

// setupSharedAnalysisSession creates one entity + one in-progress
// analysis session for target and returns the session id. Tests
// then ingest several analyst outputs into the same session via
// ingestIntoSharedSession — the shared ingestSynthesisForAccept
// helper creates a fresh entity per call and collides on the URI
// uniqueness constraint when used for multi-output scenarios.
func setupSharedAnalysisSession(t *testing.T, g *Globals, target, shortName string) string {
	t.Helper()
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	sqliteStore, ok := s.(*store.SQLite)
	require.True(t, ok, "test setup expects *store.SQLite")

	entityURI, _ := profile.SplitURIVersion(target)
	entity := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: entityURI,
		Type:         profile.EntityProject,
		ShortName:    shortName,
	}
	require.NoError(t, sqliteStore.PutEntity(ctx, entity))

	session := &profile.AnalysisSession{
		ID:        profile.NewEntityID(),
		EntityID:  entity.ID,
		TargetURI: entityURI,
		InvokedBy: "html-mode-test",
		StartedAt: time.Now().UTC(),
		Status:    profile.AnalysisSessionInProgress,
	}
	require.NoError(t, sqliteStore.CreateAnalysisSession(ctx, session))
	return session.ID
}

// ingestIntoSharedSession ingests one AnalystOutput against an
// existing session and returns the resulting OutputID. Pairs with
// setupSharedAnalysisSession.
func ingestIntoSharedSession(t *testing.T, g *Globals, sessionID string, out *exchange.AnalystOutput) string {
	t.Helper()
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	result, err := s.IngestAnalystOutput(ctx, out, "html-mode-test",
		store.WithAnalysisSession(sessionID))
	require.NoError(t, err)
	return result.OutputID
}

// extractRelativeHrefs pulls every href="<rel>" value out of html
// where the value does NOT start with "/" or "http". Naive but
// adequate for the integration test's structural assertions; the
// htmlreport unit tests cover correctness of individual anchors.
func extractRelativeHrefs(html string) []string {
	var hrefs []string
	const marker = `href="`
	for {
		i := strings.Index(html, marker)
		if i < 0 {
			break
		}
		html = html[i+len(marker):]
		j := strings.Index(html, `"`)
		if j < 0 {
			break
		}
		val := html[:j]
		html = html[j+1:]
		if val == "" || strings.HasPrefix(val, "/") || strings.HasPrefix(val, "http") {
			continue
		}
		hrefs = append(hrefs, val)
	}
	return hrefs
}
