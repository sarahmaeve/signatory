package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// VersionPinSource looks up the version_pin_table compound signal
// for an entity. The source-evolution collector consumes this to
// anchor matrix rows to commit SHAs without touching proxy.golang.org
// directly — gopublish owns proxy access; source-evolution reads the
// signal gopublish emitted (design/coll7.md D3, Architecture B).
type VersionPinSource interface {
	VersionPinTable(ctx context.Context, entity *profile.Entity) (PinTable, error)
}

// PinTable mirrors gopublish's emitted version_pin_table value
// shape, with PublishedAt parsed into a time.Time so callers don't
// re-parse RFC3339 strings on every access.
//
// Source-of-truth for the JSON shape is gopublish/pintable.go's
// VersionPinTableValue. The two definitions are intentionally
// independent (loose coupling across the signal-emit / signal-
// consume boundary): if gopublish adds fields we don't read, we
// silently ignore them; if it changes a field name, our unmarshal
// surfaces the gap rather than introducing a binary-compat issue.
type PinTable struct {
	ModulePath            string       `json:"module_path"`
	VersionCountTotal     int          `json:"version_count_total"`
	VersionCountProcessed int          `json:"version_count_processed"`
	Pins                  []VersionPin `json:"pins"`
	MissingOriginVersions []string     `json:"missing_origin_versions"`
	FetchFailedVersions   []string     `json:"fetch_failed_versions"`
}

// VersionPin is one (version, sha, published_at) tuple. Source is
// always "proxy.golang.org" in v0.1 — see gopublish/pintable.go for
// the forward-compat rationale on retaining the field.
type VersionPin struct {
	Version     string    `json:"version"`
	SHA         string    `json:"sha"`
	Source      string    `json:"source"`
	PublishedAt time.Time `json:"published_at"`
}

// SignalStore is the narrow store interface VersionPinSource needs
// for fallback lookup. Defined locally rather than imported from
// internal/store so the source package stays loosely coupled —
// store.Store satisfies it implicitly via Go's structural interface
// satisfaction. Tests inject fakes implementing this single method.
type SignalStore interface {
	GetLatestSignals(ctx context.Context, entityID string) ([]profile.Signal, error)
}

// pinSourceImpl is the production VersionPinSource. It consults the
// in-run CollectionResult first (when the same analysis already ran
// gopublish and a merged result is available) and falls back to the
// signal store (a previous analysis's persisted pin table).
//
// Either source may be nil; an entirely-nil pin source returns
// ErrPinTableNotAvailable for every lookup. Production wiring
// (commit 16) decides which to pass; tests typically pass a
// hand-built CollectionResult or a fake SignalStore.
type pinSourceImpl struct {
	inRun *signal.CollectionResult
	store SignalStore
}

// NewPinSource constructs a VersionPinSource. Either argument may be
// nil — useful for tests that exercise one path at a time and for
// production wiring that may not have a store available yet.
func NewPinSource(inRun *signal.CollectionResult, store SignalStore) VersionPinSource {
	return &pinSourceImpl{inRun: inRun, store: store}
}

// VersionPinTable returns the pin table for the given entity, trying
// the in-run CollectionResult before the store. Both lookups are
// best-effort; ErrPinTableNotAvailable is returned only when both
// sources are exhausted without a hit.
func (p *pinSourceImpl) VersionPinTable(ctx context.Context, entity *profile.Entity) (PinTable, error) {
	if entity == nil {
		return PinTable{}, errors.New("VersionPinTable: nil entity")
	}

	// 1. In-run CollectionResult.
	if p.inRun != nil {
		pt, ok, err := pinTableFromSignals(p.inRun.Signals(), entity.ID)
		if err != nil {
			return PinTable{}, fmt.Errorf("read pin table from in-run result: %w", err)
		}
		if ok {
			return pt, nil
		}
	}

	// 2. Signal store fallback.
	if p.store != nil {
		signals, err := p.store.GetLatestSignals(ctx, entity.ID)
		if err != nil {
			return PinTable{}, fmt.Errorf("query store for pin table: %w", err)
		}
		pt, ok, err := pinTableFromSignals(signals, entity.ID)
		if err != nil {
			return PinTable{}, fmt.Errorf("read pin table from store: %w", err)
		}
		if ok {
			return pt, nil
		}
	}

	return PinTable{}, ErrPinTableNotAvailable
}

// pinTableFromSignals scans a slice of signals for the entity's
// version_pin_table and unmarshals it. Returns ok=false (and nil
// error) when no matching signal is present; returns an error only
// when a matching signal exists but its JSON value can't be parsed.
//
// entityID filtering matters because in-run CollectionResults can
// in principle hold signals for multiple entities (the orchestrator
// may merge results across collectors); store lookups are usually
// already entity-scoped but the filter is cheap and defensive.
//
// Skips absence records (Type prefixed "absence:") implicitly —
// CollectionResult.Signals() flattens absences but their Type
// values won't match "version_pin_table" exactly, so they fall
// through.
func pinTableFromSignals(signals []profile.Signal, entityID string) (PinTable, bool, error) {
	for _, sig := range signals {
		if sig.EntityID != entityID || sig.Type != "version_pin_table" {
			continue
		}
		var pt PinTable
		if err := json.Unmarshal(sig.Value, &pt); err != nil {
			return PinTable{}, false, fmt.Errorf("unmarshal version_pin_table value: %w", err)
		}
		return pt, true, nil
	}
	return PinTable{}, false, nil
}
