package main

import (
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	signatory "github.com/sarahmaeve/signatory"
	"github.com/sarahmaeve/signatory/internal/config"
	"github.com/sarahmaeve/signatory/internal/exchange"
)

// DispatchPrompt is a single rendered agent dispatch prompt.
//
// AnalystID is the canonical signatory-<role>-v<N> string the
// orchestrator will look for at verify time. Surfaced explicitly
// (in addition to being inlined into Prompt) so the host adapter
// can assert / log it without parsing the rendered prompt body,
// and so dogfood telemetry can compare orchestrator-expected vs
// actually-ingested values.
type DispatchPrompt struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	AllowedTools string `json:"allowed_tools"`
	AnalystID    string `json:"analyst_id"`
}

// DispatchPromptsResult is the JSON contract returned by
// `signatory pipeline dispatch-prompts`. Contains the rendered
// dispatch prompts for each analyst role, ready for Agent() calls.
type DispatchPromptsResult struct {
	Prompts map[string]DispatchPrompt `json:"prompts"`
}

// dispatchRole maps a role name to its template file, allowed-tools
// string, and canonical analyst_id. The template files live under
// templates/dispatch/ and are shipped inside the binary via
// //go:embed all:templates (embedded.go).
//
// analystID is the exact signatory-<role>-v<N> string the agent
// must use in attribution.analyst_id and the orchestrator will
// look for at verify time. Inlined into the dispatch prompt via
// the {ANALYST_ID} substitution so the agent reads the right value
// from the dispatch body directly instead of having to faithfully
// copy it from the (long) handoff body — the asciify-image
// (e572ed87) drift fix.
type dispatchRole struct {
	templatePath string
	allowedTools string
	analystID    string
}

var dispatchRoles = map[string]dispatchRole{
	"security": {
		templatePath: "templates/dispatch/security-dispatch-v1.md",
		allowedTools: "Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis",
		analystID:    "signatory-security-v1",
	},
	"provenance": {
		templatePath: "templates/dispatch/provenance-dispatch-v1.md",
		allowedTools: "Read Glob Grep WebFetch mcp__signatory__signatory_signals mcp__signatory__signatory_summary mcp__signatory__signatory_detail mcp__signatory__signatory_ingest_analysis",
		analystID:    "signatory-provenance-v1",
	},
	"synthesist": {
		templatePath: "templates/dispatch/synthesist-dispatch-v1.md",
		allowedTools: "Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis",
		analystID:    "signatory-synthesis-v1",
	},
}

// collectionRoles returns the dispatch role keys for the collection
// phase (start) — every role in dispatchRoles whose analystID is
// NOT a synthesist. The synthesist dispatches during the resume
// phase after the collection analysts have landed.
//
// Derived from the dispatchRoles map at call time so a new
// non-synthesist role added to dispatchRoles is automatically
// dispatched by runStart without a second hardcoded list to update.
// Sorted lexicographically for deterministic iteration order.
func collectionRoles() []string {
	var roles []string
	for role, dr := range dispatchRoles {
		if exchange.IsSynthesistRole(dr.analystID) {
			continue
		}
		roles = append(roles, role)
	}
	slices.Sort(roles)
	return roles
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

		// Per-role substitutions overlay the caller-supplied subs map
		// without mutating it (the same map may be reused across
		// multiple role renders). ANALYST_ID is per-role and comes
		// from dispatchRoles, not from the caller — the orchestrator
		// is the source of truth for what analyst_id each role is
		// expected to ingest under, so it must not be caller-supplied.
		roleSubs := make(map[string]string, len(subs)+1)
		maps.Copy(roleSubs, subs)
		roleSubs["ANALYST_ID"] = dr.analystID

		rendered, _ := config.RenderTemplate(raw, roleSubs)
		body := strings.TrimSpace(string(rendered))
		out[role] = DispatchPrompt{
			Description:  descriptionForRole(role, targetName),
			Prompt:       body,
			AllowedTools: dr.allowedTools,
			AnalystID:    dr.analystID,
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
