package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// sessionSummary aggregates what we know about a single session
// across both data sources (hook events + OTEL trace spans).
// Either source alone is enough to surface the session in the
// listing — the goal is "what session ids exist?", not strict
// schema validation.
type sessionSummary struct {
	SessionID string
	FirstSeen time.Time
	LastSeen  time.Time
	HookCount int
	SpanCount int
}

// runListSessions scans inDir for hook JSONL files and the
// trace JSONL stream, builds a per-session summary, and writes
// a sorted (newest-last-seen first) table to w. Empty dir
// produces a clear "no sessions" message rather than an empty
// table.
//
// Used immediately before `report <session-id>` to find the
// right session id without `ls`+grep gymnastics.
func runListSessions(inDir string, w io.Writer) error {
	sessions := map[string]*sessionSummary{}

	if err := scanHookFiles(inDir, sessions); err != nil {
		return fmt.Errorf("scan hooks: %w", err)
	}
	if err := scanTraceFile(filepath.Join(inDir, "traces.jsonl"), sessions); err != nil {
		return fmt.Errorf("scan traces: %w", err)
	}

	if len(sessions) == 0 {
		fmt.Fprintln(w, "no sessions found in", inDir)
		return nil
	}

	list := make([]*sessionSummary, 0, len(sessions))
	for _, s := range sessions {
		list = append(list, s)
	}
	// Newest-last-seen first. Zero-time sessions (trace-only
	// without span timestamps) sort to the bottom — acceptable
	// since they're the corner case.
	sort.Slice(list, func(i, j int) bool {
		return list[i].LastSeen.After(list[j].LastSeen)
	})

	return renderSessionList(list, w)
}

// scanHookFiles walks every hooks-*.jsonl file in inDir,
// extracts the session id from the filename, and aggregates
// event counts + first/last timestamps. Files that don't parse
// or events with bad ts fields are skipped silently — the
// session listing is best-effort, not schema validation.
func scanHookFiles(inDir string, sessions map[string]*sessionSummary) error {
	matches, err := filepath.Glob(filepath.Join(inDir, "hooks-*.jsonl"))
	if err != nil {
		return err
	}
	for _, path := range matches {
		base := filepath.Base(path)
		sessionID := strings.TrimSuffix(strings.TrimPrefix(base, "hooks-"), ".jsonl")
		// "hooks-malformed.jsonl" is the parse-failure landing pad
		// from hook.go — not a real session, exclude it from the
		// listing so users don't try to `report` against it.
		if sessionID == "" || sessionID == "malformed" {
			continue
		}
		if err := scanHookFile(path, sessionID, sessions); err != nil {
			// Skip unreadable files but keep going — partial data
			// beats no listing.
			continue
		}
	}
	return nil
}

// scanHookFile reads one per-session hook JSONL and accumulates
// counts/timestamps into sessions[sessionID].
func scanHookFile(path, sessionID string, sessions map[string]*sessionSummary) error {
	f, err := os.Open(path) //nolint:gosec // G304: path from filepath.Glob inside caller-supplied inDir
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // close after read; no err to act on

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 4*1024), 1*1024*1024)
	for scanner.Scan() {
		var ev hookEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, ev.Timestamp)
		if err != nil {
			continue
		}
		s, ok := sessions[sessionID]
		if !ok {
			s = &sessionSummary{SessionID: sessionID, FirstSeen: ts, LastSeen: ts}
			sessions[sessionID] = s
		}
		s.HookCount++
		if ts.Before(s.FirstSeen) {
			s.FirstSeen = ts
		}
		if ts.After(s.LastSeen) {
			s.LastSeen = ts
		}
	}
	return scanner.Err()
}

// scanTraceFile reads the OTLP-JSON traces stream and groups
// spans by session.id. Checks the resource attribute first; if
// absent, falls back to per-span session.id — matching the shape
// report.go's loadTraces uses (c62b835). Span timestamps
// (startTimeUnixNano / endTimeUnixNano) extend the
// FirstSeen/LastSeen window where present.
//
// Missing file is NOT an error — trace-free environments
// (receiver wasn't running) are valid.
func scanTraceFile(path string, sessions map[string]*sessionSummary) error {
	f, err := os.Open(path) //nolint:gosec // G304: path from caller-supplied inDir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var batch otlpTraceBatch
		if err := json.Unmarshal(scanner.Bytes(), &batch); err != nil {
			continue
		}
		for _, rs := range batch.ResourceSpans {
			resSessionID := stringAttr(rs.Resource.Attributes, "session.id")
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					// Match by either resource or span attribute —
					// same dual-check as report.go's loadTraces.
					sessionID := resSessionID
					if sessionID == "" {
						sessionID = stringAttr(sp.Attributes, "session.id")
					}
					if sessionID == "" {
						continue
					}
					s, ok := sessions[sessionID]
					if !ok {
						s = &sessionSummary{SessionID: sessionID}
						sessions[sessionID] = s
					}
					s.SpanCount++
					if ts := nanoStringToTime(sp.StartTimeUnixNano); !ts.IsZero() {
						if s.FirstSeen.IsZero() || ts.Before(s.FirstSeen) {
							s.FirstSeen = ts
						}
					}
					if ts := nanoStringToTime(sp.EndTimeUnixNano); !ts.IsZero() {
						if ts.After(s.LastSeen) {
							s.LastSeen = ts
						}
					}
				}
			}
		}
	}
	return scanner.Err()
}

// nanoStringToTime parses an OTLP-encoded uint64-nanos-since-
// epoch field. Per OTLP-JSON proto encoding, int64/uint64
// fields are JSON strings (JSON's number type can't safely
// represent the full uint64 range). Empty / malformed input
// returns zero time, which the caller treats as "no signal."
func nanoStringToTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}

// renderSessionList writes a tabwriter-aligned table. Column
// order is the stable contract — pinned by
// TestListSessions_HeaderFormat — so users can pipe through
// awk/cut without surprises.
func renderSessionList(list []*sessionSummary, w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SESSION ID\tFIRST SEEN\tLAST SEEN\tHOOKS\tSPANS"); err != nil {
		return err
	}
	for _, s := range list {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n",
			s.SessionID,
			formatSummaryTime(s.FirstSeen),
			formatSummaryTime(s.LastSeen),
			s.HookCount,
			s.SpanCount,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// formatSummaryTime renders to ISO-ish minute precision —
// enough to distinguish sessions visually in a listing without
// the visual noise of seconds + timezone. Zero time renders as
// "-" so trace-only sessions without span timestamps don't
// confuse the eye.
func formatSummaryTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02T15:04")
}
