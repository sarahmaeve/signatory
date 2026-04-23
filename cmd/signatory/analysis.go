package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sarahmaeve/signatory/internal/identity"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// AnalysisCmd manages analysis-session lifecycle — the durable audit
// identity for each /analyze run.
//
// This is sibling to (not nested under) the AnalyzeCmd verb. AnalyzeCmd
// stays the Layer-1 signal-collection verb; AnalysisCmd is the noun-
// group for recorded runs:
//
//   - signatory analysis begin <target>    → start a run, print session-id
//   - signatory analysis end   <session-id> → close a run with a terminal status
//   - signatory analysis list               → recent runs, filtered
//   - signatory analysis show  <session-id> → one run with linked outputs
//
// Sessions live entirely in the main signatory store (see the
// Phase 3 store foundation — design/phase3-plan.md). This CLI
// surface is what the /analyze skill invokes from Claude Code; the
// skill does NOT need a pipeline-service round-trip to manage
// sessions, because WebFetch is GET-only and can't POST updates.
type AnalysisCmd struct {
	Begin  AnalysisBeginCmd  `cmd:"" help:"Start a new analysis session. Prints the session ID on stdout."`
	End    AnalysisEndCmd    `cmd:"" help:"Close an in-progress session with a terminal status."`
	List   AnalysisListCmd   `cmd:"" help:"List analysis sessions, newest first, optionally filtered."`
	Show   AnalysisShowCmd   `cmd:"" help:"Show one session with its linked analyst outputs."`
	Timing AnalysisTimingCmd `cmd:"" help:"Decompose the wall-clock of one session: per-analyst and phase-level latency."`
}

// --- analysis begin ---------------------------------------------------------

// AnalysisBeginCmd creates an in_progress analysis_sessions row and
// prints the new session ID. The skill's /analyze step reads this
// ID and substitutes it into analyst handoff templates, so each
// agent's signatory_ingest_analysis call can carry
// analysis_session_id and the FK gets stamped at INSERT time.
//
// The target may carry a @V suffix (pkg URIs only) OR be paired
// with --version. If both name a version, they must agree.
//
// The invoked-by identity is resolved from identity.Current by
// default; --invoked-by overrides for scripted runs where the
// environment doesn't carry the right SIGNATORY_TEAM.
type AnalysisBeginCmd struct {
	Target string `arg:"" help:"Target URI (canonical or URL form)."`

	Version           string   `help:"Target version to record (e.g. 1.2.3). Conflicts with a @V suffix on the target — pass one or the other." optional:""`
	ExpectedAnalysts  []string `name:"expected-analyst" help:"Analyst role ID the skill plans to dispatch (e.g. signatory-security-v1). Repeatable; order is preserved." optional:""`
	PipelineSessionID string   `name:"pipeline-session-id" help:"Pipeline-service session ID to correlate this run with pipeline logs. Optional." optional:""`
	Notes             string   `help:"Free-form operator commentary recorded at begin-time." optional:""`
	InvokedBy         string   `name:"invoked-by" help:"Override the team identity resolved by identity.Current. Only use when scripting across identities." optional:""`
	JSON              bool     `help:"Emit the created session row as JSON to stdout." default:"false"`

	// Writer injection for tests. Production paths leave these nil
	// and Run defaults them to os.Stdout/os.Stderr.
	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
}

func (cmd *AnalysisBeginCmd) Run(globals *Globals) error {
	stdout, _ := cmd.resolveWriters()
	ctx := context.Background()

	base, version, err := normalizeTargetForPosture(cmd.Target, cmd.Version)
	if err != nil {
		return NewUsageError(err)
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	entity, err := ensureEntity(ctx, s, base)
	if err != nil {
		return fmt.Errorf("resolve target %q: %w", base, err)
	}

	actor := strings.TrimSpace(cmd.InvokedBy)
	if actor == "" {
		actor, err = identity.Current()
		if err != nil {
			return fmt.Errorf("resolve team identity: %w", err)
		}
	}

	session := &profile.AnalysisSession{
		ID:                uuid.NewString(),
		EntityID:          entity.ID,
		TargetURI:         cmd.Target,
		TargetVersion:     version,
		InvokedBy:         actor,
		PipelineSessionID: cmd.PipelineSessionID,
		ExpectedAnalysts:  normalizeExpectedAnalysts(cmd.ExpectedAnalysts),
		StartedAt:         time.Now().UTC(),
		Status:            profile.AnalysisSessionInProgress,
		Notes:             cmd.Notes,
	}
	if err := s.CreateAnalysisSession(ctx, session); err != nil {
		return fmt.Errorf("create analysis session: %w", err)
	}

	if cmd.JSON {
		return writeJSON(stdout, session)
	}
	// Human-readable: print just the session ID so callers can
	// capture it with `SID=$(signatory analysis begin …)`.
	fmt.Fprintln(stdout, session.ID)
	return nil
}

func (cmd *AnalysisBeginCmd) resolveWriters() (io.Writer, io.Writer) {
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

// --- analysis end -----------------------------------------------------------

// AnalysisEndCmd closes an in_progress session with a terminal
// status. The terminal-state guard lives at the store layer — the
// CLI is a pass-through for user input plus error translation.
//
// Wall-clock ended_at is stamped by the CLI at call time. Callers
// that want to backdate the close can't — that's deliberate; the
// end timestamp is a product of "when did the operator run end",
// not an input field.
type AnalysisEndCmd struct {
	SessionID string `arg:"" name:"session-id" help:"ID of the session to close."`

	Status            string `help:"Terminal status to record: completed, failed, or partial." enum:"completed,failed,partial" required:""`
	SynthesisOutputID string `name:"synthesis-output-id" help:"Output ID of the synthesis that closed this run, if any." optional:""`
	JSON              bool   `help:"Emit the updated session row as JSON to stdout." default:"false"`

	// Stdout receives --json payloads. Stderr receives the human
	// confirmation line. Splitting the two channels keeps stdout
	// empty in human mode so `signatory analysis end … | jq` works
	// without the human string poisoning the downstream pipe.
	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
}

func (cmd *AnalysisEndCmd) Run(globals *Globals) error {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cmd.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck

	status := profile.AnalysisSessionStatus(cmd.Status)
	params := profile.AnalysisSessionCloseParams{
		Status:            status,
		EndedAt:           time.Now().UTC(),
		SynthesisOutputID: cmd.SynthesisOutputID,
	}
	if err := s.CloseAnalysisSession(ctx, cmd.SessionID, params); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return NewUsageError(fmt.Errorf("session %q not found", cmd.SessionID))
		}
		return fmt.Errorf("close session: %w", err)
	}

	if cmd.JSON {
		sess, err := s.GetAnalysisSession(ctx, cmd.SessionID)
		if err != nil {
			return fmt.Errorf("read back closed session: %w", err)
		}
		return writeJSON(stdout, sess)
	}
	// Confirmation goes to stderr so stdout stays empty for
	// scripts that pipe this command's output. `analysis end` is
	// an action verb, not a data-producing one.
	fmt.Fprintf(stderr, "Session %s closed (%s)\n", cmd.SessionID, status)
	return nil
}

// --- analysis list ----------------------------------------------------------

// AnalysisListCmd renders recent sessions, newest-first. Filters are
// conjunctive: setting --status=in_progress and --entity foo/bar
// lists in-progress sessions targeting that entity only.
//
// --since accepts either Go duration syntax (24h, 7d is NOT valid —
// use 168h) or an RFC3339 timestamp. Duration is interpreted as
// "since <now> - duration", an easy way to say "recent runs."
type AnalysisListCmd struct {
	Entity        string `help:"Filter to sessions targeting this entity (URI or UUID)." optional:""`
	Status        string `help:"Filter by lifecycle state." enum:",in_progress,completed,failed,partial" default:""`
	TargetVersion string `name:"target-version" help:"Filter to sessions whose target carried this @V." optional:""`
	Since         string `help:"Only show sessions started on or after this instant. Accepts Go duration (24h) or RFC3339 timestamp." optional:""`
	Limit         int    `help:"Maximum number of rows." default:"50"`
	JSON          bool   `help:"Emit rows as a JSON array." default:"false"`

	Stdout io.Writer `kong:"-"`
}

func (cmd *AnalysisListCmd) Run(globals *Globals) error {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck

	entityID, err := resolveEntityFilter(ctx, s, cmd.Entity)
	if err != nil {
		return NewUsageError(err)
	}

	since, err := parseSinceFlag(cmd.Since)
	if err != nil {
		return NewUsageError(fmt.Errorf("--since %q: %w", cmd.Since, err))
	}

	filter := store.AnalysisSessionFilter{
		EntityID:      entityID,
		TargetVersion: cmd.TargetVersion,
		Status:        profile.AnalysisSessionStatus(cmd.Status),
		Since:         since,
		Limit:         cmd.Limit,
	}
	sessions, err := s.ListAnalysisSessions(ctx, filter)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	if cmd.JSON {
		return writeJSON(stdout, sessions)
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No analysis sessions match the filter")
		return nil
	}
	for _, sess := range sessions {
		renderSessionLine(stdout, &sess)
	}
	return nil
}

// --- analysis show ----------------------------------------------------------

// AnalysisShowCmd renders one session with the analyst outputs that
// landed against it. The expected-vs-landed rollup uses the session
// row's ExpectedAnalysts as the reference set and the linked
// analyst_outputs rows' AnalystIDs as the landed set — difference in
// either direction shows up as a `missing` or `unexpected` tag.
type AnalysisShowCmd struct {
	SessionID string `arg:"" name:"session-id" help:"ID of the session to render."`
	JSON      bool   `help:"Emit the session + outputs as JSON." default:"false"`

	Stdout io.Writer `kong:"-"`
}

// AnalysisShowData is the JSON shape for `analysis show --json`. Its
// sibling fields let consumers pull session metadata and the landed
// analyst outputs in one payload without a second round-trip.
type AnalysisShowData struct {
	Session    *profile.AnalysisSession     `json:"session"`
	Outputs    []store.AnalystOutputSummary `json:"outputs"`
	Expected   []string                     `json:"expected_analysts,omitempty"`
	Landed     []string                     `json:"landed_analysts,omitempty"`
	Missing    []string                     `json:"missing_analysts,omitempty"`
	Unexpected []string                     `json:"unexpected_analysts,omitempty"`
}

func (cmd *AnalysisShowCmd) Run(globals *Globals) error {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck

	sess, err := s.GetAnalysisSession(ctx, cmd.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		return NewUsageError(fmt.Errorf("session %q not found", cmd.SessionID))
	}
	if err != nil {
		return fmt.Errorf("read session: %w", err)
	}

	outputs, err := s.ListOutputsForSession(ctx, cmd.SessionID)
	if err != nil {
		return fmt.Errorf("list outputs for session: %w", err)
	}

	missing, unexpected := diffAnalysts(sess.ExpectedAnalysts, outputs)
	landed := landedAnalystIDs(outputs)

	if cmd.JSON {
		return writeJSON(stdout, &AnalysisShowData{
			Session:    sess,
			Outputs:    outputs,
			Expected:   sess.ExpectedAnalysts,
			Landed:     landed,
			Missing:    missing,
			Unexpected: unexpected,
		})
	}
	renderSessionDetail(stdout, sess, outputs, missing, unexpected)
	return nil
}

// --- shared helpers ---------------------------------------------------------

// normalizeExpectedAnalysts trims and drops empty entries from a
// repeated flag. A zero-length result round-trips through the
// domain type as a nil/empty slice — the store layer handles that
// correctly without producing a ghost-empty-string row.
func normalizeExpectedAnalysts(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveEntityFilter turns a user-supplied --entity flag into an
// entities.id. Accepts a canonical URI, a URL form, or an exact
// entity UUID. Empty input returns empty (no filter). Unknown
// targets return a helpful error rather than an empty result set,
// since filtering on a typo'd URI would silently return "no rows"
// and mask the mistake.
func resolveEntityFilter(ctx context.Context, s store.Store, raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	entity, err := resolveEntity(ctx, s, raw)
	if errors.Is(err, store.ErrNotFound) {
		// Fall back: maybe the caller already gave us the UUID.
		// GetEntity answers for any legal UUID; we keep the
		// error shape uniform so the user sees one diagnostic.
		if ent, gerr := s.GetEntity(ctx, raw); gerr == nil {
			return ent.ID, nil
		}
		return "", fmt.Errorf("--entity %q not found in store", raw)
	}
	if err != nil {
		return "", err
	}
	return entity.ID, nil
}

// parseSinceFlag accepts either Go duration syntax (interpreted as
// "now minus duration") or an RFC3339 timestamp. Empty input
// returns a zero time.Time, which the filter treats as "no bound."
func parseSinceFlag(raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a duration or RFC3339 timestamp: %w", err)
	}
	return t.UTC(), nil
}

// diffAnalysts compares the expected-analyst list against the landed
// outputs, returning (missing, unexpected) as sorted-unique strings.
//
// Missing = expected but not landed.
//
// Unexpected = landed but not expected, EXCEPT synthesis roles. The
// orchestrator auto-appends synthesis as a capping step regardless of
// the operator's --expected-analyst list, so surfacing it as
// "unexpected" is noise that buries genuinely-surprising landings.
// See exchange.SynthesistAnalystIDPrefix for the detection contract.
func diffAnalysts(expected []string, outputs []store.AnalystOutputSummary) (missing, unexpected []string) {
	expectedSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e] = struct{}{}
	}
	landedSet := make(map[string]struct{}, len(outputs))
	for _, o := range outputs {
		landedSet[o.AnalystID] = struct{}{}
	}
	for e := range expectedSet {
		if _, ok := landedSet[e]; !ok {
			missing = append(missing, e)
		}
	}
	for l := range landedSet {
		if _, ok := expectedSet[l]; ok {
			continue
		}
		if isSynthesisAnalyst(l) {
			continue
		}
		unexpected = append(unexpected, l)
	}
	slices.Sort(missing)
	slices.Sort(unexpected)
	return missing, unexpected
}

// landedAnalystIDs returns the de-duplicated sorted set of analyst
// IDs observed across the session's outputs. Sort is for stable
// JSON and human output; dedupe collapses re-ingested rounds.
func landedAnalystIDs(outputs []store.AnalystOutputSummary) []string {
	set := make(map[string]struct{}, len(outputs))
	for _, o := range outputs {
		set[o.AnalystID] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// writeJSON marshals v as indented JSON + trailing newline. Used by
// every --json code path so the output is consistent and pipeable.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// displayTarget produces the user-visible target string for a
// session. The session row preserves the caller's raw TargetURI
// (possibly with @V) AND keeps TargetVersion split out for query
// filtering — so naive concatenation double-stamps @V when the
// caller wrote it into the URI directly.
//
// Rule: if TargetURI already carries the same version suffix as
// TargetVersion, render TargetURI alone. If TargetVersion was
// supplied separately (via --version on an unversioned URI),
// append it. Unversioned runs render TargetURI verbatim.
func displayTarget(sess *profile.AnalysisSession) string {
	if sess.TargetVersion == "" {
		return sess.TargetURI
	}
	_, uriVersion := profile.SplitURIVersion(sess.TargetURI)
	if uriVersion == sess.TargetVersion {
		return sess.TargetURI
	}
	return sess.TargetURI + "@" + sess.TargetVersion
}

// renderSessionLine formats one session for the list view. Compact
// enough that 50 rows fit the screen; verbose enough that "why did
// that run fail" is answerable at a glance.
func renderSessionLine(w io.Writer, sess *profile.AnalysisSession) {
	wallClock := ""
	if sess.EndedAt != nil {
		wallClock = fmt.Sprintf("  (%s)", sess.EndedAt.Sub(sess.StartedAt).Round(time.Second))
	}
	fmt.Fprintf(w, "%s  [%s]%s  %s  by=%s\n",
		shortID(sess.ID), sess.Status, wallClock,
		displayTarget(sess), sess.InvokedBy)
}

// renderSessionDetail formats one session + its landed outputs for
// the show verb. Layout:
//
//	<id> [status] wall=Xs
//	  target: pkg:npm/X@V
//	  by: team:…
//	  expected: a, b, c        (3 total)
//	  landed:   a, b           (2 total, 1 missing: c)
//	  synthesis: <output-id>   (when set)
//	  notes: ...
//	  ---
//	  <output-id>  <analyst-id>  round=N  ingested=…
//	  …
func renderSessionDetail(
	w io.Writer,
	sess *profile.AnalysisSession,
	outputs []store.AnalystOutputSummary,
	missing, unexpected []string,
) {
	fmt.Fprintf(w, "%s  [%s]", sess.ID, sess.Status)
	if sess.EndedAt != nil {
		fmt.Fprintf(w, "  wall=%s", sess.EndedAt.Sub(sess.StartedAt).Round(time.Second))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  target:    %s\n", displayTarget(sess))
	fmt.Fprintf(w, "  by:        %s\n", sess.InvokedBy)
	fmt.Fprintf(w, "  started:   %s\n", sess.StartedAt.Format(time.RFC3339))
	if sess.EndedAt != nil {
		fmt.Fprintf(w, "  ended:     %s\n", sess.EndedAt.Format(time.RFC3339))
	}
	if len(sess.ExpectedAnalysts) > 0 {
		fmt.Fprintf(w, "  expected:  %s  (%d total)\n",
			strings.Join(sess.ExpectedAnalysts, ", "), len(sess.ExpectedAnalysts))
	}
	if len(outputs) > 0 {
		fmt.Fprintf(w, "  landed:    %s  (%d total",
			strings.Join(landedAnalystIDs(outputs), ", "), len(outputs))
		if len(missing) > 0 {
			fmt.Fprintf(w, ", %d missing: %s", len(missing), strings.Join(missing, ", "))
		}
		if len(unexpected) > 0 {
			fmt.Fprintf(w, ", %d unexpected: %s", len(unexpected), strings.Join(unexpected, ", "))
		}
		fmt.Fprintln(w, ")")
	} else if len(sess.ExpectedAnalysts) > 0 {
		fmt.Fprintf(w, "  landed:    none yet  (%d missing: %s)\n",
			len(missing), strings.Join(missing, ", "))
	}
	if sess.SynthesisOutputID != "" {
		fmt.Fprintf(w, "  synthesis: %s\n", sess.SynthesisOutputID)
	}
	if sess.PipelineSessionID != "" {
		fmt.Fprintf(w, "  pipeline:  %s\n", sess.PipelineSessionID)
	}
	if sess.Notes != "" {
		fmt.Fprintf(w, "  notes:     %s\n", sess.Notes)
	}
	if len(outputs) == 0 {
		return
	}
	fmt.Fprintln(w, "  ---")
	for _, o := range outputs {
		fmt.Fprintf(w, "  %s  %s  round=%d  ingested=%s\n",
			shortID(o.OutputID), o.AnalystID, o.Round, o.IngestedAt)
	}
}
