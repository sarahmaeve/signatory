package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/certs"
	"github.com/sarahmaeve/signatory/internal/pipeline"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"

	_ "modernc.org/sqlite"
)

// TestPipelineRun_StartPhase is the happy-path integration test for
// the "start" half of the orchestrator state machine. It composes
// prepare + dispatch-prompts (security + provenance only) and emits
// a single JSON event the host LLM consumes to dispatch its agents.
//
// The event shape is the host-agnostic interface: any LLM runtime
// that can read JSON, dispatch a constrained agent, and exec a follow-
// up command can host this pipeline. The Claude Code skill becomes
// a thin adapter; an OpenAI / Cursor / plain-SDK host writes its own
// thin adapter to the same JSON contract.
func TestPipelineRun_StartPhase(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t, newMockCollector())
	cloneDir := filepath.Join(t.TempDir(), "clones")

	var stdout, stderr bytes.Buffer
	cmd := &PipelineRunCmd{
		Target:      "https://github.com/JedWatson/classnames",
		CloneDir:    cloneDir,
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
		CertsChecker: func() certs.CheckResult {
			return certs.CheckResult{OK: true}
		},
		RunGitClone: func(_ context.Context, _, dest, _ string) error {
			return os.MkdirAll(dest, 0o755)
		},
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"package.json"},
			Language: "JavaScript",
		},
	}

	require.NoError(t, cmd.Run(globals))

	var result RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	// Phase identifier — the host's branch on this field.
	assert.Equal(t, "analysts_dispatch_required", result.Phase,
		"start phase emits analysts_dispatch_required")

	// Session IDs are present and distinct (the cef3c5ab synthesis bug
	// was a confusion between these two; the contract makes them
	// separate top-level fields).
	assert.Regexp(t, uuidRegex, result.SessionID+"\n",
		"session_id must be a UUID")
	assert.Regexp(t, uuidRegex, result.AnalysisSessionID+"\n",
		"analysis_session_id must be a UUID")
	assert.NotEqual(t, result.SessionID, result.AnalysisSessionID,
		"pipeline session ID and analysis session ID must differ")

	// Target metadata propagated for the host to display.
	assert.Equal(t, "https://github.com/JedWatson/classnames", result.Target)
	assert.Equal(t, "classnames", result.TargetName)
	assert.NotEmpty(t, result.TargetURL)
	assert.Contains(t, result.ClonePath, "classnames")

	// Both analyst dispatches are emitted, security first then
	// provenance (deterministic order so the host can iterate without
	// surprise — alphabetical is the simple rule).
	require.Len(t, result.Dispatches, 2,
		"start phase emits exactly two analyst dispatches")
	roles := []string{result.Dispatches[0].Role, result.Dispatches[1].Role}
	assert.ElementsMatch(t, []string{"security", "provenance"}, roles,
		"start phase dispatches must be security + provenance")

	// Each dispatch carries the three fields the host needs to make
	// an Agent() / Assistants run / SDK call.
	for _, d := range result.Dispatches {
		assert.NotEmpty(t, d.Description, "%s: description", d.Role)
		assert.NotEmpty(t, d.Prompt, "%s: prompt", d.Role)
		assert.NotEmpty(t, d.AllowedTools, "%s: allowed_tools", d.Role)

		// Substitution actually happened — no naked placeholders.
		assert.NotContains(t, d.Prompt, "{SESSION_ID}",
			"%s: {SESSION_ID} not substituted", d.Role)
		assert.NotContains(t, d.Prompt, "{ANALYSIS_SID}",
			"%s: {ANALYSIS_SID} not substituted", d.Role)
		assert.NotContains(t, d.Prompt, "{TARGET}",
			"%s: {TARGET} not substituted", d.Role)
		assert.Contains(t, d.Prompt, result.SessionID,
			"%s: prompt must reference pipeline session ID for handoff WebFetch", d.Role)
	}

	// Synthesist is NOT in the start phase — it's deferred until
	// analysts have landed (M6c contract).
	for _, d := range result.Dispatches {
		assert.NotEqual(t, "synthesist", d.Role,
			"synthesist must not be dispatched in start phase")
	}

	// next_command tells the host how to resume after the analysts
	// have landed. Argv form (not a shell string) so hosts can exec
	// without re-parsing or escaping.
	require.NotEmpty(t, result.NextCommand,
		"next_command must be present so the host knows how to resume")
	assert.Equal(t, "signatory", result.NextCommand[0])
	assert.Contains(t, result.NextCommand, "run")
	assert.Contains(t, result.NextCommand, "--resume")
	assert.Contains(t, result.NextCommand, result.AnalysisSessionID,
		"next_command must thread the analysis session ID")

	// Instructions string is human-readable (and LLM-readable) prose
	// describing what to do with the dispatches.
	assert.NotEmpty(t, result.Instructions,
		"instructions field must guide the host")
	assert.True(t,
		strings.Contains(result.Instructions, "dispatch") ||
			strings.Contains(result.Instructions, "Dispatch"),
		"instructions must mention dispatching")
}

// TestPipelineRun_StartPhase_PrepareFails verifies that a failure in
// the prepare composition (here: certs check) bubbles up as a Run
// error with no JSON output — the host sees stderr diagnostics and a
// non-zero exit code, not a partial event.
func TestPipelineRun_StartPhase_PrepareFails(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelineRunCmd{
		Target:      "https://github.com/JedWatson/classnames",
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
		CertsChecker: func() certs.CheckResult {
			return certs.CheckResult{
				OK:      false,
				Message: "NODE_EXTRA_CA_CERTS is not set",
				Fix:     "run `signatory certs init --write-profile`",
			}
		},
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NODE_EXTRA_CA_CERTS",
		"prepare failure must propagate the underlying message")
	assert.Empty(t, stdout.String(),
		"no partial JSON when prepare fails — host sees nothing on stdout")
}

// TestPipelineRun_RejectsTargetWithResume verifies that --resume
// requires no target (it identifies the session by ID) and rejects
// the combination as an operator-intent error. Catches the obvious
// mistake of running `pipeline run "$TARGET" --resume "$SID"` and
// silently re-preparing instead of resuming.
func TestPipelineRun_RejectsTargetWithResume(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelineRunCmd{
		Target: "https://github.com/JedWatson/classnames",
		Resume: "11111111-2222-3333-4444-555555555555",
		Stdout: &stdout,
		Stderr: &stderr,
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--resume",
		"error must name --resume as the conflicting flag")
	assert.Empty(t, stdout.String(),
		"no JSON output on usage error")
}

// TestPipelineRun_ResumePhaseHappyPath verifies the resume side of
// the orchestrator: given an analysis session with both analysts
// landed, --resume verifies landing, renders + deposits the
// synthesis handoff, and emits a synthesist_dispatch_required
// event whose next_command points at `pipeline close`.
//
// This is the second of the two LLM-synchronization points the
// state machine drives — after this event, the host dispatches the
// synthesist agent, then runs `pipeline close <sid>` to retrieve
// the proposed posture for user confirmation.
func TestPipelineRun_ResumePhaseHappyPath(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t)
	ctx := t.Context()

	// Set up a session that already has both analysts landed.
	// Mirrors the post-start-phase state of the world.
	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	sess := newTestAnalysisSessionWithPipeline(t, s, ts.URL,
		"https://github.com/JedWatson/classnames",
		[]string{"signatory-security-v1", "signatory-provenance-v1", "signatory-synthesis-v1"},
	)
	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")
	ingestTestOutput(t, s, sess.ID, "signatory-provenance-v1")

	var stdout, stderr bytes.Buffer
	cmd := &PipelineRunCmd{
		Resume:      sess.ID,
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}
	require.NoError(t, cmd.Run(globals))

	var result RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "synthesist_dispatch_required", result.Phase,
		"resume phase emits synthesist_dispatch_required when analysts landed")

	// IDs propagated from the loaded session.
	assert.Equal(t, sess.PipelineSessionID, result.SessionID,
		"session_id must be the pipeline session ID stored on the analysis row")
	assert.Equal(t, sess.ID, result.AnalysisSessionID)
	assert.Equal(t, sess.TargetURI, result.Target)
	assert.NotEmpty(t, result.TargetName)

	// Exactly one dispatch — synthesist.
	require.Len(t, result.Dispatches, 1,
		"resume phase emits exactly one dispatch (synthesist)")
	assert.Equal(t, "synthesist", result.Dispatches[0].Role)
	assert.NotEmpty(t, result.Dispatches[0].Prompt)
	assert.NotEmpty(t, result.Dispatches[0].AllowedTools)

	// Synthesist prompt was rendered with both session IDs (the cef3c5ab
	// confusion-bug fix lives in the template; resume must wire both
	// values into the substitution).
	synthBody := result.Dispatches[0].Prompt
	assert.Contains(t, synthBody, sess.PipelineSessionID,
		"synthesist prompt must contain pipeline session ID for handoff WebFetch")
	assert.Contains(t, synthBody, sess.ID,
		"synthesist prompt must contain analysis session ID for ingest")

	// next_command points at close — argv form, threaded session ID.
	require.NotEmpty(t, result.NextCommand)
	assert.Equal(t, "signatory", result.NextCommand[0])
	assert.Contains(t, result.NextCommand, "close")
	assert.Contains(t, result.NextCommand, sess.ID)

	// The synthesis handoff was deposited into the pipeline session.
	// Retrieve it via the same URL pattern the synthesist agent uses
	// at dispatch time — catches a class of bug where the deposit
	// silently no-ops.
	url := ts.URL + "/api/sessions/" + sess.PipelineSessionID +
		"/messages?role=synthesist&type=handoff&format=raw"
	resp, err := http.Get(url) //nolint:gosec // G107: test URL from httptest
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"synthesist handoff GET failed: %s", string(body))
	assert.NotEmpty(t, body, "synthesist handoff content must not be empty")
}

// TestPipelineRun_ResumePhase_MissingAnalyst verifies that resuming
// a session where one expected non-synthesis analyst hasn't landed
// emits a missing_analysts event (not an error), names the missing
// role, and does NOT render or deposit a synthesis handoff.
//
// The host's correct response is to re-dispatch the named analyst
// and re-run the resume command — the v0.1 deterministic pipeline's
// answer to the "what if an analyst silently failed?" failure mode
// that previously required prose-parsing of show-analyses output.
func TestPipelineRun_ResumePhase_MissingAnalyst(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	sess := newTestAnalysisSessionWithPipeline(t, s, ts.URL,
		"https://github.com/JedWatson/classnames",
		[]string{"signatory-security-v1", "signatory-provenance-v1", "signatory-synthesis-v1"},
	)
	// Only security lands; provenance is missing.
	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")

	var stdout, stderr bytes.Buffer
	cmd := &PipelineRunCmd{
		Resume:      sess.ID,
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}

	// Missing analysts is a transient state, not an error — the
	// host re-dispatches and retries. Run() returns nil; the JSON
	// event tells the host what happened.
	require.NoError(t, cmd.Run(globals),
		"missing analysts is a transient state expressed via JSON, not an error")

	var result RunResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "missing_analysts", result.Phase,
		"phase must name the missing-analysts state")
	assert.Equal(t, sess.ID, result.AnalysisSessionID)
	assert.Equal(t, []string{"signatory-provenance-v1"}, result.Missing,
		"missing must name the unlanded analyst — and only it (synthesis-v1 is filtered)")

	// No dispatches and no next_command — the host's job is to
	// re-dispatch the missing analyst and re-run resume, not to
	// proceed.
	assert.Empty(t, result.Dispatches,
		"missing_analysts emits no dispatches")
	assert.Empty(t, result.NextCommand,
		"missing_analysts emits no next_command — host re-dispatches and re-runs")

	// Instructions describe the recovery path.
	assert.NotEmpty(t, result.Instructions,
		"instructions must guide the host through re-dispatch")
	assert.Contains(t, strings.ToLower(result.Instructions), "re-dispatch")

	// No synthesis handoff was deposited — confirms we exited before
	// the deposit step. The pipeline server returns 200 with an
	// empty JSON array when no messages match the filter, so check
	// the body content rather than the status code (raw mode only
	// kicks in when exactly one message is present).
	url := ts.URL + "/api/sessions/" + sess.PipelineSessionID +
		"/messages?role=synthesist&type=handoff"
	resp, err := http.Get(url) //nolint:gosec // G107: test URL from httptest
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, "[]\n", string(body),
		"synthesis handoff must NOT have been deposited when analysts are missing; "+
			"got body: %s", string(body))
}

// TestPipelineRun_ResumePhase_SessionNotFound verifies that resuming
// against an unknown session ID produces a clear error and no
// partial output — the host sees nothing on stdout.
func TestPipelineRun_ResumePhase_SessionNotFound(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelineRunCmd{
		Resume:      "00000000-0000-0000-0000-000000000000",
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, stdout.String())
}

// TestPipelineRun_StartPhaseRequiresTarget verifies that the start
// phase (no --resume) requires a target. Without one there is
// nothing to prepare.
func TestPipelineRun_StartPhaseRequiresTarget(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelineRunCmd{
		// no Target, no Resume
		Stdout: &stdout,
		Stderr: &stderr,
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target",
		"error must explain a target is required")
	assert.Empty(t, stdout.String())
}

// newTestAnalysisSessionWithPipeline is the resume-test analog of
// newTestAnalysisSession (pipeline_verify_test.go). It additionally
// creates a real pipeline session via the test server and links the
// analysis session to it via PipelineSessionID — required for the
// resume phase, which needs the pipeline session to deposit the
// synthesis handoff into.
func newTestAnalysisSessionWithPipeline(
	t *testing.T,
	s store.Store,
	pipelineURL, target string,
	expectedAnalysts []string,
) *profile.AnalysisSession {
	t.Helper()
	ctx := t.Context()

	// Create a real pipeline-service session so the synthesis handoff
	// has a transport to land in.
	client, err := pipeline.NewClient(pipelineURL)
	require.NoError(t, err)
	psess, err := client.CreateSession(ctx, target, "")
	require.NoError(t, err)

	entity, err := ensureEntity(ctx, s, target)
	require.NoError(t, err)

	sess := &profile.AnalysisSession{
		ID:                uuid.NewString(),
		EntityID:          entity.ID,
		TargetURI:         target,
		InvokedBy:         "team:test",
		PipelineSessionID: psess.ID,
		ExpectedAnalysts:  expectedAnalysts,
		StartedAt:         time.Now().UTC(),
		Status:            profile.AnalysisSessionInProgress,
	}
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))
	return sess
}
