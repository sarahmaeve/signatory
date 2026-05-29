package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rhtml "github.com/sarahmaeve/pr-analyzer/render/html"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// TestDeepScan_sections pins the pure mapper: stored findings + burn →
// pr-analyzer drill-down sections in the pra-pill vocabulary.
func TestDeepScan_sections(t *testing.T) {
	t.Parallel()

	secs := deepScan{
		Verdict:    "block",
		Reasons:    []string{"6 exfil-host reference(s)"},
		ExfilHosts: []string{"webhook.site (init.go:5)"},
		Burned:     true,
		BurnVia:    "author identity:github/mallory",
		BurnReason: "malicious PR",
	}.sections()

	require.Len(t, secs, 1)
	s := secs[0]
	assert.Equal(t, "pr-scan deep findings", s.Title)
	assert.Contains(t, s.Pills, rhtml.Pill{Text: "BLOCK", Tier: "danger"})
	assert.Contains(t, s.Pills, rhtml.Pill{Text: "AUTHOR BURNED", Tier: "danger"})
	assert.Contains(t, s.Rows, rhtml.Row{Term: "Verdict reason", Detail: "6 exfil-host reference(s)"})
	assert.Contains(t, s.Rows, rhtml.Row{Term: "Exfil hosts", Detail: "webhook.site (init.go:5)"})
	assert.Contains(t, s.Rows, rhtml.Row{Term: "Author burned", Detail: "malicious PR (via author identity:github/mallory)"})
}

// TestDeepScan_sections_empty: a PR with neither a verdict nor a burn
// contributes no enrichment.
func TestDeepScan_sections_empty(t *testing.T) {
	t.Parallel()
	assert.Nil(t, deepScan{}.sections())
}

// TestDeepScan_sections_verdictTiers maps each verdict to its pill tier.
func TestDeepScan_sections_verdictTiers(t *testing.T) {
	t.Parallel()
	for verdict, want := range map[string]rhtml.Pill{
		"block": {Text: "BLOCK", Tier: "danger"},
		"warn":  {Text: "WARN", Tier: "warning"},
		"clear": {Text: "CLEAR", Tier: "success"},
	} {
		secs := deepScan{Verdict: verdict}.sections()
		require.Len(t, secs, 1, "verdict %q", verdict)
		assert.Contains(t, secs[0].Pills, want, "verdict %q", verdict)
	}
}

// TestFunctional_PRScanReport_EmitsSidecar drives the command end to end:
// signatory scanned PR #1 of octo/hello (block + exfil) but not #2. The
// emitted pr-scan.js sidecar must assign the loader global, carry #1's
// deep sections (verdict pill + exfil row), and omit the unscanned #2 —
// and it must touch no index.html (it only writes the sidecar).
func TestFunctional_PRScanReport_EmitsSidecar(t *testing.T) {
	t.Parallel()
	globals := testGlobals(t)
	seedScannedPR(t, globals, "patch:github/octo/hello/1")

	out := t.TempDir()
	var msg bytes.Buffer
	require.NoError(t, (&PRScanReportCmd{Repo: "octo/hello", Out: out, Stdout: &msg}).Run(globals))

	// The command writes exactly the sidecar — never an index.html.
	if _, err := os.Stat(filepath.Join(out, "index.html")); !os.IsNotExist(err) {
		t.Fatalf("pr-scan report must not create/modify index.html (stat err: %v)", err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "pr-scan.js"))
	require.NoError(t, err)
	body := string(raw)

	const prefix = "window.__praEnrichment = "
	require.True(t, strings.HasPrefix(body, prefix), "sidecar must assign the loader global:\n%s", body)
	payload := strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(body, prefix)), ";")
	var enrich map[string][]rhtml.Section
	require.NoError(t, json.Unmarshal([]byte(payload), &enrich))

	secs, ok := enrich["1"]
	require.True(t, ok, "scanned PR #1 must appear in the sidecar")
	require.Len(t, secs, 1)
	assert.Contains(t, secs[0].Pills, rhtml.Pill{Text: "BLOCK", Tier: "danger"})
	var hasExfil bool
	for _, r := range secs[0].Rows {
		if r.Term == "Exfil hosts" && strings.Contains(r.Detail, "webhook.site (telemetry/init.go:6)") {
			hasExfil = true
		}
	}
	assert.True(t, hasExfil, "exfil-host row expected in #1's section; got %+v", secs[0].Rows)

	_, has2 := enrich["2"]
	assert.False(t, has2, "unscanned PR #2 must not appear in the sidecar")
	assert.Contains(t, msg.String(), "pr-scan.js")
}

// seedScannedPR mints a patch entity and stores a block verdict + one
// exfil-host finding, modelling a completed pr-scan check.
func seedScannedPR(t *testing.T, globals *Globals, patchURI string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.OpenSQLite(ctx, globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	ent, _, err := s.EnsureEntityByCanonicalURI(ctx, patchURI, "hello#1")
	require.NoError(t, err)

	now := time.Now().UTC()
	mk := func(sigType string, value any) profile.Signal {
		raw, merr := json.Marshal(value)
		require.NoError(t, merr)
		return profile.Signal{
			ID:                profile.NewEntityID(),
			EntityID:          ent.ID,
			Type:              sigType,
			Group:             profile.SignalGroupHygiene,
			Source:            prScanSource,
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             raw,
			CollectedAt:       now,
			ExpiresAt:         now.Add(time.Hour),
		}
	}
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		mk(verdictSignalType, verdictRecord{Verdict: "block", Reasons: []string{"1 exfil-host reference(s)"}, HeadSHA: "deadbeef"}),
		mk("pr_exfil_host_reference", map[string]any{
			"hits": []map[string]any{{"file": "telemetry/init.go", "line": 6, "host": "webhook.site"}},
		}),
	}))
}
