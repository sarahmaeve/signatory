package main

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"

	signatory "github.com/sarahmaeve/signatory"
	"github.com/sarahmaeve/signatory/internal/certs"
	"github.com/sarahmaeve/signatory/internal/ecosystem"
	"github.com/sarahmaeve/signatory/internal/ecosystem/resolver"
	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// RunDispatch is one analyst dispatch entry in a RunResult. It
// carries everything a host LLM needs to spawn an agent: a short
// description for telemetry, the fully-substituted prompt body,
// the allowed-tools string, and the canonical analyst_id the
// agent must ingest under. The host translates allowed_tools into
// its native runtime's tool-grant format.
//
// AnalystID is the orchestrator's expected value for
// attribution.analyst_id. It is also inlined into Prompt via the
// {ANALYST_ID} substitution. Surfaced on the JSON event so:
//   - host adapters can assert / log without parsing Prompt
//   - dogfood telemetry can compare expected vs actually-ingested
//   - re-dispatch flows can re-emphasize the value if the agent
//     drifted on the first attempt
type RunDispatch struct {
	Role         string `json:"role"`
	AnalystID    string `json:"analyst_id"`
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	AllowedTools string `json:"allowed_tools"`
}

// RunResult is the JSON contract emitted by `signatory pipeline run`.
// It is the host-agnostic interface to the orchestrator state machine:
// any LLM runtime that can read JSON, dispatch a constrained agent,
// and exec a follow-up command can host signatory's analysis pipeline.
//
// Phases:
//
//   - "analysts_dispatch_required" — start phase emitted by the bare
//     `pipeline run "$TARGET"` invocation. Dispatches contains
//     security + provenance; next_command resumes after both land.
//   - "synthesist_dispatch_required" — emitted by `pipeline run
//     --resume <sid>` after both analysts have ingested. Dispatches
//     contains the synthesist; next_command points at `pipeline close`.
//   - "missing_analysts" — emitted by `--resume` when one or more
//     expected analysts haven't ingested. Missing names them; the
//     host re-dispatches and re-runs the resume command. No
//     dispatches[] in this shape.
//
// next_command is argv form (not a shell string) so hosts can exec
// it without re-parsing or escaping, and so the contract is portable
// across platforms whose shell quoting rules differ.
type RunResult struct {
	Phase             string        `json:"phase"`
	SessionID         string        `json:"session_id,omitempty"`
	AnalysisSessionID string        `json:"analysis_session_id"`
	Target            string        `json:"target,omitempty"`
	TargetName        string        `json:"target_name,omitempty"`
	TargetURL         string        `json:"target_url,omitempty"`
	ClonePath         string        `json:"clone_path,omitempty"`
	Dispatches        []RunDispatch `json:"dispatches,omitempty"`
	Missing           []string      `json:"missing,omitempty"`
	NextCommand       []string      `json:"next_command,omitempty"`
	Instructions      string        `json:"instructions,omitempty"`
}

// PipelineRunCmd is the orchestrator state machine driver. It
// composes prepare + dispatch-prompts (start phase) and verify +
// synthesis-handoff + dispatch-prompts (resume phase) into a single
// command surface that emits structured "what to do next" events
// instead of relying on prose orchestration in a SKILL.md.
//
// The command runs to the next LLM-synchronization point, emits a
// JSON event, and exits. The host (Claude Code skill, OpenAI app,
// raw SDK loop) reads the event, performs the dispatch in its
// runtime, then invokes the next_command from the event when the
// dispatched agent(s) have landed their output.
//
// This shape lets the orchestration logic live in Go while
// preserving Invariant 1 (no LLM client in the binary): the dispatch
// itself stays in the host.
type PipelineRunCmd struct {
	Target string `arg:"" optional:"" help:"Target URI (GitHub URL, pkg URI, owner/repo). Required for the start phase; must be omitted with --resume."`

	Resume string `name:"resume" help:"Resume an existing analysis session by ID. Skips prepare and verifies analyst landing instead." default:""`

	ExpectedAnalysts []string `name:"expected-analyst" help:"Analyst role IDs the pipeline plans to dispatch (repeatable). Defaults to the standard security+provenance+synthesis triple." optional:""`

	CloneDir    string `name:"clone-dir" help:"Clone the target into this directory." default:"filestore/clones/" type:"path"`
	PipelineURL string `name:"pipeline-url" help:"Override the pipeline service base URL." default:"${pipelineURL}"`

	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`

	// --- Test injection seams (forwarded to PipelinePrepareCmd) ----
	CertsChecker      func() certs.CheckResult                                   `kong:"-"`
	RunGitClone       func(ctx context.Context, url, dest, version string) error `kong:"-"`
	PrecheckSource    ecosystem.Source                                           `kong:"-"`
	EcosystemRegistry *resolver.Registry                                         `kong:"-"`

	// TemplateFS overrides the dispatch-template filesystem. nil →
	// signatory.EmbeddedTemplates. Tests inject fstest.MapFS.
	TemplateFS fs.FS `kong:"-"`
}

// Run dispatches to the start or resume phase based on whether
// --resume was passed. The two phases are deliberately distinct
// commands rather than a single phase that decides everything,
// because they have different inputs (start needs a target;
// resume needs a session ID) and different failure modes.
func (cmd *PipelineRunCmd) Run(globals *Globals) error {
	if cmd.Resume != "" && cmd.Target != "" {
		return fmt.Errorf(
			"--resume identifies an existing session by ID; do not also pass a target. " +
				"Either re-run start with the target, or resume with --resume <analysis-session-id>")
	}
	if cmd.Resume == "" && cmd.Target == "" {
		return fmt.Errorf(
			"start phase requires a target URI (GitHub URL, pkg URI, owner/repo). " +
				"To resume an existing session, pass --resume <analysis-session-id>")
	}

	if cmd.Resume != "" {
		return cmd.runResume(globals)
	}
	return cmd.runStart(globals)
}

// runStart is the start-phase implementation: prepare + dispatch
// prompts for security + provenance. The synthesist dispatch is
// deferred to the resume phase because the synthesis handoff
// requires both analysts' conclusions to assemble its evidence
// block (see internal/synthesis).
func (cmd *PipelineRunCmd) runStart(globals *Globals) error {
	stdout, stderr := cmd.resolveWriters()

	expected := cmd.ExpectedAnalysts
	if len(expected) == 0 {
		expected = defaultExpectedAnalysts()
	}

	prep := &PipelinePrepareCmd{
		Target:            cmd.Target,
		ExpectedAnalysts:  expected,
		CloneDir:          cmd.CloneDir,
		PipelineURL:       cmd.PipelineURL,
		Stdout:            io.Discard, // Run owns stdout for the RunResult
		Stderr:            stderr,
		CertsChecker:      cmd.CertsChecker,
		RunGitClone:       cmd.RunGitClone,
		PrecheckSource:    cmd.PrecheckSource,
		EcosystemRegistry: cmd.EcosystemRegistry,
	}
	manifest, err := prep.prepare(globals)
	if err != nil {
		return err
	}

	subs := map[string]string{
		"SESSION_ID":   manifest.SessionID,
		"ANALYSIS_SID": manifest.AnalysisSessionID,
		"TARGET":       manifest.Target,
		"TARGET_NAME":  manifest.TargetName,
		"CLONE_PATH":   manifest.ClonePath,
	}

	templateFS := cmd.TemplateFS
	if templateFS == nil {
		templateFS = signatory.EmbeddedTemplates
	}
	roles := collectionRoles()
	prompts, err := renderDispatchPromptsFor(
		roles,
		manifest.TargetName, subs, templateFS,
	)
	if err != nil {
		return fmt.Errorf("render dispatch prompts: %w", err)
	}

	// collectionRoles() returns a sorted slice, so the dispatch order
	// is deterministic (provenance, security). The host iterates the
	// array in order; sorted-by-name is the simplest stable rule.
	dispatches := make([]RunDispatch, 0, len(roles))
	for _, role := range roles {
		dispatches = append(dispatches, dispatchAsRun(role, prompts[role]))
	}

	result := &RunResult{
		Phase:             "analysts_dispatch_required",
		SessionID:         manifest.SessionID,
		AnalysisSessionID: manifest.AnalysisSessionID,
		Target:            manifest.Target,
		TargetName:        manifest.TargetName,
		TargetURL:         manifest.TargetURL,
		ClonePath:         manifest.ClonePath,
		Dispatches:        dispatches,
		NextCommand: []string{
			"signatory", "pipeline", "run",
			"--resume", manifest.AnalysisSessionID,
		},
		Instructions: "Dispatch each agent in dispatches[] in parallel using your runtime's " +
			"agent primitive. Each dispatched agent calls signatory_ingest_analysis " +
			"(or your host's equivalent MCP write tool) to land its output. " +
			"After both agents have ingested, exec next_command to render the " +
			"synthesist handoff and produce its dispatch prompt.",
	}

	return writeJSON(stdout, result)
}

// runResume is the resume-phase implementation. It verifies that
// every expected analyst has landed output for the session, then
// renders + deposits the synthesis handoff and produces the
// synthesist dispatch prompt. The host LLM consumes the resulting
// JSON event, dispatches the synthesist, then runs `pipeline close`
// to retrieve the proposed posture for user confirmation.
//
// Failure mode: if any expected analyst hasn't ingested, the
// command emits a missing_analysts event (no error, exit 0) so
// the host can re-dispatch the named role and retry. This is
// distinct from the "session unknown" error path — that's a real
// failure (bad input), where missing analysts is a transient state.
func (cmd *PipelineRunCmd) runResume(globals *Globals) error {
	stdout, stderr := cmd.resolveWriters()
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit

	// Verify analyst landing first — this surfaces the
	// "unknown session" error before we waste cycles trying to load
	// the session for handoff rendering.
	verify, err := verifyAnalystLanding(ctx, s, cmd.Resume)
	if err != nil {
		return err
	}

	// Load the session for the pipeline_session_id and target URI.
	// Both already touched by verifyAnalystLanding; loading again is
	// the cost of keeping verifyAnalystLanding's signature narrow
	// (returns VerifyResult, not the underlying session row).
	sess, err := s.GetAnalysisSession(ctx, cmd.Resume)
	if err != nil {
		return fmt.Errorf("get analysis session: %w", err)
	}

	// Pre-synthesis readiness: every NON-synthesis expected analyst
	// must have landed. The synthesist itself is in expected_analysts
	// (and verify.Missing) at this point — that's correct, because
	// it lands DURING the next phase, not before. Filter it out so a
	// pending synthesist doesn't masquerade as an unlanded analyst.
	preSynthMissing := nonSynthesist(verify.Missing)
	if len(preSynthMissing) > 0 {
		fmt.Fprintf(stderr, "# missing analysts: %v\n", preSynthMissing)
		return writeJSON(stdout, &RunResult{
			Phase:             "missing_analysts",
			AnalysisSessionID: cmd.Resume,
			Missing:           preSynthMissing,
			Instructions: "The named analyst(s) did not land output for this session. " +
				"Re-dispatch them with explicit guidance to call signatory_ingest_analysis " +
				"with source, collected_from, and analysis_session_id all set, then re-run " +
				"this command (signatory pipeline run --resume <analysis-session-id>).",
		})
	}

	// All analysts landed — render and deposit the synthesis handoff.
	// HandoffCmd assembles the evidence block from the session's
	// non-synthesis analyses (internal/synthesis), so it must run
	// after the verify gate confirms both are present.
	synthHandoff := &HandoffCmd{
		Role:              "synthesist",
		Target:            sess.TargetURI,
		AnalysisSessionID: cmd.Resume,
		DepositTo:         sess.PipelineSessionID,
		PipelineURL:       cmd.PipelineURL,
		Quiet:             true,
	}
	if err := synthHandoff.Run(globals); err != nil {
		return fmt.Errorf("synthesist handoff: %w", err)
	}
	fmt.Fprintln(stderr, "# synthesist handoff: deposited")

	// Render the synthesist dispatch prompt with the substitutions
	// the template references (SESSION_ID + ANALYSIS_SID). The
	// remaining placeholders (TARGET, TARGET_NAME, CLONE_PATH) are
	// passed for completeness but the synthesist template doesn't
	// reference them.
	targetName := basenameForTarget(sess.TargetURI)
	subs := map[string]string{
		"SESSION_ID":   sess.PipelineSessionID,
		"ANALYSIS_SID": cmd.Resume,
		"TARGET":       sess.TargetURI,
		"TARGET_NAME":  targetName,
		"CLONE_PATH":   "",
	}

	templateFS := cmd.TemplateFS
	if templateFS == nil {
		templateFS = signatory.EmbeddedTemplates
	}
	prompts, err := renderDispatchPromptsFor(
		[]string{"synthesist"}, targetName, subs, templateFS,
	)
	if err != nil {
		return fmt.Errorf("render synthesist dispatch prompt: %w", err)
	}

	result := &RunResult{
		Phase:             "synthesist_dispatch_required",
		SessionID:         sess.PipelineSessionID,
		AnalysisSessionID: cmd.Resume,
		Target:            sess.TargetURI,
		TargetName:        targetName,
		Dispatches: []RunDispatch{
			dispatchAsRun("synthesist", prompts["synthesist"]),
		},
		NextCommand: []string{
			"signatory", "pipeline", "close", cmd.Resume,
		},
		Instructions: "Dispatch the synthesist agent. It calls " +
			"signatory_ingest_analysis to land its synthesis output. " +
			"After it ingests, exec next_command (without --yes) to retrieve " +
			"the proposed posture; present that to the user, and on confirmation " +
			"re-run next_command with --yes to accept the posture and close the session.",
	}
	return writeJSON(stdout, result)
}

// nonSynthesist returns the input slice minus any analyst IDs the
// exchange package classifies as a synthesist role. Used by the
// resume phase to compute "have all collection-side analysts
// landed?" — the question that gates synthesis dispatch — without
// being confused by the synthesist itself, which is correctly
// "missing" pre-synthesis.
func nonSynthesist(analystIDs []string) []string {
	var out []string
	for _, id := range analystIDs {
		if exchange.IsSynthesistRole(id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// basenameForTarget returns a short display name for a target URI.
// Falls back to the URI verbatim if profile.ResolveTarget can't
// parse it (e.g., a local path or a hand-typed shorthand).
func basenameForTarget(target string) string {
	resolved, err := profile.ResolveTarget(target)
	if err != nil || resolved.ShortName == "" {
		return target
	}
	return resolved.ShortName
}

func (cmd *PipelineRunCmd) resolveWriters() (io.Writer, io.Writer) {
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

// dispatchAsRun lifts a DispatchPrompt (the dispatch-prompts JSON
// entry shape) to a RunDispatch (the run JSON entry shape). The
// difference is just adding the role tag — RunResult flattens the
// dispatches into a list so order is meaningful, while
// DispatchPromptsResult uses a map keyed by role.
func dispatchAsRun(role string, p DispatchPrompt) RunDispatch {
	return RunDispatch{
		Role:         role,
		AnalystID:    p.AnalystID,
		Description:  p.Description,
		Prompt:       p.Prompt,
		AllowedTools: p.AllowedTools,
	}
}

// defaultExpectedAnalysts returns the analyst IDs for every role
// in dispatchRoles, sorted lexicographically. This is the set
// pipeline run records on the analysis session when the caller
// doesn't pass --expected-analyst.
//
// Derived from dispatchRoles at call time so adding a new role to
// the map automatically updates the expected set — no second
// hardcoded list to keep in sync.
func defaultExpectedAnalysts() []string {
	ids := make([]string, 0, len(dispatchRoles))
	for _, dr := range dispatchRoles {
		ids = append(ids, dr.analystID)
	}
	sort.Strings(ids)
	return ids
}
