package summary

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ErrEntityNotFound is returned when the target URI doesn't resolve
// to any entity in the store. Distinct from "entity exists but has
// no posture/burn/analyses" — that's a zero-filled Summary, not an
// error.
var ErrEntityNotFound = errors.New("no entity matches target")

// AssemblerStore is the narrow Store subset the assembler needs.
// Defined as an interface here so tests can construct a fake
// without pulling in the full Store surface. Production code uses
// *store.SQLite, which satisfies this interface naturally.
type AssemblerStore interface {
	FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error)
	GetPostures(ctx context.Context, entityID string) ([]profile.Posture, error)
	GetBurn(ctx context.Context, entityID string) (*profile.Burn, error)
	ListAnalystOutputs(ctx context.Context, filter store.AnalystOutputFilter) ([]store.AnalystOutputSummary, error)
	SeverityCounts(ctx context.Context, outputID string) (SeverityCounts, error)
	ListRelatedURIs(ctx context.Context, entityID string) ([]string, error)
}

// Assembler composes a Summary for a single target. Separate from
// the Store interface because Summary is a presentation concern:
// the Assembler orchestrates multiple store calls, merges the
// results, and formats them into one compact view.
type Assembler struct {
	Store AssemblerStore
}

// New returns an Assembler backed by s. s is typically *store.SQLite
// but can be any type satisfying AssemblerStore (tests, future
// alternate backends).
func New(s AssemblerStore) *Assembler {
	return &Assembler{Store: s}
}

// Assemble returns the Summary for targetURI. Expects a canonical
// URI; callers resolve non-canonical inputs via profile.ResolveTarget
// before calling.
//
// Returns ErrEntityNotFound when the URI doesn't resolve to an
// entity in the store. Other errors surface verbatim — failed
// posture lookup, DB-closed, etc.
func (a *Assembler) Assemble(ctx context.Context, targetURI string) (*Summary, error) {
	entity, err := a.Store.FindEntityByURI(ctx, targetURI)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrEntityNotFound, targetURI)
		}
		return nil, fmt.Errorf("lookup entity: %w", err)
	}

	out := &Summary{
		CanonicalURI: entity.CanonicalURI,
		ShortName:    entity.ShortName,
		EntityType:   string(entity.Type),
		URL:          entity.URL,
	}

	// Posture: the "current" posture is the newest non-withdrawn
	// row. GetPostures returns active rows newest-first (per the
	// store contract), so index 0 is the active snapshot when the
	// entity has any posture history.
	postures, err := a.Store.GetPostures(ctx, entity.ID)
	if err != nil {
		return nil, fmt.Errorf("list postures: %w", err)
	}
	if len(postures) > 0 {
		latest := postures[0]
		out.Posture = &PostureSnapshot{
			Tier:      string(latest.Tier),
			Version:   latest.Version,
			Rationale: latest.Rationale,
			SetBy:     latest.SetBy,
			SetAt:     latest.SetAt,
		}
	}

	// Burn: GetBurn already filters withdrawn rows by default, so
	// ErrNotFound here means "no active burn." Any other error
	// propagates.
	burn, err := a.Store.GetBurn(ctx, entity.ID)
	switch {
	case err == nil:
		out.Burn = &BurnSnapshot{
			Reason:   burn.Reason,
			BurnedBy: burn.BurnedBy,
			BurnedAt: burn.BurnedAt,
		}
	case errors.Is(err, store.ErrNotFound):
		// No active burn — leave Burn nil.
	default:
		return nil, fmt.Errorf("lookup burn: %w", err)
	}

	// Analyses: M2's cross-URI walk surfaces analyses indexed under
	// this entity AND analyses indexed under another entity with
	// collected_from_entity_id pointing here.
	outputs, err := a.Store.ListAnalystOutputs(ctx, store.AnalystOutputFilter{
		EntityID: entity.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("list analyses: %w", err)
	}
	if len(outputs) > 0 {
		out.Analyses = make([]AnalysisRollup, 0, len(outputs))
		for _, ao := range outputs {
			sevCounts, err := a.Store.SeverityCounts(ctx, ao.OutputID)
			if err != nil {
				return nil, fmt.Errorf("severity counts for %s: %w", ao.OutputID, err)
			}
			ingestedAt, _ := time.Parse(time.RFC3339, ao.IngestedAt)
			out.Analyses = append(out.Analyses, AnalysisRollup{
				OutputID:              ao.OutputID,
				AnalystID:             ao.AnalystID,
				Model:                 ao.Model,
				Round:                 ao.Round,
				IngestedAt:            ingestedAt,
				TargetCommit:          ao.TargetCommit,
				CollectedFromURI:      ao.CollectedFromURI,
				ConclusionCount:       ao.ConclusionsCount,
				SeverityCounts:        sevCounts,
				PositiveAbsenceCount:  ao.PositiveAbsenceCount,
				ObservationCount:      ao.ObservationCount,
				MethodologyPatternCnt: ao.PatternCount,
			})
		}
	}

	// Related URIs: both directions of the collected_from link.
	// Dedupe against the entity's own URI (which might show up in
	// a self-link row) and sort for stable output.
	related, err := a.Store.ListRelatedURIs(ctx, entity.ID)
	if err != nil {
		return nil, fmt.Errorf("list related URIs: %w", err)
	}
	if len(related) > 0 {
		related = slices.DeleteFunc(related, func(s string) bool {
			return s == "" || s == entity.CanonicalURI
		})
		slices.Sort(related)
		related = slices.Compact(related)
		out.RelatedURIs = related
	}

	return out, nil
}
