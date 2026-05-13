package deltas

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTimeWindow_KindRange: when RangeStart and RangeEnd are set,
// the window is in "range" mode.
func TestTimeWindow_KindRange(t *testing.T) {
	t.Parallel()
	w := TimeWindow{
		RangeStart: time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
		RangeEnd:   time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC),
	}
	assert.Equal(t, "range", w.Kind())
}

// TestTimeWindow_KindPrecedence: All / Last / Range / Cutoff have a
// stable precedence in Kind(). All wins outright (it's the
// "everything" mode); Last second; Range third; Cutoff is the
// fallback "since" reading.
func TestTimeWindow_KindPrecedence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		w    TimeWindow
		want string
	}{
		{"all-only", TimeWindow{All: true}, "all"},
		{"last-only", TimeWindow{Last: 5}, "last"},
		{"range-only", TimeWindow{
			RangeStart: time.Now(),
			RangeEnd:   time.Now().Add(time.Hour),
		}, "range"},
		{"cutoff-only", TimeWindow{Cutoff: time.Now()}, "since"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.w.Kind())
		})
	}
}

// TestTimeWindow_MarshalJSON_Range: range mode emits both endpoints
// alongside the kind discriminator.
func TestTimeWindow_MarshalJSON_Range(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 12, 23, 59, 59, 0, time.UTC)
	w := TimeWindow{RangeStart: start, RangeEnd: end}

	b, err := json.Marshal(w)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))

	assert.Equal(t, "range", decoded["kind"])
	assert.Equal(t, "2026-05-10T00:00:00Z", decoded["range_start"])
	assert.Equal(t, "2026-05-12T23:59:59Z", decoded["range_end"])
	// cutoff and last should be omitted since they're zero.
	_, hasCutoff := decoded["cutoff"]
	_, hasLast := decoded["last"]
	assert.False(t, hasCutoff, "cutoff omitted in range mode")
	assert.False(t, hasLast, "last omitted in range mode")
}

// TestTimeWindow_MarshalJSON_SinceUnchanged: existing "since" mode
// keeps its previous JSON shape — no regression to existing
// consumers.
func TestTimeWindow_MarshalJSON_SinceUnchanged(t *testing.T) {
	t.Parallel()
	cutoff := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	w := TimeWindow{Cutoff: cutoff}

	b, err := json.Marshal(w)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))
	assert.Equal(t, "since", decoded["kind"])
	assert.Equal(t, "2026-05-12T00:00:00Z", decoded["cutoff"])
	_, hasStart := decoded["range_start"]
	assert.False(t, hasStart, "range_start omitted in since mode")
}
