package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/store"

	_ "modernc.org/sqlite"
)

// TestShowAnalyses_JSON_WithResults verifies that --json returns a
// structured array of outputs when analyses exist for the target.
func TestShowAnalyses_JSON_WithResults(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	sess := newTestAnalysisSession(t, s,
		"https://github.com/JedWatson/classnames",
		[]string{"signatory-security-v1"},
	)
	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")

	var stdout bytes.Buffer
	cmd := &ShowAnalysesCmd{
		Target: "https://github.com/JedWatson/classnames",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowAnalysesResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "ok", result.Status)
	require.Len(t, result.Analyses, 1)
	assert.Equal(t, "signatory-security-v1", result.Analyses[0].AnalystID)
	assert.NotEmpty(t, result.Analyses[0].OutputID)
}

// TestShowAnalyses_JSON_NoEntity verifies that --json returns a
// clear status when the target has never been ingested.
func TestShowAnalyses_JSON_NoEntity(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)

	var stdout bytes.Buffer
	cmd := &ShowAnalysesCmd{
		Target: "https://github.com/nonexistent/repo",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowAnalysesResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "no_entity", result.Status)
	assert.Empty(t, result.Analyses)
}

// TestShowAnalyses_JSON_NoAnalyses verifies that --json returns an
// empty array (not an error) when the entity exists but has no
// outputs.
func TestShowAnalyses_JSON_NoAnalyses(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	// Create an entity but don't ingest anything.
	_, err = ensureEntity(ctx, s, "https://github.com/JedWatson/classnames")
	require.NoError(t, err)

	var stdout bytes.Buffer
	cmd := &ShowAnalysesCmd{
		Target: "https://github.com/JedWatson/classnames",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowAnalysesResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "empty", result.Status)
	assert.Empty(t, result.Analyses)
}

// TestShowAnalyses_JSON_VanityResolution covers the golang.org/x/mod
// → repo:github/golang/mod equivalence at the show-analyses surface.
//
// The bug, surfaced 2026-05-07: show-analyses' target resolution went
// straight through profile.ResolveTarget + FindEntityByURI without the
// alternate-URI walking that summary already used via
// store.LookupEntity. So a vanity Go path canonicalized to
// pkg:golang/golang.org/x/mod, looked up that exact URI, missed,
// and printed "no entity matches" — even when an entity row existed
// at the equivalent repo:github/golang/mod.
//
// Test shape: seed an analyst output indexed under the resolved
// repo URI, then query with the vanity input. The fix puts the show-*
// commands on the same alternate-walk footing as summary; this
// regression test pins the new contract.
func TestShowAnalyses_JSON_VanityResolution(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	// Seed at the resolved canonical (repo:github/golang/mod). The
	// session's TargetURI is the same canonical so the entity row
	// gets created at that URI by ensureEntity.
	sess := newTestAnalysisSession(t, s,
		"https://github.com/golang/mod",
		[]string{"signatory-security-v1"})

	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-security-v1",
			Round:     1,
		},
		Target: "https://github.com/golang/mod",
		Conclusions: []exchange.Conclusion{{
			ID:        "T001",
			Verdict:   "vanity-walk fixture",
			Rationale: "fixture for show-analyses alternate-walk regression",
			Severity:  exchange.Severity{Default: exchange.SeverityInformational},
			Category:  "test",
		}},
	}
	_, err = s.IngestAnalystOutput(ctx, out, "",
		store.WithAnalysisSession(sess.ID))
	require.NoError(t, err)

	// Query with the vanity form. Pre-fix this returned "no_entity";
	// post-fix LookupEntity walks pkg:golang/... → pkg:go/... →
	// repo:github/golang/mod and finds the analysis.
	var stdout bytes.Buffer
	cmd := &ShowAnalysesCmd{
		Target: "golang.org/x/mod",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowAnalysesResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "ok", result.Status,
		"vanity input must resolve through alternate-URI walking; "+
			"got status=%q (pre-fix value was %q)", result.Status, "no_entity")
	require.Len(t, result.Analyses, 1)
	assert.Equal(t, "signatory-security-v1", result.Analyses[0].AnalystID)
	assert.Equal(t, "repo:github/golang/mod", result.Analyses[0].EntityURI,
		"the listing must surface the canonical entity URI, not the input vanity form")
}
