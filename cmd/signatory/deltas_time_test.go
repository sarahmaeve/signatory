package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixed reference time for all tests — chosen to make hand math easy.
var refNow = time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)

func TestParseTimeShorthand_Empty(t *testing.T) {
	t.Parallel()
	got, err := parseTimeShorthandAt("", refNow)
	require.NoError(t, err)
	assert.True(t, got.IsZero(), "empty input returns zero time")
}

func TestParseTimeShorthand_WordShortcuts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want time.Time
	}{
		{"today", time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)},
		{"yesterday", time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)},
		{"last-week", time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)},
		{"last-month", time.Date(2026, 4, 12, 12, 0, 0, 0, time.UTC)},
		// "last week" with space should normalize identically.
		{"last week", time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)},
		// Case-insensitive.
		{"YESTERDAY", time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseTimeShorthandAt(tc.raw, refNow)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseTimeShorthand_GoDuration(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw    string
		offset time.Duration
	}{
		{"12h", -12 * time.Hour},
		{"30m", -30 * time.Minute},
		{"24h", -24 * time.Hour},
		{"168h", -168 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseTimeShorthandAt(tc.raw, refNow)
			require.NoError(t, err)
			assert.Equal(t, refNow.Add(tc.offset), got)
		})
	}
}

func TestParseTimeShorthand_DayUnit(t *testing.T) {
	t.Parallel()
	// "Nd" → -(N*24h) relative to refNow.
	cases := []struct {
		raw    string
		offset time.Duration
	}{
		{"2d", -48 * time.Hour},
		{"7d", -7 * 24 * time.Hour},
		{"1d", -24 * time.Hour},
		// Composite: 1d12h = 36h total.
		{"1d12h", -36 * time.Hour},
		{"7d6h", -7*24*time.Hour - 6*time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseTimeShorthandAt(tc.raw, refNow)
			require.NoError(t, err)
			assert.Equal(t, refNow.Add(tc.offset), got,
				"day-unit parsing must convert Nd to (N*24)h before delegating to Go's ParseDuration")
		})
	}
}

func TestParseTimeShorthand_RFC3339(t *testing.T) {
	t.Parallel()
	got, err := parseTimeShorthandAt("2026-05-12T19:20:00Z", refNow)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 5, 12, 19, 20, 0, 0, time.UTC), got)
}

func TestParseTimeShorthand_Invalid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"not-a-thing",
		"tomorrow", // not in our shortcut list
		"2d3y",     // years not supported
		"abc",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			_, err := parseTimeShorthandAt(tc, refNow)
			assert.Error(t, err)
		})
	}
}

func TestParseTimeShorthand_WhitespaceTolerated(t *testing.T) {
	t.Parallel()
	got, err := parseTimeShorthandAt("  12h  ", refNow)
	require.NoError(t, err)
	assert.Equal(t, refNow.Add(-12*time.Hour), got,
		"leading/trailing whitespace should be tolerated")
}
