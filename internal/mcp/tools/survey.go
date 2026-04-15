package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/sarahmaeve/signatory/internal/mcp"
)

// SurveyTool implements signatory_survey.
//
// v0.1 limitation: auto-detection of the manifest from cwd is deferred to
// v0.2. In v0.1 manifest_path is required and must be supplied by the caller.
// Even when a manifest_path is provided, dependency parsing for most ecosystems
// is not yet wired up — the tool returns a clear CodeNotFound error explaining
// what is not yet implemented rather than silently returning an empty result.
//
// The honest "not yet implemented" signaling is intentional per the design
// philosophy: we never pretend to auto-detect when we cannot.
type SurveyTool struct{}

func (t *SurveyTool) Name() string { return "signatory_survey" }

func (t *SurveyTool) Description() string {
	return "Return a dependency-tree posture overview for a project manifest."
}

func (t *SurveyTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"manifest_path": {
				"type": "string",
				"description": "Path to the project manifest file (go.mod, Cargo.toml, package.json, pyproject.toml). Required in v0.1."
			},
			"refresh": {
				"type": "boolean",
				"default": false,
				"description": "Re-collect signals for discovered dependencies."
			}
		},
		"additionalProperties": false
	}`)
}

// surveyInput is the typed input for signatory_survey.
type surveyInput struct {
	ManifestPath string `json:"manifest_path,omitempty"`
	Refresh      bool   `json:"refresh,omitempty"`
}

// Compile-time interface check.
var _ mcp.Tool = (*SurveyTool)(nil)

func (t *SurveyTool) Handle(_ context.Context, raw json.RawMessage) *mcp.Response {
	var in surveyInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}

	// v0.1: manifest_path is required — auto-detect from cwd is v0.2.
	if in.ManifestPath == "" {
		return mcp.Err(mcp.CodeSchemaViolation,
			"manifest_path is required in v0.1 (auto-detect from cwd is v0.2)",
			map[string]string{"field": "manifest_path"})
	}

	// Detect ecosystem from the manifest filename.
	ecosystem := detectEcosystemFromPath(in.ManifestPath)

	// v0.1: dependency parsing is not wired up for any ecosystem yet.
	// Signal this honestly rather than pretending to parse and returning
	// empty results.
	return mcp.Err(mcp.CodeNotFound,
		"dependency parsing for "+ecosystem+" is not yet implemented (v0.2)",
		map[string]string{
			"manifest_path": in.ManifestPath,
			"ecosystem":     ecosystem,
			"note":          "Use signatory_analyze per-dependency as a workaround until v0.2 survey support lands.",
		})
}

// detectEcosystemFromPath returns a human-friendly ecosystem label based on
// the manifest filename. Used only in error messages — not a production
// dispatch path. The mapping mirrors internal/ecosystem/detect.go's
// manifestSignals table.
func detectEcosystemFromPath(path string) string {
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "go.mod":
		return "go"
	case "cargo.toml":
		return "crates (Rust)"
	case "package.json":
		return "npm"
	case "pyproject.toml", "setup.py":
		return "pypi (Python)"
	case "signatory.config.toml":
		return "signatory"
	default:
		return "unknown ecosystem (unrecognised manifest: " + base + ")"
	}
}
