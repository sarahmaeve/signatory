package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/deltas"
	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// seedDeltasSignals appends N observations of one signal type
// (stars/github) at increasing timestamps, value = {"count": i+1}.
// Returns the (t0, tLast) bounds.
func seedDeltasSignals(t *testing.T, s store.Store, entityID, sigType string, n int) (time.Time, time.Time) {
	t.Helper()
	base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	var first, last time.Time
	for i := 0; i < n; i++ {
		at := base.Add(time.Duration(i) * 24 * time.Hour)
		if i == 0 {
			first = at
		}
		last = at
		sig := profile.Signal{
			ID:                fmt.Sprintf("ds-%s-%s-%d", entityID[:6], sigType, i),
			EntityID:          entityID,
			Type:              sigType,
			Group:             profile.SignalGroupCriticality,
			Source:            "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(fmt.Sprintf(`{"count": %d}`, i+1)),
			CollectedAt:       at,
			ExpiresAt:         at.Add(24 * time.Hour),
		}
		require.NoError(t, s.AppendSignals(context.Background(), []profile.Signal{sig}))
	}
	return first, last
}

// TestDeltasTool_HappyPath: a target with two observations of one
// signal type produces one transition; the response payload
// deserializes into a deltas.RenderInput-shape.
func TestDeltasTool_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "pkg:npm/example-mcp", "example-mcp")
	first, last := seedDeltasSignals(t, s, e.ID, "stars", 2)
	_ = first

	tool := &DeltasTool{Store: s}
	req := fmt.Sprintf(`{"target":"pkg:npm/example-mcp","range_start":"2026-04-01T00:00:00Z","range_end":%q}`,
		last.Add(time.Hour).Format(time.RFC3339))
	resp := tool.Handle(context.Background(), json.RawMessage(req))
	require.Equal(t, "ok", resp.Status, "unexpected error: %+v", resp.Error)

	payload, ok := resp.Data.(*DeltasResponse)
	require.True(t, ok, "response data must be *DeltasResponse, got %T", resp.Data)
	assert.Equal(t, "pkg:npm/example-mcp", payload.Target)
	require.Len(t, payload.Groups, 1, "one signal type → one group")
	assert.Equal(t, "stars", payload.Groups[0].Type)
	assert.Len(t, payload.Groups[0].PairDiffs, 1, "two observations → one transition")
	assert.False(t, payload.Truncated)
}

// TestDeltasTool_NotFound: unknown target → CodeNotFound with the
// canonical URI in the error message.
func TestDeltasTool_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &DeltasTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"pkg:npm/never","range_start":"2026-01-01T00:00:00Z","range_end":"2026-12-31T23:59:59Z"}`))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "pkg:npm/never")
}

// TestDeltasTool_RejectsMissingTarget: input schema enforcement.
func TestDeltasTool_RejectsMissingTarget(t *testing.T) {
	t.Parallel()
	tool := &DeltasTool{}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"since":"2026-05-01T00:00:00Z"}`))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

// TestDeltasTool_RejectsUnknownFields: strict decode — typos in
// field names must surface as schema violations, not silently drop.
func TestDeltasTool_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	tool := &DeltasTool{}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"pkg:npm/x","range_strt":"2026-05-01T00:00:00Z"}`))
	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

// TestDeltasTool_RequiresTimeScope: no time params at all → reject.
// Forces the agent to be explicit about the window; there's no
// implicit "all history" or "last 24h" default.
func TestDeltasTool_RequiresTimeScope(t *testing.T) {
	t.Parallel()
	tool := &DeltasTool{}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"pkg:npm/x"}`))
	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "since",
		"error must name the param the caller should set")
}

// TestDeltasTool_TimeParamsMutex: at most one of (since, last,
// range_start) may be set.
func TestDeltasTool_TimeParamsMutex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"since+last", `{"target":"pkg:npm/x","since":"2026-05-01T00:00:00Z","last":3}`},
		{"since+range_start", `{"target":"pkg:npm/x","since":"2026-05-01T00:00:00Z","range_start":"2026-05-01T00:00:00Z","range_end":"2026-05-12T00:00:00Z"}`},
		{"last+range_start", `{"target":"pkg:npm/x","last":3,"range_start":"2026-05-01T00:00:00Z","range_end":"2026-05-12T00:00:00Z"}`},
	}
	tool := &DeltasTool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := tool.Handle(context.Background(), json.RawMessage(tc.body))
			require.Equal(t, "error", resp.Status)
			assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
			assert.Contains(t, resp.Error.Message, "mutually exclusive")
		})
	}
}

// TestDeltasTool_RangeEndpointPairing: range_start without
// range_end (or vice versa) is rejected — the range is a pair.
func TestDeltasTool_RangeEndpointPairing(t *testing.T) {
	t.Parallel()
	tool := &DeltasTool{}
	cases := []string{
		`{"target":"pkg:npm/x","range_start":"2026-05-01T00:00:00Z"}`,
		`{"target":"pkg:npm/x","range_end":"2026-05-01T00:00:00Z"}`,
	}
	for i, body := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			t.Parallel()
			resp := tool.Handle(context.Background(), json.RawMessage(body))
			require.Equal(t, "error", resp.Status)
			assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
		})
	}
}

// TestDeltasTool_TransitionsCapTruncates: a target with more than
// the transitions cap produces a Truncated=true response with the
// groups slice trimmed to fit under the cap.
func TestDeltasTool_TransitionsCapTruncates(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "pkg:npm/firehose", "firehose")

	// Seed enough signal types and observations to exceed
	// TransitionsCap. With DeltasCap = 200, seed N signal types
	// each with 10 observations → 9 transitions per group →
	// need >22 groups to overflow. Generate 25.
	for i := 0; i < 25; i++ {
		seedDeltasSignals(t, s, e.ID, fmt.Sprintf("metric_%02d", i), 10)
	}

	tool := &DeltasTool{Store: s}
	req := `{"target":"pkg:npm/firehose","range_start":"2026-01-01T00:00:00Z","range_end":"2030-12-31T23:59:59Z"}`
	resp := tool.Handle(context.Background(), json.RawMessage(req))
	require.Equal(t, "ok", resp.Status, "unexpected error: %+v", resp.Error)

	payload := resp.Data.(*DeltasResponse)
	assert.True(t, payload.Truncated,
		"25 groups × 9 transitions = 225 > 200, must truncate")
	assert.Greater(t, payload.GroupsTotal, payload.GroupsReturned,
		"GroupsTotal must reflect pre-truncation count")

	// Verify the kept groups carry at most TransitionsCap transitions.
	sum := 0
	for _, g := range payload.Groups {
		sum += len(g.PairDiffs)
	}
	assert.LessOrEqual(t, sum, DeltasTransitionsCap,
		"returned transitions must be at or below the cap")
}

// TestDeltasTool_BelowCapNotTruncated: a small result set →
// Truncated=false, GroupsTotal == GroupsReturned.
func TestDeltasTool_BelowCapNotTruncated(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "pkg:npm/tiny", "tiny")
	seedDeltasSignals(t, s, e.ID, "stars", 3) // 3 obs → 2 transitions

	tool := &DeltasTool{Store: s}
	req := `{"target":"pkg:npm/tiny","range_start":"2026-01-01T00:00:00Z","range_end":"2030-12-31T23:59:59Z"}`
	resp := tool.Handle(context.Background(), json.RawMessage(req))
	require.Equal(t, "ok", resp.Status)

	payload := resp.Data.(*DeltasResponse)
	assert.False(t, payload.Truncated)
	assert.Equal(t, payload.GroupsTotal, payload.GroupsReturned)
}

// TestDeltasTool_SinceScoping: --since trims to observations at or
// after the timestamp.
func TestDeltasTool_SinceScoping(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "pkg:npm/since-test", "since-test")
	seedDeltasSignals(t, s, e.ID, "stars", 5)

	// Day index 3 → 2026-05-04. Scope from day index 2 onward keeps
	// observations 3..5 (indices 2..4) → 3 observations → 2 diffs.
	tool := &DeltasTool{Store: s}
	req := `{"target":"pkg:npm/since-test","since":"2026-05-03T00:00:00Z"}`
	resp := tool.Handle(context.Background(), json.RawMessage(req))
	require.Equal(t, "ok", resp.Status, "unexpected error: %+v", resp.Error)
	payload := resp.Data.(*DeltasResponse)
	require.Len(t, payload.Groups, 1)
	assert.Len(t, payload.Groups[0].Observations, 3)
	assert.Len(t, payload.Groups[0].PairDiffs, 2)
}

// TestDeltasTool_ReturnsRenderInputShape: response must decode as
// the RenderInput JSON shape (same as the CLI --json output). The
// MCP and CLI agree on wire format; agents written against one
// surface trivially parse the other.
func TestDeltasTool_ReturnsRenderInputShape(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "pkg:npm/shape-test", "shape-test")
	seedDeltasSignals(t, s, e.ID, "stars", 2)

	tool := &DeltasTool{Store: s}
	req := `{"target":"pkg:npm/shape-test","range_start":"2026-04-01T00:00:00Z","range_end":"2030-01-01T00:00:00Z"}`
	resp := tool.Handle(context.Background(), json.RawMessage(req))
	require.Equal(t, "ok", resp.Status)

	// Marshal/unmarshal cycle to confirm the wire shape — this is
	// what an MCP client will actually see.
	wire, err := json.Marshal(resp.Data)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(wire, &decoded))

	assert.Equal(t, "pkg:npm/shape-test", decoded["target"])
	assert.Contains(t, decoded, "window")
	assert.Contains(t, decoded, "groups")
	assert.Contains(t, decoded, "truncated")
	assert.Contains(t, decoded, "groups_total")
	assert.Contains(t, decoded, "groups_returned")

	// Window discriminator carries through.
	window, ok := decoded["window"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "range", window["kind"])
}

// Compile-time assertion: ensure DeltasTool satisfies mcp.Tool.
// Drives a build break if the interface changes underneath.
var _ mcp.Tool = (*DeltasTool)(nil)

// Sanity: ensure deltas.Computer is reachable from the test file
// to silence "imported and not used" if the production code reshape
// changes. The test file uses deltas.RenderInput too, indirectly.
var _ = deltas.New
