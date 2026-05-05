package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	signatory "github.com/sarahmaeve/signatory"
)

// TestPipelineDispatchPrompts_AllRolesRendered verifies that
// dispatch-prompts returns a prompt for each of the three analyst
// roles, with all placeholders substituted.
func TestPipelineDispatchPrompts_AllRolesRendered(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	cmd := &PipelineDispatchPromptsCmd{
		SessionID:         "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		AnalysisSessionID: "11111111-2222-3333-4444-555555555555",
		Target:            "https://github.com/JedWatson/classnames",
		TargetName:        "classnames",
		ClonePath:         "filestore/clones/classnames",
		TemplateFS:        signatory.EmbeddedTemplates,
		Stdout:            &stdout,
	}
	require.NoError(t, cmd.Run(nil))

	var result DispatchPromptsResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	// All three roles present.
	assert.Contains(t, result.Prompts, "security")
	assert.Contains(t, result.Prompts, "provenance")
	assert.Contains(t, result.Prompts, "synthesist")

	// Each prompt has the required fields.
	for role, prompt := range result.Prompts {
		assert.NotEmpty(t, prompt.Description,
			"%s: description must not be empty", role)
		assert.NotEmpty(t, prompt.Prompt,
			"%s: prompt must not be empty", role)
		assert.NotEmpty(t, prompt.AllowedTools,
			"%s: allowed_tools must not be empty", role)
	}
}

// TestPipelineDispatchPrompts_PlaceholdersSubstituted verifies that
// every {PLACEHOLDER} in the dispatch templates is replaced with
// the corresponding value from the command inputs.
func TestPipelineDispatchPrompts_PlaceholdersSubstituted(t *testing.T) {
	t.Parallel()

	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	analysisSID := "11111111-2222-3333-4444-555555555555"
	target := "https://github.com/JedWatson/classnames"
	targetName := "classnames"
	clonePath := "filestore/clones/classnames"

	var stdout bytes.Buffer
	cmd := &PipelineDispatchPromptsCmd{
		SessionID:         sessionID,
		AnalysisSessionID: analysisSID,
		Target:            target,
		TargetName:        targetName,
		ClonePath:         clonePath,
		TemplateFS:        signatory.EmbeddedTemplates,
		Stdout:            &stdout,
	}
	require.NoError(t, cmd.Run(nil))

	var result DispatchPromptsResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	for role, prompt := range result.Prompts {
		body := prompt.Prompt

		// No unsubstituted {PLACEHOLDER} markers.
		assert.False(t, strings.Contains(body, "{SESSION_ID}"),
			"%s: {SESSION_ID} not substituted", role)
		assert.False(t, strings.Contains(body, "{ANALYSIS_SID}"),
			"%s: {ANALYSIS_SID} not substituted", role)
		assert.False(t, strings.Contains(body, "{TARGET}"),
			"%s: {TARGET} not substituted", role)
		assert.False(t, strings.Contains(body, "{TARGET_NAME}"),
			"%s: {TARGET_NAME} not substituted", role)
		assert.False(t, strings.Contains(body, "{CLONE_PATH}"),
			"%s: {CLONE_PATH} not substituted", role)

		// Values actually present in the rendered body.
		assert.Contains(t, body, sessionID,
			"%s: rendered prompt must contain session ID", role)
	}

	// Security prompt mentions clone path.
	assert.Contains(t, result.Prompts["security"].Prompt, clonePath,
		"security prompt must reference the clone path")

	// Synthesist prompt mentions analysis session ID explicitly
	// (preventing the pipeline/analysis session confusion bug).
	assert.Contains(t, result.Prompts["synthesist"].Prompt, analysisSID,
		"synthesist prompt must contain analysis session ID")
}

// TestPipelineDispatchPrompts_SecurityHasCloneGuidance verifies
// that the security dispatch prompt instructs the agent to use the
// local clone rather than WebFetch for source inspection.
func TestPipelineDispatchPrompts_SecurityHasCloneGuidance(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	cmd := &PipelineDispatchPromptsCmd{
		SessionID:         "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		AnalysisSessionID: "11111111-2222-3333-4444-555555555555",
		Target:            "https://github.com/JedWatson/classnames",
		TargetName:        "classnames",
		ClonePath:         "filestore/clones/classnames",
		TemplateFS:        signatory.EmbeddedTemplates,
		Stdout:            &stdout,
	}
	require.NoError(t, cmd.Run(nil))

	var result DispatchPromptsResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	secPrompt := result.Prompts["security"].Prompt
	assert.Contains(t, secPrompt, "LOCAL CLONE",
		"security prompt must instruct agent to use local clone")
	assert.Contains(t, secPrompt, "filestore/clones/classnames",
		"security prompt must name the clone path")
}

// TestPipelineDispatchPrompts_SynthesistWarnsAboutSessionIDs
// verifies that the synthesist prompt carries the explicit warning
// about the two different session IDs — the direct mitigation for
// the dogfood session cef3c5ab bug.
func TestPipelineDispatchPrompts_SynthesistWarnsAboutSessionIDs(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	cmd := &PipelineDispatchPromptsCmd{
		SessionID:         "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		AnalysisSessionID: "11111111-2222-3333-4444-555555555555",
		Target:            "https://github.com/JedWatson/classnames",
		TargetName:        "classnames",
		ClonePath:         "filestore/clones/classnames",
		TemplateFS:        signatory.EmbeddedTemplates,
		Stdout:            &stdout,
	}
	require.NoError(t, cmd.Run(nil))

	var result DispatchPromptsResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	synthPrompt := result.Prompts["synthesist"].Prompt
	assert.Contains(t, synthPrompt, "two different session IDs",
		"synthesist prompt must warn about session ID confusion")
	assert.Contains(t, synthPrompt, "Pipeline session",
		"synthesist prompt must name the pipeline session concept")
	assert.Contains(t, synthPrompt, "Analysis session",
		"synthesist prompt must name the analysis session concept")
}

// TestPipelineDispatchPrompts_TemplateMissing verifies that a
// missing template produces a clear error naming the file.
func TestPipelineDispatchPrompts_TemplateMissing(t *testing.T) {
	t.Parallel()

	// An empty FS has no templates at all.
	emptyFS := fstest.MapFS{}

	var stdout bytes.Buffer
	cmd := &PipelineDispatchPromptsCmd{
		SessionID:         "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		AnalysisSessionID: "11111111-2222-3333-4444-555555555555",
		Target:            "https://github.com/JedWatson/classnames",
		TargetName:        "classnames",
		ClonePath:         "filestore/clones/classnames",
		TemplateFS:        emptyFS,
		Stdout:            &stdout,
	}

	err := cmd.Run(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dispatch template",
		"error must mention dispatch template")
	assert.Contains(t, err.Error(), "dispatch-v1.md",
		"error must name the missing template file")
}

// TestPipelineDispatchPrompts_CustomTemplate verifies that an
// injected FS overrides the embedded templates — so a user who
// edits templates/dispatch/ on disk gets their version.
func TestPipelineDispatchPrompts_CustomTemplate(t *testing.T) {
	t.Parallel()

	customFS := fstest.MapFS{
		"templates/dispatch/security-dispatch-v1.md": &fstest.MapFile{
			Data: []byte("Custom security prompt for {TARGET_NAME}"),
		},
		"templates/dispatch/provenance-dispatch-v1.md": &fstest.MapFile{
			Data: []byte("Custom provenance prompt for {TARGET}"),
		},
		"templates/dispatch/synthesist-dispatch-v1.md": &fstest.MapFile{
			Data: []byte("Custom synthesist prompt with {ANALYSIS_SID}"),
		},
	}

	var stdout bytes.Buffer
	cmd := &PipelineDispatchPromptsCmd{
		SessionID:         "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		AnalysisSessionID: "11111111-2222-3333-4444-555555555555",
		Target:            "https://github.com/JedWatson/classnames",
		TargetName:        "classnames",
		ClonePath:         "filestore/clones/classnames",
		TemplateFS:        customFS,
		Stdout:            &stdout,
	}
	require.NoError(t, cmd.Run(nil))

	var result DispatchPromptsResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "Custom security prompt for classnames",
		result.Prompts["security"].Prompt)
	assert.Equal(t, "Custom provenance prompt for https://github.com/JedWatson/classnames",
		result.Prompts["provenance"].Prompt)
	assert.Equal(t, "Custom synthesist prompt with 11111111-2222-3333-4444-555555555555",
		result.Prompts["synthesist"].Prompt)
}

// TestDispatchRoles_AllowedToolsFence enforces the independence rule
// directly against the dispatchRoles Go map — the single source of
// truth for agent tool grants after the deterministic-orchestration
// rewrite. This is the authoritative fence; the invariants package
// provides a secondary text-parsing check from outside the package.
//
// Forbidden tools: Bash, Write, and every signatory_* MCP read tool.
// Exception: provenance may use cache-read tools (signals, summary,
// detail) because they return cached Layer-1 mechanical data, not
// prior analyst judgment.
func TestDispatchRoles_AllowedToolsFence(t *testing.T) {
	t.Parallel()

	forbiddenTools := []string{
		"Bash",
		"Write",
		"mcp__signatory__signatory_analyze",
		"mcp__signatory__signatory_detail",
		"mcp__signatory__signatory_show_analyses",
		"mcp__signatory__signatory_show_conclusions",
		"mcp__signatory__signatory_show_methodology",
		"mcp__signatory__signatory_signals",
		"mcp__signatory__signatory_summary",
		"mcp__signatory__signatory_survey",
	}

	provenanceAllowedCache := []string{
		"mcp__signatory__signatory_signals",
		"mcp__signatory__signatory_summary",
		"mcp__signatory__signatory_detail",
	}

	for role, dr := range dispatchRoles {
		t.Run(role, func(t *testing.T) {
			tools := strings.Fields(dr.allowedTools)
			for _, forbidden := range forbiddenTools {
				if role == "provenance" && slices.Contains(provenanceAllowedCache, forbidden) {
					continue
				}
				assert.NotContains(t, tools, forbidden,
					"dispatch role %q: must not grant %q "+
						"(independence rule / data minimization)", role, forbidden)
			}
		})
	}
}

// TestPipelineDispatchPrompts_EmbeddedFSHasAllTemplates is a
// build-level smoke test: the go:embed directive in embedded.go
// must capture all three dispatch template files.
func TestPipelineDispatchPrompts_EmbeddedFSHasAllTemplates(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"templates/dispatch/security-dispatch-v1.md",
		"templates/dispatch/provenance-dispatch-v1.md",
		"templates/dispatch/synthesist-dispatch-v1.md",
	} {
		f, err := fs.ReadFile(signatory.EmbeddedTemplates, name)
		require.NoError(t, err, "embedded FS must contain %s", name)
		assert.NotEmpty(t, f, "%s must not be empty", name)
	}
}
