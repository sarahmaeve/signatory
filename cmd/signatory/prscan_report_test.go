package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rhtml "github.com/sarahmaeve/pr-analyzer/render/html"

	"github.com/sarahmaeve/signatory/internal/profile"
	ghclient "github.com/sarahmaeve/signatory/internal/signal/github"
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

// TestDeepScan_sections_riskyPath: an org-defined sensitive-path touch
// surfaces a SENSITIVE PATH pill + the paths row.
func TestDeepScan_sections_riskyPath(t *testing.T) {
	t.Parallel()
	secs := deepScan{Verdict: "warn", RiskyPaths: []string{"internal/secret/keys.go"}}.sections()
	require.Len(t, secs, 1)
	assert.Contains(t, secs[0].Pills, rhtml.Pill{Text: "SENSITIVE PATH", Tier: "warning"})
	assert.Contains(t, secs[0].Rows, rhtml.Row{Term: "Sensitive paths", Detail: "internal/secret/keys.go"})
}

// TestDeepScan_sections_anomalousLang: a non-acceptable language surfaces
// an ANOMALOUS LANG pill + the languages row.
func TestDeepScan_sections_anomalousLang(t *testing.T) {
	t.Parallel()
	secs := deepScan{Verdict: "warn", AnomalousLangs: []string{"Rust"}}.sections()
	require.Len(t, secs, 1)
	assert.Contains(t, secs[0].Pills, rhtml.Pill{Text: "ANOMALOUS LANG", Tier: "warning"})
	assert.Contains(t, secs[0].Rows, rhtml.Row{Term: "Anomalous languages", Detail: "Rust"})
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

// TestFunctional_PRScanCheck_RiskyPathFromConfig drives the org-policy
// path end to end: --config names a pr-analyzer.yaml declaring
// internal/secret risky; a PR touching internal/secret/keys.go (benign
// content) must warn and store a pr_risky_path_touched finding.
func TestFunctional_PRScanCheck_RiskyPathFromConfig(t *testing.T) {
	t.Parallel()
	globals := testGlobals(t)

	cfgPath := filepath.Join(t.TempDir(), "pr-analyzer.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("codeshape:\n  risky_paths:\n    - internal/secret\n"), 0o600))

	src := fakeContentProvider{content: map[string][]byte{
		"internal/secret/keys.go": []byte("package secret\n\nvar Rotation = 1\n"),
	}}
	srv := prScanGitHubServerAs(t, "headRP", "octocat", "User", prFilesJSON("internal/secret/keys.go"))
	cmd := &PRScanCheckCmd{
		Target:      "octo/hello#1",
		JSON:        true,
		Config:      cfgPath,
		Client:      ghclient.NewClientWithBaseURL(srv.URL),
		NewProvider: countingProvider(src, new(int)),
		Stdout:      &bytes.Buffer{},
		Stderr:      io.Discard,
	}
	require.NoError(t, cmd.Run(globals), "a risky-path touch warns (exit 0), not blocks")

	ctx := context.Background()
	s, err := store.OpenSQLite(ctx, globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	patch, err := s.FindEntityByURI(ctx, "patch:github/octo/hello/1")
	require.NoError(t, err)
	sigs, err := s.GetSignals(ctx, patch.ID)
	require.NoError(t, err)
	var risky, verdict string
	for _, sg := range sigs {
		switch sg.Type {
		case "pr_risky_path_touched":
			risky = string(sg.Value)
		case "pr_defense_verdict":
			verdict = string(sg.Value)
		}
	}
	require.NotEmpty(t, risky, "a PR touching a configured risky path must store pr_risky_path_touched")
	assert.Contains(t, risky, "internal/secret/keys.go")
	assert.Contains(t, verdict, `"verdict":"warn"`)
}

// TestFunctional_PRScanCheck_AnomalousLanguageFromConfig: --config names a
// pr-analyzer.yaml that prefers Go; a PR adding a Rust file (benign) is
// warned and stores pr_anomalous_language, while a markup-only change would
// not trip it.
func TestFunctional_PRScanCheck_AnomalousLanguageFromConfig(t *testing.T) {
	t.Parallel()
	globals := testGlobals(t)

	cfgPath := filepath.Join(t.TempDir(), "pr-analyzer.yaml")
	require.NoError(t, os.WriteFile(cfgPath,
		[]byte("codeshape:\n  languages:\n    preferred:\n      - Go\n"), 0o600))

	src := fakeContentProvider{content: map[string][]byte{
		"x/lib.rs": []byte("pub fn f() {}\n"),
	}}
	srv := prScanGitHubServerAs(t, "headAL", "octocat", "User", prFilesJSON("x/lib.rs"))
	cmd := &PRScanCheckCmd{
		Target:      "octo/hello#1",
		JSON:        true,
		Config:      cfgPath,
		Client:      ghclient.NewClientWithBaseURL(srv.URL),
		NewProvider: countingProvider(src, new(int)),
		Stdout:      &bytes.Buffer{},
		Stderr:      io.Discard,
	}
	require.NoError(t, cmd.Run(globals), "an anomalous language warns (exit 0), not blocks")

	ctx := context.Background()
	s, err := store.OpenSQLite(ctx, globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	patch, err := s.FindEntityByURI(ctx, "patch:github/octo/hello/1")
	require.NoError(t, err)
	sigs, err := s.GetSignals(ctx, patch.ID)
	require.NoError(t, err)
	var lang, verdict string
	for _, sg := range sigs {
		switch sg.Type {
		case "pr_anomalous_language":
			lang = string(sg.Value)
		case "pr_defense_verdict":
			verdict = string(sg.Value)
		}
	}
	require.NotEmpty(t, lang, "a PR introducing a non-preferred language must store pr_anomalous_language")
	assert.Contains(t, lang, "Rust")
	assert.Contains(t, verdict, `"verdict":"warn"`)
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
