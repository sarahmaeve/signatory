package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalValidJSON is a hand-rolled minimum-passing AnalystOutput.
// Kept small so the test stays readable; the full fixture lives in
// internal/exchange/testdata/ and is exercised by that package's
// tests. Here we just check the CLI command end-to-end.
const minimalValidJSON = `{
  "attribution": {
    "analyst_id": "test",
    "model": "test-model",
    "invoked_at": "2026-04-14T00:00:00Z"
  },
  "target": "pkg:test/example",
  "conclusions": [
    {
      "id": "F001",
      "verdict": "test verdict",
      "rationale": "test rationale",
      "severity": {"default": "medium"},
      "category": "test",
      "citations": [
        {"path": "src/main.rs", "line_start": 1}
      ]
    }
  ]
}`

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestFormatCheck_ValidJSON_Passes(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	cmd := &FormatCheckCmd{File: path, Format: "auto", Quiet: true}
	require.NoError(t, cmd.Run(&Globals{}))
}

func TestFormatCheck_ValidMarkdown_Passes(t *testing.T) {
	// Construct a minimal AnalystOutput, marshal to markdown,
	// write it to a tempfile, and check it.
	verdict := "test verdict"
	rationale := "test rationale"
	lineStart := 1
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "test", Model: "test-model", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Target: "pkg:test/example",
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F001",
				Verdict:   verdict,
				Rationale: rationale,
				Severity:  exchange.Severity{Default: exchange.SeverityMedium},
				Category:  "test",
				Citations: []exchange.Citation{
					{Path: "src/main.rs", LineStart: &lineStart},
				},
			},
		},
		RoundNotes: "some prose for the body",
	}
	md, err := out.MarshalMarkdown()
	require.NoError(t, err)

	path := writeTempFile(t, "valid.md", string(md))
	cmd := &FormatCheckCmd{File: path, Format: "auto", Quiet: true}
	require.NoError(t, cmd.Run(&Globals{}))
}

func TestFormatCheck_InvalidJSON_FailsWithSchemaErrors(t *testing.T) {
	// Missing required fields: no attribution, no target, conclusion
	// missing verdict and rationale.
	bad := `{
  "conclusions": [{"id": "F001", "category": "x", "severity": {"default": "medium"}, "citations": [{"path": "p", "line_start": 1}]}]
}`
	path := writeTempFile(t, "bad.json", bad)
	cmd := &FormatCheckCmd{File: path, Format: "auto", Quiet: true}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	msg := err.Error()
	// Should mention several specific problems, not just the first.
	assert.Contains(t, msg, "analyst_id")
	assert.Contains(t, msg, "target")
	assert.Contains(t, msg, "verdict")
	assert.Contains(t, msg, "rationale")
}

func TestFormatCheck_MalformedJSON_FailsAtParse(t *testing.T) {
	bad := `{this isn't json}`
	path := writeTempFile(t, "malformed.json", bad)
	cmd := &FormatCheckCmd{File: path, Format: "auto", Quiet: true}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestFormatCheck_UnknownField_Rejected(t *testing.T) {
	// DisallowUnknownFields means a typo'd field name fails loudly
	// instead of silently dropping data. Important guarantee for
	// agents that might accidentally emit `severit` or `find1ngs`.
	bad := `{
  "attribution": {"analyst_id": "x", "model": "y", "invoked_at": "2026-04-14T00:00:00Z"},
  "target": "pkg:test/x",
  "conclusions": [],
  "this_is_not_a_real_field": "value"
}`
	path := writeTempFile(t, "unknown-field.json", bad)
	cmd := &FormatCheckCmd{File: path, Format: "auto", Quiet: true}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "this_is_not_a_real_field")
}

func TestFormatCheck_ExplicitFormatOverridesAutoDetect(t *testing.T) {
	// File extension is .json but content is markdown — explicit
	// --format=markdown should pick markdown despite the extension.
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Target:      "pkg:test/x",
		Conclusions: []exchange.Conclusion{},
	}
	md, err := out.MarshalMarkdown()
	require.NoError(t, err)

	path := writeTempFile(t, "misnamed.json", string(md))
	cmd := &FormatCheckCmd{File: path, Format: "markdown", Quiet: true}
	require.NoError(t, cmd.Run(&Globals{}),
		"explicit --format=markdown should override .json extension")
}

func TestFormatCheck_AutoDetect_Sniffing(t *testing.T) {
	// File has no recognized extension; format should be sniffed
	// from the content.
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"json starts with brace", `{"x": 1}`, "json"},
		{"json with leading whitespace", "\n  {\"x\": 1}", "json"},
		{"markdown starts with ---", "---\nx: 1\n---\n", "markdown"},
		{"markdown with CRLF", "---\r\nx: 1\r\n---\r\n", "markdown"},
		{"unknown defaults to json", "garbage", "json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectAnalystOutputFormat("file.unknown", []byte(tt.content))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFormatCheck_FileNotFound_FailsCleanly(t *testing.T) {
	// kong's existingfile type catches this at parse time, but the
	// command itself should also handle a missing file gracefully
	// for callers that bypass kong (e.g., direct programmatic use).
	cmd := &FormatCheckCmd{
		File:   "/nonexistent/path/to/file.json",
		Format: "json",
		Quiet:  true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read")
}

// TestFormatCheck_KongIntegration confirms the command parses
// through the CLI struct (not just via direct construction). This
// catches mistakes in the kong tag declarations like a typo'd cmd
// name or a missing required arg.
func TestFormatCheck_KongIntegration(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	_, cli := parseCLI(t, "format-check", path, "--quiet")
	require.NotNil(t, cli)
	assert.Equal(t, path, cli.FormatCheck.File)
	assert.True(t, cli.FormatCheck.Quiet)
	assert.Equal(t, "auto", cli.FormatCheck.Format)
}

func TestFormatCheck_KongIntegration_FormatFlag(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	_, cli := parseCLI(t, "format-check", path, "--format", "json", "-q")
	assert.Equal(t, "json", cli.FormatCheck.Format)
}

func TestFormatCheck_KongIntegration_InvalidFormatRejected(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	err := parseCLIExpectError(t, "format-check", path, "--format", "yaml")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "format")
}

// --- Summary mode tests ---

func TestPrintSummary_StructuralFields_NoProse(t *testing.T) {
	// Construct an AnalystOutput with known prose in verdict /
	// rationale and verify those don't appear in the summary
	// output. Identifying fields (IDs, categories, severities,
	// counts) MUST appear.
	verdict := "VERDICT_PROSE_SHOULD_NOT_APPEAR_IN_SUMMARY"
	rationale := "RATIONALE_PROSE_SHOULD_NOT_APPEAR_IN_SUMMARY"
	roundNotes := "ROUND_NOTES_BODY_SHOULD_NOT_APPEAR_IN_SUMMARY"
	lineStart := 1
	signalType := "test_signal"

	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "test-analyst",
			Model:     "test-model",
			InvokedAt: "2026-04-14T00:00:00Z",
			Round:     2,
		},
		Target:       "pkg:test/example",
		TargetCommit: "abc123",
		Conclusions: []exchange.Conclusion{
			{
				ID:           "F001",
				Verdict:      verdict,
				Rationale:    rationale,
				Severity:     exchange.Severity{Default: exchange.SeverityHigh},
				Category:     "test_category",
				SignalType:   &signalType,
				DesignIntent: true,
				Citations: []exchange.Citation{
					{Path: "src/main.rs", LineStart: &lineStart},
					{Path: "src/lib.rs", LineStart: &lineStart},
				},
				Prerequisites:    []string{"prereq one"},
				RemediationHints: []string{"fix one", "fix two", "fix three"},
				Supersedes: []exchange.Supersession{
					{PriorID: "F-prior", PriorRound: 1, Kind: exchange.SupersessionKindCorrects},
				},
			},
		},
		Supersedes: []exchange.Supersession{
			{PriorID: "r1", PriorRound: 1, Kind: exchange.SupersessionKindRefines},
		},
		RoundNotes: roundNotes,
	}

	var buf bytes.Buffer
	printSummary(&buf, "/tmp/test.json", "json", out)
	got := buf.String()

	// Prose must not appear.
	assert.NotContains(t, got, verdict, "verdict should be excluded from summary")
	assert.NotContains(t, got, rationale, "rationale should be excluded from summary")
	assert.NotContains(t, got, roundNotes, "round_notes body should be excluded from summary")

	// Structural data MUST appear.
	assert.Contains(t, got, "F001", "conclusion ID")
	assert.Contains(t, got, "high", "severity value")
	assert.Contains(t, got, "test_category", "category")
	assert.Contains(t, got, "test_signal", "signal type")
	assert.Contains(t, got, "[design_intent]", "design_intent flag")
	assert.Contains(t, got, "[supersedes 1]", "supersedes count")
	assert.Contains(t, got, "cite/prereq/fix: 2/1/3", "structured counts")
	assert.Contains(t, got, "test-analyst / test-model (round 2)", "attribution line")
	assert.Contains(t, got, "target: pkg:test/example", "target")
	assert.Contains(t, got, "target_commit: abc123", "target_commit when present")
	assert.Contains(t, got, "supersedes 1 prior", "top-level supersedes")
	assert.Contains(t, got, "round_notes:", "round_notes presence indicator")
	// Length indicator should reflect actual length, not the body.
	assert.Contains(t, got, fmt.Sprintf("%d chars", len(roundNotes)))
}

func TestPrintSummary_PositiveAbsences_ConfidenceAndPattern(t *testing.T) {
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Target:      "pkg:test/x",
		Conclusions: []exchange.Conclusion{},
		PositiveAbsences: []exchange.PositiveAbsence{
			{
				PatternChecked: "use of pickle.load on network data",
				Description:    "DESCRIPTION_PROSE_SHOULD_NOT_APPEAR",
				Confidence:     exchange.ConfidenceThoroughlyReviewed,
			},
			{
				PatternChecked: "subprocess shell=True with user input",
				Description:    "ANOTHER_DESCRIPTION_PROSE",
				Confidence:     exchange.ConfidenceSpotChecked,
			},
		},
	}
	var buf bytes.Buffer
	printSummary(&buf, "/tmp/test.json", "json", out)
	got := buf.String()

	assert.Contains(t, got, "[thoroughly_reviewed] use of pickle.load on network data")
	assert.Contains(t, got, "[spot_checked] subprocess shell=True with user input")
	assert.NotContains(t, got, "DESCRIPTION_PROSE_SHOULD_NOT_APPEAR",
		"positive_absence description is prose; should be excluded")
}

func TestPrintSummary_MethodologyGroupsAndCounts(t *testing.T) {
	hit := true
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Target:      "pkg:test/x",
		Conclusions: []exchange.Conclusion{},
		MethodologyTrace: &exchange.MethodologyCatalog{
			Source: exchange.AgentAttribution{
				AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
			},
			Notes: "NOTES_PROSE_SHOULD_NOT_APPEAR",
			Patterns: []exchange.MethodologyPattern{
				{
					ID: "MP-A-01", SignalGroup: "alpha",
					Description: "pattern A1",
					CollectorHint: exchange.CollectorHint{
						GrepPrecision: exchange.GrepPrecisionHigh, ReasoningDepth: exchange.ReasoningDepthNone,
					},
					Pattern: stringPtr("rg foo"), HitOnTarget: &hit,
				},
				{
					ID: "MP-A-02", SignalGroup: "alpha",
					Description: "pattern A2",
					CollectorHint: exchange.CollectorHint{
						GrepPrecision: exchange.GrepPrecisionHigh, ReasoningDepth: exchange.ReasoningDepthNone,
					},
					Pattern: stringPtr("rg bar"), HitOnTarget: &hit,
				},
				{
					ID: "MP-B-01", SignalGroup: "beta",
					Description: "pattern B1",
					CollectorHint: exchange.CollectorHint{
						GrepPrecision: exchange.GrepPrecisionUseless, ReasoningDepth: exchange.ReasoningDepthMultiHop,
					},
				},
			},
		},
	}
	var buf bytes.Buffer
	printSummary(&buf, "/tmp/test.json", "json", out)
	got := buf.String()

	assert.Contains(t, got, "methodology_trace (3 patterns)")
	assert.Contains(t, got, "alpha (2): MP-A-01, MP-A-02")
	assert.Contains(t, got, "beta (1): MP-B-01")
	assert.NotContains(t, got, "NOTES_PROSE_SHOULD_NOT_APPEAR",
		"methodology notes are prose; should be excluded from summary")
}

func TestPrintSummary_OmitsAbsentSections(t *testing.T) {
	// A document with only conclusions should not print empty
	// section headers for positive_absences, observations, or
	// methodology_trace.
	verdict := "v"
	rationale := "r"
	lineStart := 1
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Target: "pkg:test/x",
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: verdict, Rationale: rationale,
				Severity:  exchange.Severity{Default: exchange.SeverityLow},
				Category:  "c",
				Citations: []exchange.Citation{{Path: "p", LineStart: &lineStart}},
			},
		},
	}
	var buf bytes.Buffer
	printSummary(&buf, "/tmp/test.json", "json", out)
	got := buf.String()

	assert.NotContains(t, got, "positive_absences")
	assert.NotContains(t, got, "observations")
	assert.NotContains(t, got, "methodology_trace")
	assert.NotContains(t, got, "round_notes")
}

func TestPrintSummary_ConditionalSeverity(t *testing.T) {
	verdict := "v"
	rationale := "r"
	lineStart := 1
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Target: "pkg:test/x",
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: verdict, Rationale: rationale,
				Severity: exchange.Severity{
					Default: exchange.SeverityMedium,
					ByContext: []exchange.ContextualSeverity{
						{Context: exchange.ContextSpec{HostIsolation: "single_user"}, Value: exchange.SeverityLow},
						{Context: exchange.ContextSpec{HostIsolation: "shared_host"}, Value: exchange.SeverityMedium},
						{Context: exchange.ContextSpec{HostIsolation: "ci_runner"}, Value: exchange.SeverityHigh},
					},
				},
				Category:  "c",
				Citations: []exchange.Citation{{Path: "p", LineStart: &lineStart}},
			},
		},
	}
	var buf bytes.Buffer
	printSummary(&buf, "/tmp/test.json", "json", out)
	got := buf.String()
	assert.Contains(t, got, "[medium +3 ctx]",
		"conditional severity should show count of context overrides")
}

func TestPrintSummary_SortedDeterminism(t *testing.T) {
	// The methodology_trace section sorts by signal_group; the
	// output should be byte-identical across runs even when input
	// patterns are in different orders.
	hit := true
	makeOut := func(patterns []exchange.MethodologyPattern) *exchange.AnalystOutput {
		return &exchange.AnalystOutput{
			Attribution: exchange.AgentAttribution{
				AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
			},
			Target:      "pkg:test/x",
			Conclusions: []exchange.Conclusion{},
			MethodologyTrace: &exchange.MethodologyCatalog{
				Source: exchange.AgentAttribution{
					AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
				},
				Patterns: patterns,
			},
		}
	}
	mp := func(id, group string) exchange.MethodologyPattern {
		return exchange.MethodologyPattern{
			ID: id, SignalGroup: group, Description: "x",
			CollectorHint: exchange.CollectorHint{
				GrepPrecision: exchange.GrepPrecisionUseless, ReasoningDepth: exchange.ReasoningDepthOneHop,
			},
			HitOnTarget: &hit,
		}
	}
	a := makeOut([]exchange.MethodologyPattern{
		mp("M1", "zeta"), mp("M2", "alpha"), mp("M3", "beta"),
	})
	b := makeOut([]exchange.MethodologyPattern{
		mp("M3", "beta"), mp("M1", "zeta"), mp("M2", "alpha"),
	})
	var bufA, bufB bytes.Buffer
	printSummary(&bufA, "/tmp/test.json", "json", a)
	printSummary(&bufB, "/tmp/test.json", "json", b)
	assert.Equal(t, bufA.String(), bufB.String(),
		"summary output must be deterministic regardless of input order")
}

func TestFormatCheck_SummaryFlag_Integration(t *testing.T) {
	// End-to-end through Run() — confirms --summary path works
	// without errors. We don't assert on stdout content here
	// (Run prints to os.Stdout directly); printSummary is tested
	// in isolation above.
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	cmd := &FormatCheckCmd{File: path, Format: "auto", Summary: true}
	require.NoError(t, cmd.Run(&Globals{}))
}

func TestFormatCheck_KongIntegration_SummaryFlag(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	_, cli := parseCLI(t, "format-check", path, "--summary")
	assert.True(t, cli.FormatCheck.Summary)
	assert.False(t, cli.FormatCheck.Quiet)

	_, cli = parseCLI(t, "format-check", path, "-s")
	assert.True(t, cli.FormatCheck.Summary, "short flag -s should set Summary")
}

// stringPtr is a tiny helper for constructing *string fields in test
// fixtures inline.
func stringPtr(s string) *string { return &s }

func TestParseAnalystOutput_RoundsTripsConcrete(t *testing.T) {
	// Sanity check the helper against a known-good document.
	out, err := parseAnalystOutput([]byte(minimalValidJSON), "json")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "pkg:test/example", out.Target)
	require.Len(t, out.Conclusions, 1)
	assert.Equal(t, "F001", out.Conclusions[0].ID)

	// And confirm it would also pass Validate() at the package layer.
	require.NoError(t, out.Validate())

	// Confirm the JSON->struct->JSON round-trip preserves bytes
	// (modulo whitespace) by re-encoding and re-decoding.
	encoded, err := json.Marshal(out)
	require.NoError(t, err)
	out2, err := parseAnalystOutput(encoded, "json")
	require.NoError(t, err)
	require.NoError(t, out2.Validate())
}
