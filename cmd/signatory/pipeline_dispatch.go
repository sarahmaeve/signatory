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

	roles := make([]string, 0, len(dispatchRoles))
	for role := range dispatchRoles {
		roles = append(roles, role)
	}

	prompts, err := renderDispatchPromptsFor(roles, cmd.TargetName, subs, templateFS)
	if err != nil {
		return err
	}
	return writeJSON(stdout, &DispatchPromptsResult{Prompts: prompts})
}

// renderDispatchPromptsFor loads the dispatch templates for the
// requested roles, substitutes placeholders, and returns the
// rendered prompt map. Callers that compose this stage (the
// orchestrator command in pipeline_run.go) request only the
// roles they need at the current pipeline phase: security +
// provenance during start, synthesist during resume.
//
// templateFS defaults to signatory.EmbeddedTemplates at the call
// site; this helper takes whatever the caller passed so tests can
// inject fstest.MapFS.
//
// Returns a clear "unknown role" error if a caller asks for a
// role that isn't in dispatchRoles — guards against typos in the
// roles slice surfacing as a confusing template-not-found error.
func renderDispatchPromptsFor(
	roles []string,
	targetName string,
	subs map[string]string,
	templateFS fs.FS,
) (map[string]DispatchPrompt, error) {
	out := make(map[string]DispatchPrompt, len(roles))
	for _, role := range roles {
		dr, ok := dispatchRoles[role]
		if !ok {
			return nil, fmt.Errorf("unknown dispatch role %q", role)
		}
		raw, err := fs.ReadFile(templateFS, dr.templatePath)
		if err != nil {
			return nil, fmt.Errorf("load dispatch template %s: %w", dr.templatePath, err)
		}
		rendered, _ := config.RenderTemplate(raw, subs)
		body := strings.TrimSpace(string(rendered))
		out[role] = DispatchPrompt{
			Description:  descriptionForRole(role, targetName),
			Prompt:       body,
			AllowedTools: dr.allowedTools,
		}
	}
	return out, nil
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
