package deltas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ErrEntityNotFound is returned by Compute when the target URI
// doesn't resolve to any entity in the store. Mirrors
// summary.ErrEntityNotFound's role: lets callers (CLI verb, MCP
// handler) distinguish "we asked but nothing was there" from
// "the store call itself failed."
var ErrEntityNotFound = errors.New("no entity matches target")

// ComputerStore is the narrow Store subset Compute needs. Defined
// as an interface so tests can construct a fake without pulling in
// the full Store surface; production passes *store.SQLite which
// satisfies this naturally. Mirrors summary.AssemblerStore.
type ComputerStore interface {
	FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error)
	GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error)
}

// Params describes the inputs to Compute. Callers (CLI verb, MCP
// handler, future programmatic users) populate this and call
// Computer.Compute to get a fully-built RenderInput.
//
// Target accepts any form profile.ResolveTarget understands —
// canonical URIs, URLs, owner/repo shorthand. Compute resolves it
// to a canonical URI before lookup.
//
// Window is the resolved time bound (see TimeWindow). The CLI verb
// builds this from --since / --last / --range / --all flags; the
// MCP handler builds it from JSON params.
//
// Type/Source/Group are optional filters dropped on the full signal
// list before windowing. Empty strings match everything.
type Params struct {
	Target string
	Window TimeWindow
	Type   string
	Source string
	Group  string
}

// Computer holds the store wiring for Compute. Cheap to construct;
// stateless between calls. Mirrors summary.Assembler.
type Computer struct {
	Store ComputerStore
}

// New returns a Computer backed by s. s is typically *store.SQLite
// in production; tests pass a fake satisfying ComputerStore.
func New(s ComputerStore) *Computer {
	return &Computer{Store: s}
}

// Compute resolves the target, queries the store for its signal
// history, applies the filters and time window, and returns the
// rendered-shape input. The returned RenderInput is consumable
// directly by RenderText or RenderJSON.
//
// Returns ErrEntityNotFound (wrapped with the canonical URI in the
// message) when the target doesn't resolve to a known entity.
// Other store errors propagate wrapped.
func (c *Computer) Compute(ctx context.Context, p Params) (RenderInput, error) {
	canonicalURI := resolveTarget(p.Target)

	entity, err := c.Store.FindEntityByURI(ctx, canonicalURI)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return RenderInput{}, fmt.Errorf("%w: %s", ErrEntityNotFound, canonicalURI)
		}
		return RenderInput{}, fmt.Errorf("lookup target %q: %w", p.Target, err)
	}

	allSignals, err := c.Store.GetSignals(ctx, entity.ID)
	if err != nil {
		return RenderInput{}, fmt.Errorf("query signals for %s: %w", canonicalURI, err)
	}

	filtered := applyFilters(allSignals, p.Type, p.Source, p.Group)
	groups := groupByTypeSource(filtered)
	groups = applyWindow(groups, p.Window)
	deltaGroups := buildSignalDeltas(groups)

	return RenderInput{
		Target: canonicalURI,
		Window: p.Window,
		Groups: deltaGroups,
	}, nil
}

// resolveTarget mirrors cmd/signatory's normalizeTargetForQuery:
// pass empty input through unchanged; ask profile.ResolveTarget for
// the canonical form; on parse failure pass the raw target through
// so the store returns ErrNotFound with the user's input echoed
// back in the error.
func resolveTarget(target string) string {
	if target == "" {
		return ""
	}
	resolved, err := profile.ResolveTarget(target)
	if err != nil {
		return target
	}
	return resolved.CanonicalURI
}

// applyFilters drops signals whose type, source, or group doesn't
// match the supplied filter values. Empty filter values match
// everything (no filter).
func applyFilters(signals []profile.Signal, typeFilter, sourceFilter, groupFilter string) []profile.Signal {
	if typeFilter == "" && sourceFilter == "" && groupFilter == "" {
		return signals
	}
	out := signals[:0]
	for _, sig := range signals {
		if typeFilter != "" && sig.Type != typeFilter {
			continue
		}
		if sourceFilter != "" && sig.Source != sourceFilter {
			continue
		}
		if groupFilter != "" && string(sig.Group) != groupFilter {
			continue
		}
		out = append(out, sig)
	}
	return out
}

// typeSourceKey is the grouping key for signals.
type typeSourceKey struct {
	Type   string
	Source string
}

// groupedSignals holds signals keyed by (type, source), each list
// already sorted chronologically (GetSignals returns ascending).
type groupedSignals struct {
	byKey map[typeSourceKey][]profile.Signal
}

// groupByTypeSource partitions signals into (type, source) bins.
// Preserves chronological order within each bin (relies on
// GetSignals' ASC ordering).
func groupByTypeSource(signals []profile.Signal) groupedSignals {
	out := groupedSignals{byKey: map[typeSourceKey][]profile.Signal{}}
	for _, sig := range signals {
		key := typeSourceKey{Type: sig.Type, Source: sig.Source}
		out.byKey[key] = append(out.byKey[key], sig)
	}
	return out
}

// applyWindow trims each group according to the TimeWindow:
//
//   - All: no trimming
//   - Last>0: keep the most recent Last observations per group
//   - Range: keep observations in [RangeStart, RangeEnd] inclusive
//   - Cutoff non-zero: drop observations with collected_at < cutoff
func applyWindow(g groupedSignals, w TimeWindow) groupedSignals {
	if w.All {
		return g
	}
	rangeMode := !w.RangeStart.IsZero() && !w.RangeEnd.IsZero()
	for key, sigs := range g.byKey {
		switch {
		case w.Last > 0:
			if len(sigs) > w.Last {
				g.byKey[key] = sigs[len(sigs)-w.Last:]
			}
		case rangeMode:
			kept := sigs[:0]
			for _, sig := range sigs {
				if sig.CollectedAt.Before(w.RangeStart) || sig.CollectedAt.After(w.RangeEnd) {
					continue
				}
				kept = append(kept, sig)
			}
			g.byKey[key] = kept
		default:
			// Cutoff: drop earlier observations.
			idx := 0
			for idx < len(sigs) && sigs[idx].CollectedAt.Before(w.Cutoff) {
				idx++
			}
			g.byKey[key] = sigs[idx:]
		}
	}
	return g
}

// buildSignalDeltas converts the grouped signal slice into a sorted
// []SignalDelta with per-pair diffs computed via Diff.
func buildSignalDeltas(g groupedSignals) []SignalDelta {
	out := make([]SignalDelta, 0, len(g.byKey))
	for key, sigs := range g.byKey {
		if len(sigs) == 0 {
			continue
		}
		obs := make([]Observation, 0, len(sigs))
		for _, sig := range sigs {
			obs = append(obs, Observation{
				CollectedAt: sig.CollectedAt,
				Value:       decodeSignalValue(sig.Value),
			})
		}
		pairDiffs := make([]ValueDiff, 0, len(obs)-1)
		for i := 1; i < len(obs); i++ {
			pairDiffs = append(pairDiffs, Diff(obs[i-1].Value, obs[i].Value))
		}
		out = append(out, SignalDelta{
			Type:         key.Type,
			Source:       key.Source,
			SignalGroup:  string(sigs[0].Group),
			Observations: obs,
			PairDiffs:    pairDiffs,
		})
	}
	return SortGroups(out)
}

// decodeSignalValue parses a signal's stored JSON value into a
// map[string]any for the diff engine. Returns an empty map on
// invalid JSON rather than failing — the deltas pipeline is
// read-only and best-effort; a corrupted historical row shouldn't
// crash the verb or the MCP tool.
func decodeSignalValue(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return map[string]any{}
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return map[string]any{}
	}
	return v
}
