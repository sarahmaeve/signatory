package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	signatory "github.com/sarahmaeve/signatory"
	"github.com/sarahmaeve/signatory/internal/config"
)

// DispatchPrompt is a single rendered agent dispatch prompt.
type DispatchPrompt struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	AllowedTools string `json:"allowed_tools"`
}

// DispatchPromptsResult is the JSON contract returned by
// `signatory pipeline dispatch-prompts`. Contains the rendered
// dispatch prompts for each analyst role, ready for Agent() calls.
type DispatchPromptsResult struct {
	Prompts map[string]DispatchPrompt `json:"prompts"`
}

// dispatchRole maps a role name to its template file and allowed-tools
// string. The template files live under templates/dispatch/ and are
// shipped inside the binary via //go:embed all:templates (embedded.go).
type dispatchRole struct {
	templatePath string
	allowedTools string
}

var dispatchRoles = map[string]dispatchRole{
	"security": {
		templatePath: "templates/dispatch/security-dispatch-v1.md",
		allowedTools: "Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis",
	},
	"provenance": {
		templatePath: "templates/dispatch/provenance-dispatch-v1.md",
		allowedTools: "Read Glob Grep WebFetch mcp__signatory__signatory_signals mcp__signatory__signatory_summary mcp__signatory__signatory_detail mcp__signatory__signatory_ingest_analysis",
	},
	"synthesist": {
		templatePath: "templates/dispatch/synthesist-dispatch-v1.md",
		allowedTools: "Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis",
	},
}

// PipelineDispatchPromptsCmd renders the Agent() dispatch prompts
// deterministically from template files. The orchestrator reads the
// JSON output and passes each prompt to Agent() — no variable
// substitution in the LLM's head.
//
// Templates live in templates/dispatch/*.md alongside the handoff
// templates. They use the same {PLACEHOLDER} convention and are
// processed by config.RenderTemplate.
//
// See design/deterministic-orchestration.md Proposals #2 and #6.
type PipelineDispatchPromptsCmd struct {
	SessionID         string `name:"session-id" help:"Pipeline session ID." required:""`
	AnalysisSessionID string `name:"analysis-session-id" help:"Analysis session ID." required:""`
	Target            string `name:"target" help:"Original target URI." required:""`
	TargetName        string `name:"target-name" help:"Short name (basename) of the target." required:""`
	ClonePath         string `name:"clone-path" help:"Path to the cloned repo." required:""`

	// TemplateFS overrides the filesystem used to load dispatch
	// templates. nil → signatory.EmbeddedTemplates (the compiled-in
	// copy). Tests inject fstest.MapFS or an empty FS to exercise
	// error paths without touching disk.
	TemplateFS fs.FS     `kong:"-"`
	Stdout     io.Writer `kong:"-"`
}

func (cmd *PipelineDispatchPromptsCmd) Run(_ *Globals) error {
	stdout := cmd.resolveWriter()
	templateFS := cmd.TemplateFS
	if templateFS == nil {
		templateFS = signatory.EmbeddedTemplates
	}

	subs := map[string]string{
		"SESSION_ID":   cmd.SessionID,
		"ANALYSIS_SID": cmd.AnalysisSessionID,
		"TARGET":       cmd.Target,
		"TARGET_NAME":  cmd.TargetName,
		"CLONE_PATH":   cmd.ClonePath,
	}

	prompts := make(map[string]DispatchPrompt, len(dispatchRoles))
	for role, dr := range dispatchRoles {
		raw, err := fs.ReadFile(templateFS, dr.templatePath)
		if err != nil {
			return fmt.Errorf("load dispatch template %s: %w", dr.templatePath, err)
		}

		rendered, _ := config.RenderTemplate(raw, subs)
		body := strings.TrimSpace(string(rendered))

		prompts[role] = DispatchPrompt{
			Description:  descriptionForRole(role, cmd.TargetName),
			Prompt:       body,
			AllowedTools: dr.allowedTools,
		}
	}

	return writeJSON(stdout, &DispatchPromptsResult{Prompts: prompts})
}

func (cmd *PipelineDispatchPromptsCmd) resolveWriter() io.Writer {
	if cmd.Stdout != nil {
		return cmd.Stdout
	}
	return os.Stdout
}

func descriptionForRole(role, targetName string) string {
	switch role {
	case "security":
		return "Security analyst for " + targetName
	case "provenance":
		return "Provenance analyst for " + targetName
	case "synthesist":
		return "Synthesist for " + targetName
	default:
		return "Analyst for " + targetName
	}
}
