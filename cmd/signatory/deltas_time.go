package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// parseTimeShorthand resolves the deltas verb's --since flag value
// into an absolute cutoff time. Accepts:
//
//   - Word shortcuts: "today", "yesterday", "last-week", "last-month"
//     (also accepts the more common "last week" / "last month" with a
//     space)
//   - Go duration: "12h", "30m" (relative to now)
//   - Day-extended duration: "2d", "7d" — Go's time.ParseDuration
//     does NOT accept the "d" unit; this parser converts to hours
//     before delegating
//   - Composite day+hour: "1d12h" — pre-parser strips the "d"
//     portion, adds the remainder
//   - RFC3339 timestamp: "2026-05-12T19:20:00Z" (absolute)
//
// Returns the absolute cutoff time, in UTC. Empty input returns a
// zero time.Time (which the caller can treat as "no bound").
//
// The reference time for relative shortcuts is time.Now().UTC().
// Tests injecting a fixed reference call parseTimeShorthandAt
// directly.
func parseTimeShorthand(raw string) (time.Time, error) {
	return parseTimeShorthandAt(raw, time.Now().UTC())
}

// parseTimeShorthandAt is the testable form — same logic but with
// an explicit reference time so tests can pin "now."
func parseTimeShorthandAt(raw string, now time.Time) (time.Time, error) {
	now = now.UTC()
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, nil
	}
	// Word shortcuts. Normalize "last week" → "last-week" and case.
	lowered := strings.ToLower(strings.ReplaceAll(trimmed, " ", "-"))
	switch lowered {
	case "today":
		// Start of today, UTC. "Today" means "since midnight UTC."
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), nil
	case "yesterday":
		return now.Add(-24 * time.Hour), nil
	case "last-week":
		return now.Add(-7 * 24 * time.Hour), nil
	case "last-month":
		// 30 days; not calendar-month. Documented.
		return now.Add(-30 * 24 * time.Hour), nil
	}

	// Day-extended duration: pre-convert "Nd" to "N*24h".
	if normalized, ok := convertDayUnit(trimmed); ok {
		if d, err := time.ParseDuration(normalized); err == nil {
			return now.Add(-d), nil
		}
	}

	// Plain Go duration: "12h", "30m", etc.
	if d, err := time.ParseDuration(trimmed); err == nil {
		return now.Add(-d), nil
	}

	// RFC3339 absolute timestamp.
	if t, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return t.UTC(), nil
	}

	return time.Time{}, fmt.Errorf("not a word shortcut, duration, or RFC3339 timestamp: %q", raw)
}

// dayUnitPattern matches one or more leading "Nd" segments,
// optionally followed by additional duration syntax. Captures the
// day-count and the rest.
var dayUnitPattern = regexp.MustCompile(`^(\d+)d(.*)$`)

// parseRangeShorthand resolves the deltas verb's --range flag value
// into two absolute times (start, end). Syntax is "T1..T2" — two
// endpoints separated by literal "..". Each endpoint accepts the
// same syntax as --since (word shortcuts, Go durations including
// "Nd", or RFC3339 timestamps).
//
// Both endpoints are inclusive; the rendered window is [start, end].
// The parser rejects:
//
//   - Empty input
//   - Missing "..".
//   - Empty endpoint on either side
//   - An endpoint that parseTimeShorthand can't resolve
//   - start > end (reversed range)
//
// Equal endpoints (start == end) are permitted and resolve to a
// single point-in-time match.
func parseRangeShorthand(raw string) (time.Time, time.Time, error) {
	return parseRangeShorthandAt(raw, time.Now().UTC())
}

// parseRangeShorthandAt is the testable form with an explicit
// reference time.
func parseRangeShorthandAt(raw string, now time.Time) (time.Time, time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("empty range")
	}
	parts := strings.SplitN(trimmed, "..", 2)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"range %q: missing '..' separator (expected 'T1..T2')", raw)
	}
	startRaw := strings.TrimSpace(parts[0])
	endRaw := strings.TrimSpace(parts[1])
	if startRaw == "" {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"range %q: empty start endpoint", raw)
	}
	if endRaw == "" {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"range %q: empty end endpoint", raw)
	}
	start, err := parseTimeShorthandAt(startRaw, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"range %q: start endpoint: %w", raw, err)
	}
	end, err := parseTimeShorthandAt(endRaw, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"range %q: end endpoint: %w", raw, err)
	}
	if start.After(end) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"range %q: start (%s) is after end (%s)",
			raw, start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	return start, end, nil
}

// convertDayUnit transforms "Nd[rest]" into "(N*24)h[rest]" so
// time.ParseDuration can consume it. Returns (converted, true) on
// match; (raw, false) when the input has no "d" prefix segment.
//
// Examples:
//
//	"2d"     → "48h"
//	"7d"     → "168h"
//	"1d12h"  → "24h12h"  (ParseDuration sums "24h12h" → 36h)
func convertDayUnit(raw string) (string, bool) {
	matches := dayUnitPattern.FindStringSubmatch(raw)
	if matches == nil {
		return raw, false
	}
	days, err := strconv.Atoi(matches[1])
	if err != nil {
		return raw, false
	}
	return fmt.Sprintf("%dh%s", days*24, matches[2]), true
}
