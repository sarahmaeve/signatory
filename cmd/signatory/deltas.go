package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/sarahmaeve/signatory/internal/deltas"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// DeltasCmd surfaces signal-value changes for a target over time.
// See design/deltas.md for the full spec — diff semantics, output
// shapes, and the Phase 1 / Phase 2 split.
type DeltasCmd struct {
	Target string `arg:"" help:"Target URI (pkg:npm/X, pkg:pypi/Y, repo:github/owner/repo, etc.)."`

	Since string `help:"Time-bounded view. Word shortcuts (yesterday, last-week, last-month), Go duration (2d, 12h, 30m), or RFC3339 timestamp. Default '24h' when no time flag is set."`
	Last  int    `help:"Show the most recent N observations per (type, source) group. Mutually exclusive with --since and --all."`
	All   bool   `help:"Show the full history. Mutually exclusive with --since and --last. Prompts for confirmation when more than 10 collection runs are present (use --yes to skip)."`

	Type   string `help:"Filter by signal type (e.g., trusted_publishing, version_unpublish_observed)."`
	Source string `help:"Filter by collector source (e.g., npm-registry, github, git)."`
	Group  string `help:"Filter by signal group (vitality, governance, publication, hygiene, criticality, identity)."`

	IncludeUnchanged bool `help:"Include signals with no changes in the window. Default behavior suppresses them."`
	JSON             bool `help:"Emit structured JSON instead of human-readable text."`
	Yes              bool `short:"y" help:"Skip the confirmation prompt for large --all expansions."`

	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
	Stdin  io.Reader `kong:"-"`
}

// Run executes the deltas query against the configured store and
// writes the rendered output (text or JSON) to cmd.Stdout. Warnings
// (e.g., large --all result sets) go to cmd.Stderr.
func (cmd *DeltasCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}

	if err := cmd.validateFlags(); err != nil {
		return err
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error not actionable

	// Resolve the target to an entity. Mirrors show-analyses /
	// posture / analyze: normalize the URI form, look it up by
	// canonical URI, surface a clean error when missing.
	canonicalURI := normalizeTargetForQuery(cmd.Target)
	entity, err := s.FindEntityByURI(ctx, canonicalURI)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("target %q not found in store — run `signatory analyze --refresh %s` first", cmd.Target, cmd.Target)
		}
		return fmt.Errorf("lookup target %q: %w", cmd.Target, err)
	}

	window, err := cmd.window()
	if err != nil {
		return err
	}

	// Query the full append-only signal history for the entity.
	// The store returns rows chronologically ascending — see
	// internal/store/sqlite.go GetSignals docstring.
	allSignals, err := s.GetSignals(ctx, entity.ID)
	if err != nil {
		return fmt.Errorf("query signals for %s: %w", canonicalURI, err)
	}

	filtered := applyFilters(allSignals, cmd)

	// --all bound-check: when the filtered set has more than
	// `allRunsPromptThreshold` distinct collection runs, ask the
	// user before scrolling through it. --yes (or -y) bypasses.
	// EOF / non-interactive callers default to "no" so scripts
	// don't hang.
	if cmd.All {
		runs := countRuns(filtered)
		proceed, err := confirmAllExpansion(cmd.Stderr, cmd.Stdin, runs, canonicalURI, cmd.Yes)
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}
	}
	groups := groupByTypeSource(filtered)
	groups = applyWindow(groups, window)
	deltaGroups := buildSignalDeltas(groups)

	in := deltas.RenderInput{
		Target: canonicalURI,
		Window: window,
		Groups: deltaGroups,
	}

	if cmd.JSON {
		return deltas.RenderJSON(cmd.Stdout, in)
	}
	return deltas.RenderText(cmd.Stdout, in, deltas.TextOpts{IncludeUnchanged: cmd.IncludeUnchanged})
}

// validateFlags enforces mutual exclusion of --since, --last, and
// --all. Kong doesn't have native group-mutex; we do it at Run time.
func (cmd *DeltasCmd) validateFlags() error {
	set := 0
	if cmd.Since != "" {
		set++
	}
	if cmd.Last > 0 {
		set++
	}
	if cmd.All {
		set++
	}
	if set > 1 {
		return NewUsageError(errors.New("--since, --last, and --all are mutually exclusive"))
	}
	return nil
}

// window resolves the (--since | --last | --all) trio into a
// deltas.TimeWindow. Default when none is set: --since 24h.
func (cmd *DeltasCmd) window() (deltas.TimeWindow, error) {
	if cmd.All {
		return deltas.TimeWindow{All: true}, nil
	}
	if cmd.Last > 0 {
		return deltas.TimeWindow{Last: cmd.Last}, nil
	}
	raw := cmd.Since
	if raw == "" {
		raw = "24h"
	}
	cutoff, err := parseTimeShorthand(raw)
	if err != nil {
		return deltas.TimeWindow{}, NewUsageError(fmt.Errorf("--since %q: %w", raw, err))
	}
	return deltas.TimeWindow{Cutoff: cutoff}, nil
}

// applyFilters drops signals whose type, source, or group doesn't
// match the user-supplied filter flags. Empty filter values match
// everything (no filter).
func applyFilters(signals []profile.Signal, cmd *DeltasCmd) []profile.Signal {
	if cmd.Type == "" && cmd.Source == "" && cmd.Group == "" {
		return signals
	}
	out := signals[:0]
	for _, sig := range signals {
		if cmd.Type != "" && sig.Type != cmd.Type {
			continue
		}
		if cmd.Source != "" && sig.Source != cmd.Source {
			continue
		}
		if cmd.Group != "" && string(sig.Group) != cmd.Group {
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
//   - Cutoff non-zero: drop observations with collected_at < cutoff
func applyWindow(g groupedSignals, w deltas.TimeWindow) groupedSignals {
	if w.All {
		return g
	}
	for key, sigs := range g.byKey {
		if w.Last > 0 {
			if len(sigs) > w.Last {
				g.byKey[key] = sigs[len(sigs)-w.Last:]
			}
			continue
		}
		// Cutoff: drop earlier observations.
		idx := 0
		for idx < len(sigs) && sigs[idx].CollectedAt.Before(w.Cutoff) {
			idx++
		}
		g.byKey[key] = sigs[idx:]
	}
	return g
}

// buildSignalDeltas converts the grouped signal slice into a sorted
// []deltas.SignalDelta with per-pair diffs computed via deltas.Diff.
func buildSignalDeltas(g groupedSignals) []deltas.SignalDelta {
	out := make([]deltas.SignalDelta, 0, len(g.byKey))
	for key, sigs := range g.byKey {
		if len(sigs) == 0 {
			continue
		}
		obs := make([]deltas.Observation, 0, len(sigs))
		for _, sig := range sigs {
			obs = append(obs, deltas.Observation{
				CollectedAt: sig.CollectedAt,
				Value:       decodeSignalValue(sig.Value),
			})
		}
		pairDiffs := make([]deltas.ValueDiff, 0, len(obs)-1)
		for i := 1; i < len(obs); i++ {
			pairDiffs = append(pairDiffs, deltas.Diff(obs[i-1].Value, obs[i].Value))
		}
		out = append(out, deltas.SignalDelta{
			Type:         key.Type,
			Source:       key.Source,
			SignalGroup:  string(sigs[0].Group),
			Observations: obs,
			PairDiffs:    pairDiffs,
		})
	}
	// Sort: (signal_group, type, source) ascending. Renderer also
	// sorts; we sort here so the slice has a stable order before
	// the renderer sees it. Two layers of sort doesn't hurt.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SignalGroup != out[j].SignalGroup {
			return out[i].SignalGroup < out[j].SignalGroup
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// decodeSignalValue parses a signal's stored JSON value into a
// map[string]any for the diff engine. Returns an empty map on
// invalid JSON rather than failing — the deltas verb is read-only
// and best-effort; a corrupted historical row shouldn't crash the
// verb.
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
