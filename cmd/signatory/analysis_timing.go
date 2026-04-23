package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// AnalysisTimingCmd renders the wall-clock decomposition of one
// /analyze run: how long did each analyst take, where were the
// gaps, which phase dominated?
//
// All timestamps come from existing session + analyst_output rows:
//
//   - session.started_at → operator ran `analysis begin`.
//   - output.invoked_at  → analyst agent self-reported dispatch time.
//   - output.ingested_at → server observed the output land.
//   - session.ended_at   → operator ran `analysis end`, or synthesis
//     ingest auto-closed.
//
// "Agent wall" = ingested - invoked (agent's self-report).
// "Session wall" = ingested - session.started (operator's wait).
type AnalysisTimingCmd struct {
	SessionID string `arg:"" name:"session-id" help:"ID of the session to time."`
	JSON      bool   `help:"Emit the timing breakdown as JSON." default:"false"`

	Stdout io.Writer `kong:"-"`
}

// AnalystTiming is one row of per-output timing. Pointer-to-int
// durations carry explicit presence: a nil field means "we couldn't
// compute this" (WallNotes explains why); a non-nil zero means
// "sub-second duration, legitimately measured."
type AnalystTiming struct {
	AnalystID     string `json:"analyst_id"`
	OutputID      string `json:"output_id"`
	InvokedAt     string `json:"invoked_at,omitempty"`
	IngestedAt    string `json:"ingested_at"`
	AgentWallMS   *int64 `json:"agent_wall_ms,omitempty"`
	SessionWallMS *int64 `json:"session_wall_ms,omitempty"`
	IsSynthesis   bool   `json:"is_synthesis,omitempty"`
	WallNotes     string `json:"wall_notes,omitempty"`
}

// SessionTiming is the full payload: the session row, per-analyst
// rows, and session-level latency decomposition. Every *_MS field is
// *int64 so "unset" and "zero" are distinguishable on the wire.
type SessionTiming struct {
	Session  *profile.AnalysisSession `json:"session"`
	Analysts []AnalystTiming          `json:"analysts"`

	// SessionWallMS = ended - started. Nil when session is still in_progress.
	SessionWallMS *int64 `json:"session_wall_ms,omitempty"`

	// TimeToFirstOutputMS = first-output.ingested - session.started.
	// Nil when no parseable outputs landed.
	TimeToFirstOutputMS *int64 `json:"time_to_first_output_ms,omitempty"`

	// TimeToLastAnalystMS = last-non-synth.ingested - session.started.
	// Nil when no non-synthesis outputs landed.
	TimeToLastAnalystMS *int64 `json:"time_to_last_analyst_ms,omitempty"`

	// TimeToSynthesisMS = synth.ingested - session.started. Nil when
	// no synthesis output landed.
	TimeToSynthesisMS *int64 `json:"time_to_synthesis_ms,omitempty"`

	// SynthesisToCloseMS = session.ended - synth.ingested. Nil when
	// synthesis or session close is absent.
	SynthesisToCloseMS *int64 `json:"synthesis_to_close_ms,omitempty"`

	// Missing/Unexpected mirror the `analysis show` rollup.
	Missing    []string `json:"missing_analysts,omitempty"`
	Unexpected []string `json:"unexpected_analysts,omitempty"`
}

func (cmd *AnalysisTimingCmd) Run(globals *Globals) error {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

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

	timing := computeSessionTiming(sess, outputs)

	if cmd.JSON {
		return writeJSON(stdout, timing)
	}
	renderSessionTiming(stdout, timing)
	return nil
}

// computeSessionTiming derives the timing payload from a session +
// its linked outputs. Pure function — no I/O — so tests exercise
// every branch without seeding SQLite.
//
// Malformed invoked_at leaves AgentWallMS nil + WallNotes set;
// session-level aggregates still compute from server-stamped
// ingested_at. Malformed ingested_at leaves both wall fields nil
// and excludes the row from aggregates.
func computeSessionTiming(
	sess *profile.AnalysisSession,
	outputs []store.AnalystOutputSummary,
) *SessionTiming {
	out := &SessionTiming{
		Session:  sess,
		Analysts: make([]AnalystTiming, 0, len(outputs)),
	}

	if sess.EndedAt != nil {
		out.SessionWallMS = ptrMS(sess.EndedAt.Sub(sess.StartedAt))
	}

	// Deterministic chronological ordering for the renderer. RFC3339
	// lex-order matches time order for the Z timezone we persist.
	sorted := slices.Clone(outputs)
	slices.SortStableFunc(sorted, func(a, b store.AnalystOutputSummary) int {
		return strings.Compare(a.IngestedAt, b.IngestedAt)
	})

	var (
		firstIngested     time.Time
		lastAnalystIngest time.Time
		synthIngested     time.Time
		synthSeen         bool
	)

	for _, o := range sorted {
		row := AnalystTiming{
			AnalystID:   o.AnalystID,
			OutputID:    o.OutputID,
			InvokedAt:   o.InvokedAt,
			IngestedAt:  o.IngestedAt,
			IsSynthesis: isSynthesisAnalyst(o.AnalystID),
		}

		ingested, ingErr := time.Parse(time.RFC3339, o.IngestedAt)
		if ingErr != nil {
			row.WallNotes = fmt.Sprintf("ingested_at %q unparseable: %v", o.IngestedAt, ingErr)
			out.Analysts = append(out.Analysts, row)
			continue
		}
		row.SessionWallMS = ptrMS(ingested.Sub(sess.StartedAt))

		invoked, invErr := time.Parse(time.RFC3339, o.InvokedAt)
		switch {
		case invErr != nil:
			row.WallNotes = fmt.Sprintf("invoked_at %q unparseable; agent_wall unavailable", o.InvokedAt)
		case invoked.After(ingested):
			row.WallNotes = "invoked_at > ingested_at (clock skew or bogus agent timestamp)"
		default:
			row.AgentWallMS = ptrMS(ingested.Sub(invoked))
		}

		if firstIngested.IsZero() || ingested.Before(firstIngested) {
			firstIngested = ingested
		}
		if row.IsSynthesis {
			if !synthSeen || ingested.Before(synthIngested) {
				synthIngested = ingested
				synthSeen = true
			}
		} else if lastAnalystIngest.IsZero() || ingested.After(lastAnalystIngest) {
			lastAnalystIngest = ingested
		}

		out.Analysts = append(out.Analysts, row)
	}

	if !firstIngested.IsZero() {
		out.TimeToFirstOutputMS = ptrMS(firstIngested.Sub(sess.StartedAt))
	}
	if !lastAnalystIngest.IsZero() {
		out.TimeToLastAnalystMS = ptrMS(lastAnalystIngest.Sub(sess.StartedAt))
	}
	if synthSeen {
		out.TimeToSynthesisMS = ptrMS(synthIngested.Sub(sess.StartedAt))
		if sess.EndedAt != nil {
			out.SynthesisToCloseMS = ptrMS(sess.EndedAt.Sub(synthIngested))
		}
	}

	out.Missing, out.Unexpected = diffAnalysts(sess.ExpectedAnalysts, outputs)
	return out
}

// isSynthesisAnalyst reports whether the analyst_id names a
// synthesis role. Uses the canonical prefix from the exchange
// package so CLI and validator agree on what "synthesis" means.
func isSynthesisAnalyst(analystID string) bool {
	return strings.HasPrefix(analystID, exchange.SynthesistAnalystIDPrefix)
}

// renderSessionTiming formats the timing payload for humans as a
// ragged indented list — deliberately NOT a column-aligned table.
// At v0.1 cardinality (3-5 analyst rows) the aligned form bought
// almost nothing and wanted its own width-calculator machinery.
func renderSessionTiming(w io.Writer, t *SessionTiming) {
	sess := t.Session
	fmt.Fprintf(w, "%s  [%s]", sess.ID, sess.Status)
	if t.SessionWallMS != nil {
		fmt.Fprintf(w, "  wall=%s", formatMS(*t.SessionWallMS))
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  target: %s\n", displayTarget(sess))
	fmt.Fprintf(w, "  by:     %s\n", sess.InvokedBy)
	fmt.Fprintln(w)

	if len(t.Analysts) == 0 {
		fmt.Fprintln(w, "  (no analyst outputs linked to this session yet)")
	} else {
		fmt.Fprintln(w, "  analyst timeline (by ingested_at):")
		for _, a := range t.Analysts {
			renderAnalystRow(w, &a)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  latency decomposition:")
	renderLatencyLine(w, "begin → first output", t.TimeToFirstOutputMS)
	renderLatencyLine(w, "begin → last analyst", t.TimeToLastAnalystMS)
	renderLatencyLine(w, "begin → synthesis", t.TimeToSynthesisMS)
	renderLatencyLine(w, "synthesis → close", t.SynthesisToCloseMS)
	if t.SessionWallMS != nil {
		renderLatencyLine(w, "total (begin → close)", t.SessionWallMS)
	} else {
		fmt.Fprintln(w, "    total:                  (session still in_progress)")
	}

	if len(t.Missing) > 0 {
		fmt.Fprintf(w, "\n  expected-but-missing: %s\n", strings.Join(t.Missing, ", "))
	}
	if len(t.Unexpected) > 0 {
		fmt.Fprintf(w, "  unexpected landed:    %s\n", strings.Join(t.Unexpected, ", "))
	}
}

// renderAnalystRow writes one ragged indented block per analyst.
// Three lines: header (id + tags), ingested timestamp, and the two
// wall-clock derivations. Missing wall values render as a single
// "—" line with the WallNotes explanation.
func renderAnalystRow(w io.Writer, a *AnalystTiming) {
	synth := ""
	if a.IsSynthesis {
		synth = " [synthesis]"
	}
	fmt.Fprintf(w, "    %s%s\n", a.AnalystID, synth)
	fmt.Fprintf(w, "      ingested:     %s\n", a.IngestedAt)
	if a.SessionWallMS != nil {
		fmt.Fprintf(w, "      session_wall: %s\n", formatMS(*a.SessionWallMS))
	}
	if a.AgentWallMS != nil {
		fmt.Fprintf(w, "      agent_wall:   %s\n", formatMS(*a.AgentWallMS))
	}
	if a.WallNotes != "" {
		fmt.Fprintf(w, "      note:         %s\n", a.WallNotes)
	}
}

// renderLatencyLine writes one "label: duration" line, or nothing
// at all when the duration is nil. Keeps the caller's body from
// sprouting if-nil scaffolding around every line.
func renderLatencyLine(w io.Writer, label string, ms *int64) {
	if ms == nil {
		return
	}
	fmt.Fprintf(w, "    %s: %s\n", label, formatMS(*ms))
}

// formatMS renders a millisecond count as a compact Go-duration
// string. Sub-second durations round to 100ms to avoid "2.873s"
// noise; second+ rounds to whole seconds.
func formatMS(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// ptrMS converts a Go Duration to a pointer-to-int64 milliseconds
// so the JSON envelope can distinguish "unset" (nil) from "zero"
// (non-nil, value 0).
func ptrMS(d time.Duration) *int64 {
	ms := d.Milliseconds()
	return &ms
}
