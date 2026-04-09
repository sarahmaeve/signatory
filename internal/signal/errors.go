package signal

import (
	"fmt"
	"strings"
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
	Failures []CollectionFailure
}

// CollectionFailure records a failed signal collection attempt.
type CollectionFailure struct {
	// SignalType is the signal that failed to collect (e.g., "stars", "contributors").
	SignalType string

	// Source is the collector that failed (e.g., "github").
	Source string

	// Err is the underlying error.
	Err error

	// Retryable indicates whether this failure might succeed on retry
	// (e.g., rate limiting vs. 404).
	Retryable bool
}

func (f CollectionFailure) Error() string {
	retry := ""
	if f.Retryable {
		retry = " (retryable)"
	}
	return fmt.Sprintf("failed to collect %s from %s: %v%s", f.SignalType, f.Source, f.Err, retry)
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
