package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/sarahmaeve/signatory/internal/deltas"
	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// DeltasTransitionsCap is the soft upper bound on the total number
// of pair-diff transitions a single signatory_deltas response will
// carry. When the computed result exceeds this cap, groups are
// dropped (in stable sort order, latest-by-name first) until the
// kept set fits under the cap. The response sets truncated=true so
// the caller can narrow the window and retry.
//
// 200 was chosen as the "agent context budget" floor: a typical
// transition serializes to ~200-500 bytes of JSON, so 200
// transitions ≈ 40-100 KB of payload — large but not catastrophic
// for an LLM context. The CLI's --all prompt fires at 10 runs;
// the MCP cap is intentionally more generous (agents handle
// structured JSON better than humans handle scrolled text) but
// still bounded.
const DeltasTransitionsCap = 200

// DeltasTool implements signatory_deltas — the time-series
// "what changed" companion to signatory_summary (current rollup)
// and signatory_signals (raw snapshot).
//
// Designed to answer agent queries like "did this package's
// signals drift since Tuesday?" or "show me the transitions for
// pkg:npm/X in the last week" without forcing the agent to
// reconstruct the history from raw signatory_signals output.
//
// Wire shape mirrors the CLI's --json output (deltas.RenderInput)
// with three meta fields appended for truncation reporting:
// truncated, groups_total, groups_returned. See DeltasResponse.
type DeltasTool struct {
	Store store.Store
}

func (t *DeltasTool) Name() string { return "signatory_deltas" }

func (t *DeltasTool) Description() string {
	return "USE THIS when the user asks 'what changed for X recently?', 'has this drifted since <time>?', 'show transitions for X'. Returns per-signal-type transitions with field-level before/after diffs. Complements signatory_signals (raw snapshot at one point in time) and signatory_summary (current rollup); use those when the user wants a single point-in-time view, not change over time. Requires a time scope (since OR last OR range_start+range_end) — there is no implicit 'all history' default. Caps total transitions at " + fmt.Sprint(DeltasTransitionsCap) + "; the response's truncated flag tells you to narrow the window."
}

func (t *DeltasTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target": {"type": "string", "description": "Canonical URI, URL, or owner/repo shorthand."},
			"since": {"type": "string", "description": "RFC3339 timestamp. Returns observations at or after this time. Mutually exclusive with last and range_start/range_end."},
			"last": {"type": "integer", "minimum": 1, "description": "Most recent N observations per (type, source) group. Mutually exclusive with since and range_start/range_end."},
			"range_start": {"type": "string", "description": "RFC3339 timestamp. Lower bound (inclusive). Must be paired with range_end. Mutually exclusive with since and last."},
			"range_end": {"type": "string", "description": "RFC3339 timestamp. Upper bound (inclusive). Must be paired with range_start. Mutually exclusive with since and last."},
			"type": {"type": "string", "description": "Filter by signal type (e.g., trusted_publishing, version_unpublish_observed)."},
			"source": {"type": "string", "description": "Filter by collector source (e.g., npm-registry, github, git)."},
			"group": {"type": "string", "description": "Filter by signal group (vitality, governance, publication, hygiene, criticality, identity)."}
		},
		"required": ["target"],
		"additionalProperties": false
	}`)
}

// deltasInput is the typed input for signatory_deltas. DisallowUnknownFields
// surfaces typos at decode time.
type deltasInput struct {
	Target     string `json:"target"`
	Since      string `json:"since,omitempty"`
	Last       int    `json:"last,omitempty"`
	RangeStart string `json:"range_start,omitempty"`
	RangeEnd   string `json:"range_end,omitempty"`
	Type       string `json:"type,omitempty"`
	Source     string `json:"source,omitempty"`
	Group      string `json:"group,omitempty"`
}

// DeltasResponse is the wire shape of the tool's response: a
// deltas.RenderInput (inlined) plus three truncation-meta fields.
// Inlined rather than nested so the JSON layout matches the CLI's
// --json output, with only the truncation fields appended.
type DeltasResponse struct {
	Target string               `json:"target"`
	Window deltas.TimeWindow    `json:"window"`
	Groups []deltas.SignalDelta `json:"groups"`

	// Truncated is true when the computed result exceeded
	// DeltasTransitionsCap and groups were dropped.
	Truncated bool `json:"truncated"`

	// GroupsTotal is the count of groups computed BEFORE
	// truncation. GroupsReturned is what's in the Groups slice.
	GroupsTotal    int `json:"groups_total"`
	GroupsReturned int `json:"groups_returned"`
}

func (t *DeltasTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in deltasInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}

	if in.Target == "" {
		return mcp.Err(mcp.CodeSchemaViolation, "target is required", nil)
	}

	window, err := buildWindow(in)
	if err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}

	computer := deltas.New(t.Store)
	result, err := computer.Compute(ctx, deltas.Params{
		Target: in.Target,
		Window: window,
		Type:   in.Type,
		Source: in.Source,
		Group:  in.Group,
	})
	if err != nil {
		if errors.Is(err, deltas.ErrEntityNotFound) {
			return mcp.Err(mcp.CodeNotFound, err.Error(),
				map[string]string{"target": in.Target})
		}
		return mcp.Err(mcp.CodeInternalError, "compute deltas: "+err.Error(), nil)
	}

	response := assembleResponse(result)
	return mcp.OK(response)
}

// buildWindow translates the input's time params into a
// deltas.TimeWindow, enforcing exclusivity and pairing. An empty
// time scope is rejected — agents must be explicit; there's no
// implicit 24h default like the CLI offers.
func buildWindow(in deltasInput) (deltas.TimeWindow, error) {
	hasSince := in.Since != ""
	hasLast := in.Last > 0
	hasRangeStart := in.RangeStart != ""
	hasRangeEnd := in.RangeEnd != ""
	hasRange := hasRangeStart || hasRangeEnd

	setCount := 0
	if hasSince {
		setCount++
	}
	if hasLast {
		setCount++
	}
	if hasRange {
		setCount++
	}
	if setCount > 1 {
		return deltas.TimeWindow{}, errors.New("since, last, and range_start/range_end are mutually exclusive — set at most one")
	}
	if setCount == 0 {
		return deltas.TimeWindow{}, errors.New("must specify one of since (RFC3339), last (int), or range_start+range_end (RFC3339 pair) — there is no implicit window default")
	}

	switch {
	case hasSince:
		cutoff, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return deltas.TimeWindow{}, fmt.Errorf("since: not a valid RFC3339 timestamp: %w", err)
		}
		return deltas.TimeWindow{Cutoff: cutoff.UTC()}, nil

	case hasLast:
		return deltas.TimeWindow{Last: in.Last}, nil

	case hasRange:
		if !hasRangeStart || !hasRangeEnd {
			return deltas.TimeWindow{}, errors.New("range_start and range_end must be set together")
		}
		start, err := time.Parse(time.RFC3339, in.RangeStart)
		if err != nil {
			return deltas.TimeWindow{}, fmt.Errorf("range_start: not a valid RFC3339 timestamp: %w", err)
		}
		end, err := time.Parse(time.RFC3339, in.RangeEnd)
		if err != nil {
			return deltas.TimeWindow{}, fmt.Errorf("range_end: not a valid RFC3339 timestamp: %w", err)
		}
		if start.After(end) {
			return deltas.TimeWindow{}, fmt.Errorf(
				"range_start (%s) is after range_end (%s)",
				start.Format(time.RFC3339), end.Format(time.RFC3339))
		}
		return deltas.TimeWindow{RangeStart: start.UTC(), RangeEnd: end.UTC()}, nil
	}

	// Unreachable — the setCount check above covers all combinations.
	return deltas.TimeWindow{}, errors.New("internal: window builder reached unreachable branch")
}

// assembleResponse builds a DeltasResponse from a computed
// RenderInput, applying the transitions cap and recording
// truncation metadata.
//
// Truncation strategy: sort groups deterministically (matches
// renderer order), then walk left-to-right, accumulating
// transition counts. Stop including groups once adding the next
// group would push past DeltasTransitionsCap. The truncated flag
// fires when any groups were dropped, even if the kept set is at
// exactly the cap.
func assembleResponse(in deltas.RenderInput) *DeltasResponse {
	// Groups already arrive sorted by (signal_group, type, source)
	// from buildSignalDeltas; double-check here so the truncation
	// is deterministic regardless of upstream changes.
	sort.SliceStable(in.Groups, func(i, j int) bool {
		if in.Groups[i].SignalGroup != in.Groups[j].SignalGroup {
			return in.Groups[i].SignalGroup < in.Groups[j].SignalGroup
		}
		if in.Groups[i].Type != in.Groups[j].Type {
			return in.Groups[i].Type < in.Groups[j].Type
		}
		return in.Groups[i].Source < in.Groups[j].Source
	})

	total := 0
	for _, g := range in.Groups {
		total += len(g.PairDiffs)
	}

	groupsTotal := len(in.Groups)
	truncated := false

	if total > DeltasTransitionsCap {
		sum := 0
		keep := 0
		for ; keep < len(in.Groups); keep++ {
			next := sum + len(in.Groups[keep].PairDiffs)
			if next > DeltasTransitionsCap {
				break
			}
			sum = next
		}
		in.Groups = in.Groups[:keep]
		truncated = true
	}

	return &DeltasResponse{
		Target:         in.Target,
		Window:         in.Window,
		Groups:         in.Groups,
		Truncated:      truncated,
		GroupsTotal:    groupsTotal,
		GroupsReturned: len(in.Groups),
	}
}

// Compile-time assertion: DeltasTool implements mcp.Tool.
var _ mcp.Tool = (*DeltasTool)(nil)
