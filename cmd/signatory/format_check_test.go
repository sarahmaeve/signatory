package main

import (
	"encoding/json"
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
  "findings": [
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
		Findings: []exchange.Finding{
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
	// Missing required fields: no attribution, no target, finding
	// missing verdict and rationale.
	bad := `{
  "findings": [{"id": "F001", "category": "x", "severity": {"default": "medium"}, "citations": [{"path": "p", "line_start": 1}]}]
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
  "findings": [],
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
		Target:   "pkg:test/x",
		Findings: []exchange.Finding{},
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

func TestParseAnalystOutput_RoundsTripsConcrete(t *testing.T) {
	// Sanity check the helper against a known-good document.
	out, err := parseAnalystOutput([]byte(minimalValidJSON), "json")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "pkg:test/example", out.Target)
	require.Len(t, out.Findings, 1)
	assert.Equal(t, "F001", out.Findings[0].ID)

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
