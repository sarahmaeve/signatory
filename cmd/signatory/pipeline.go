package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/sarahmaeve/signatory/internal/pipeline"
)

// PipelineCmd is the dispatcher for verbs that talk to signatory's
// local pipeline message service — the HTTP/SQLite transport layer
// the /analyze skill uses to hand prompts to subagents over a
// WebFetch-compatible (GET-only) surface.
//
// The pipeline service is operated by `signatory serve`; this
// command group is its client-side front door from the CLI. It
// replaces the shell-level `curl -X POST` calls the /analyze skill
// previously used, which were brittle (JSON-escaping handoff bodies
// across shell boundaries) and couldn't be executed from a subagent
// context anyway.
//
// Pipeline sessions are a different concept from analysis sessions
// (`signatory analysis begin`): pipeline is transport (short-lived,
// holds handoff messages), analysis is audit identity (durable,
// rolls up analyst outputs). The skill creates both and threads the
// pipeline session id onto the analysis row via
// `analysis begin --pipeline-session-id`. See design/tls-trust.md
// for the trust architecture these verbs participate in.
type PipelineCmd struct {
	Session         PipelineSessionCmd         `cmd:"" help:"Pipeline session operations (create, list, delete)."`
	Prepare         PipelinePrepareCmd         `cmd:"" help:"Prepare the full analysis pipeline: create sessions, render handoffs, refresh signals, return a JSON manifest."`
	DispatchPrompts PipelineDispatchPromptsCmd `cmd:"dispatch-prompts" help:"Render the agent dispatch prompts with all placeholders substituted."`
	Verify          PipelineVerifyCmd          `cmd:"" help:"Check whether all expected analysts have landed output for an analysis session."`
	Close           PipelineCloseCmd           `cmd:"" help:"Find synthesis output, accept posture, and close the analysis session."`
	Run             PipelineRunCmd             `cmd:"" help:"Drive the orchestrator state machine: emit a structured 'dispatch this prompt' event for the host LLM, exit, and resume on next invocation. Composes prepare + dispatch-prompts (start) and verify + synthesis-handoff + dispatch-prompts (--resume) so SKILL.md becomes a thin host adapter rather than the orchestrator itself."`
}

// PipelineSessionCmd groups the session-scoped verbs. v0.1 only
// needs `create`; `list` / `delete` exist as natural future
// additions but are not wired today — every other read of pipeline
// state goes through the skill, which uses WebFetch directly.
type PipelineSessionCmd struct {
	Create PipelineSessionCreateCmd `cmd:"" help:"Create a pipeline session. Prints the session ID on stdout."`
}

// PipelineSessionCreateCmd issues POST /api/sessions and emits the
// new session UUID on stdout. Matches the `signatory analysis begin`
// convention: bare ID to stdout for shell capture
// (`SESSION_ID=$(signatory pipeline session create "$TARGET")`),
// informational line to stderr unless `--quiet`.
//
// Replaces the `curl -sk -X POST .../api/sessions` call previously
// in .claude/skills/analyze/SKILL.md Step 1. The `-k` (insecure skip
// verify) disappears automatically: this verb goes through the
// shared pipeline.Client which trusts the signatory-managed CA
// anchor per design/tls-trust.md — no verification opt-out.
type PipelineSessionCreateCmd struct {
	Target string `arg:"" help:"Target URI the session is scoped to (URL, owner/repo, or canonical URI)."`

	Metadata    string `help:"Optional JSON blob to store alongside the session." default:""`
	PipelineURL string `name:"pipeline-url" help:"Override the pipeline service base URL (default: production localhost)." default:"${pipelineURL}"`
	Quiet       bool   `help:"Suppress the stderr info line." short:"q"`

	// Writer injection for tests. Production paths leave these nil
	// and Run defaults them to os.Stdout/os.Stderr. Follows the
	// AnalysisBeginCmd pattern (cmd/signatory/analysis.go:72-73).
	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
}

func (cmd *PipelineSessionCreateCmd) Run(globals *Globals) error {
	stdout, stderr := cmd.resolveWriters()

	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}

	client, err := pipeline.NewClient(cmd.PipelineURL)
	if err != nil {
		return fmt.Errorf("open pipeline client: %w", err)
	}

	sess, err := client.CreateSession(ctx, cmd.Target, cmd.Metadata)
	if err != nil {
		return fmt.Errorf("pipeline session create: %w", err)
	}

	// Bare UUID on stdout — same convention as `signatory analysis
	// begin` (cmd/signatory/analysis.go:125) so shell callers can
	// capture with $(…) substitution.
	fmt.Fprintln(stdout, sess.ID)

	if !cmd.Quiet {
		fmt.Fprintf(stderr, "# pipeline session: %s for %s\n", sess.ID, sess.Target)
	}
	return nil
}

func (cmd *PipelineSessionCreateCmd) resolveWriters() (io.Writer, io.Writer) {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cmd.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return stdout, stderr
}
