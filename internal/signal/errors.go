package signal

import (
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// CollectionResult captures the outcome of a signal collection attempt,
// including both successful signals and failures. Partial success is
// expected — a rate-limited API call should not prevent other signals
// from being collected and stored.
type CollectionResult struct {
	// Signals successfully collected.
	Collected []SignalOrAbsence

	// Failures encountered during collection. Each entry describes
	// what signal was being collected and why it failed.
	Failures []CollectionError
}

// Signals extracts all collected signals (including absence records
// converted to signals) as a flat slice for storage.
func (r *CollectionResult) Signals() []profile.Signal {
	if r == nil {
		return nil
	}
	signals := make([]profile.Signal, 0, len(r.Collected))
	for _, s := range r.Collected {
		signals = append(signals, s.ToSignal())
	}
	return signals
}

// SignalCount returns the number of real signals (not absences).
func (r *CollectionResult) SignalCount() int {
	count := 0
	for _, s := range r.Collected {
		if !s.IsAbsence() {
			count++
		}
	}
	return count
}

// AbsenceCount returns the number of absence records.
func (r *CollectionResult) AbsenceCount() int {
	count := 0
	for _, s := range r.Collected {
		if s.IsAbsence() {
			count++
		}
	}
	return count
}

// RetryableCount returns the number of retryable failures.
func (r *CollectionResult) RetryableCount() int {
	count := 0
	for _, f := range r.Failures {
		if f.Retryable {
			count++
		}
	}
	return count
}

// CollectionError records a failed signal collection attempt.
type CollectionError struct {
	// SignalType is the signal that failed to collect (e.g., "stars", "contributors").
	SignalType string

	// Source is the collector that failed (e.g., "github").
	Source string

	// Reason is a sanitized, safe-to-persist description of why collection
	// failed. This must NEVER contain raw error messages, which may include
	// tokens, URLs with credentials, or API response bodies.
	Reason string

	// Retryable indicates whether this failure might succeed on retry
	// (e.g., rate limiting vs. 404).
	Retryable bool
}

func (f CollectionError) Error() string {
	retry := ""
	if f.Retryable {
		retry = " (retryable)"
	}
	return fmt.Sprintf("failed to collect %s from %s: %s%s", f.SignalType, f.Source, f.Reason, retry)
}

// HasFailures returns true if any signal collection failed.
func (r *CollectionResult) HasFailures() bool {
	return len(r.Failures) > 0
}

// Summary returns a human-readable summary of the collection result.
//
// Two enumerations live in the line: the collected signal types
// (after the count) and the failures (after a `;` separator). Both
// help a manual user reading the per-collector line in `signatory
// analyze` ([source] <summary>) see WHAT happened without scrolling
// through the rendered profile or re-running with extra logging:
//
//	Collected 4 signals: last_publish, version_count, absence:publish_origin, transparency_log_present
//	Collected 17 signals: stars, forks, ...; 1 failures (1 retryable): adoption=GitHub API 401
//
// Signal-type enumeration is in emission order, deduped (a collector
// emitting the same type twice shows it once). Absences carry the
// "absence:" prefix so a definitive negative (publish_origin not in
// proxy) reads distinctly from a positive observation (4 signals
// fired).
//
// Failure enumeration uses "type=reason" per entry, comma-separated.
// Reasons come from each collector's already-sanitized error
// classifier — the github package strips attacker-influenceable
// bodies via sanitizeErrorForStorage before any CollectionError is
// constructed, so the enumeration here is safe to surface verbatim.
//
// The bulk "(N retryable)" parenthetical is preserved alongside the
// per-failure detail because the count is useful at a glance even
// when the enumeration is long.
func (r *CollectionResult) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Collected %d signals", len(r.Collected))

	// Signal-type enumeration: list each unique type in emission
	// order. ToSignal().Type yields the canonical type name, prefixed
	// with "absence:" for absence records, so the output naturally
	// distinguishes the two without us hand-rolling the prefix here.
	if len(r.Collected) > 0 {
		seen := make(map[string]bool, len(r.Collected))
		types := make([]string, 0, len(r.Collected))
		for i := range r.Collected {
			t := r.Collected[i].ToSignal().Type
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			types = append(types, t)
		}
		if len(types) > 0 {
			b.WriteString(": ")
			b.WriteString(strings.Join(types, ", "))
		}
	}

	if len(r.Failures) == 0 {
		return b.String()
	}

	// Section separator from signal enumeration to failure enumeration:
	// `;` rather than `,` so the two ":"-introduced lists don't visually
	// merge into one ambiguous comma-separated stream.
	fmt.Fprintf(&b, "; %d failures", len(r.Failures))
	retryable := 0
	for _, f := range r.Failures {
		if f.Retryable {
			retryable++
		}
	}
	if retryable > 0 {
		fmt.Fprintf(&b, " (%d retryable)", retryable)
	}
	b.WriteString(": ")
	for i, f := range r.Failures {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%s", f.SignalType, f.Reason)
	}
	return b.String()
}
