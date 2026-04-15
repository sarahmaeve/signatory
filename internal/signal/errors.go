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
func (r *CollectionResult) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Collected %d signals", len(r.Collected))
	if len(r.Failures) > 0 {
		fmt.Fprintf(&b, ", %d failures", len(r.Failures))
		retryable := 0
		for _, f := range r.Failures {
			if f.Retryable {
				retryable++
			}
		}
		if retryable > 0 {
			fmt.Fprintf(&b, " (%d retryable)", retryable)
		}
	}
	return b.String()
}
