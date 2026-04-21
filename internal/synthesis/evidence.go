// Package synthesis composes the structured evidence rollup that
// feeds the synthesist handoff body.
//
// Scope: agent-facing-contract M6b. Sibling to internal/summary — where
// Summary is a shallow counts-and-snapshots view suited to MCP
// response compactness, Evidence is the deep content view the
// synthesist needs to reason over: full conclusion verdicts and
// rationales, positive absences, observations, and the cross-URI
// hop context produced by M2's collected_from link.
//
// Prior synthesis outputs (analyst_id prefix "signatory-synthesis")
// are filtered out of the rollup: the synthesist must derive its
// judgment from analyst evidence, not from prior syntheses of the
// same target. This is the D9 cross-pollination prohibition enforced
// at the assembler layer so the handoff body can't accidentally
// surface a prior synthesis even if a caller forgot to fence the
// template.
package synthesis

import (
	"context"
	"errors"
	"fmt"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ErrEntityNotFound is returned when the target URI doesn't resolve
// to any entity in the store. Distinct from "entity exists but has
// no analyses" — that's a zero-filled Evidence with Analyses empty,
// not an error.
var ErrEntityNotFound = errors.New("no entity matches target")

// Evidence is the deep rollup the synthesist handoff body carries.
// Unlike summary.Summary (counts + snapshots), Evidence embeds the
// full analyst conclusions / positive absences / observations so the
// synthesist can reason across them without a second round-trip to
// the store.
type Evidence struct {
	// CanonicalURI is the entity's canonical URI — always populated.
	CanonicalURI string `json:"canonical_uri"`

	// ShortName is the human-readable identifier for display.
	ShortName string `json:"short_name"`

	// EntityType is the entity's kind (project, package, identity,
	// patch, org).
	EntityType string `json:"entity_type"`

	// URL is the clone URL / homepage when the entity maps to one,
	// empty otherwise.
	URL string `json:"url,omitempty"`

	// RelatedURIs names other canonical URIs linked via the M2
	// collected_from walk — both directions. Lets the synthesist
	// see the identity hops its analyses came from (e.g. a
	// pkg:npm/X target whose analyses were collected against
	// repo:github/Y).
	RelatedURIs []string `json:"related_uris,omitempty"`

	// Analyses carries one AnalystEvidence per non-synthesis
	// analyst output indexed under this target. Ordered
	// newest-ingested first, matching ListAnalystOutputs.
	// Synthesis outputs are filtered out at assembly time per D9.
	Analyses []AnalystEvidence `json:"analyses,omitempty"`
}

// AnalystEvidence is one analyst output expanded to its full content.
// Reuses the exchange types verbatim so the JSON serialization
// already handles citations, severity, methodology trace, and every
// other nested field that has a home in the v1 schema.
type AnalystEvidence struct {
	OutputID         string                       `json:"output_id"`
	AnalystID        string                       `json:"analyst_id"`
	Model            string                       `json:"model,omitempty"`
	PromptVersion    string                       `json:"prompt_version,omitempty"`
	Round            int                          `json:"round,omitempty"`
	InvokedAt        string                       `json:"invoked_at,omitempty"`
	IngestedAt       string                       `json:"ingested_at,omitempty"`
	TargetCommit     string                       `json:"target_commit,omitempty"`
	CollectedFromURI string                       `json:"collected_from_uri,omitempty"`
	RoundNotes       string                       `json:"round_notes,omitempty"`
	Conclusions      []exchange.Conclusion        `json:"conclusions,omitempty"`
	PositiveAbsences []exchange.PositiveAbsence   `json:"positive_absences,omitempty"`
	Observations     []exchange.Observation       `json:"observations,omitempty"`
	MethodologyTrace *exchange.MethodologyCatalog `json:"methodology_trace,omitempty"`
}

// AssemblerStore is the narrow Store subset the evidence assembler
// needs. Distinct from summary.AssemblerStore because Evidence
// requires the full AnalystOutput document (summary only needs
// counts). Tests pass a fake; production passes *store.SQLite.
type AssemblerStore interface {
	FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error)
	ListAnalystOutputs(ctx context.Context, filter store.AnalystOutputFilter) ([]store.AnalystOutputSummary, error)
	GetAnalystOutput(ctx context.Context, outputID string) (*exchange.AnalystOutput, error)
	ListRelatedURIs(ctx context.Context, entityID string) ([]string, error)
}

// Assembler composes an Evidence for a single target URI. Parallel
// to summary.Assembler in shape and constructor style.
type Assembler struct {
	Store AssemblerStore
}

// New returns an Assembler backed by s.
func New(s AssemblerStore) *Assembler {
	return &Assembler{Store: s}
}

// Assemble returns the Evidence for targetURI. Expects a canonical
// URI; callers resolve non-canonical inputs before calling.
//
// Walk order:
//  1. Resolve entity.
//  2. ListAnalystOutputs (cross-URI per M2).
//  3. For each non-synthesis row, GetAnalystOutput (full document)
//     and project into an AnalystEvidence.
//  4. ListRelatedURIs (M2 both-directions hop context).
//
// Synthesis rows (analyst_id prefix "signatory-synthesis") are
// skipped in step 3: the synthesist must not see prior syntheses of
// the same target. D9 cross-pollination prohibition enforced at the
// data layer.
func (a *Assembler) Assemble(ctx context.Context, targetURI string) (*Evidence, error) {
	entity, err := a.Store.FindEntityByURI(ctx, targetURI)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrEntityNotFound, targetURI)
		}
		return nil, fmt.Errorf("lookup entity: %w", err)
	}

	out := &Evidence{
		CanonicalURI: entity.CanonicalURI,
		ShortName:    entity.ShortName,
		EntityType:   string(entity.Type),
		URL:          entity.URL,
	}

	summaries, err := a.Store.ListAnalystOutputs(ctx, store.AnalystOutputFilter{
		EntityID: entity.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("list analyst outputs: %w", err)
	}

	for _, s := range summaries {
		if exchange.IsSynthesistRole(s.AnalystID) {
			// Synthesis outputs are filtered out of the evidence
			// rollup per D9 — prior syntheses don't anchor a new
			// synthesis.
			continue
		}
		full, err := a.Store.GetAnalystOutput(ctx, s.OutputID)
		if err != nil {
			return nil, fmt.Errorf("load analyst output %s: %w", s.OutputID, err)
		}
		out.Analyses = append(out.Analyses, AnalystEvidence{
			OutputID:         s.OutputID,
			AnalystID:        full.Attribution.AnalystID,
			Model:            full.Attribution.Model,
			PromptVersion:    full.Attribution.PromptVersion,
			Round:            full.Attribution.Round,
			InvokedAt:        full.Attribution.InvokedAt,
			IngestedAt:       s.IngestedAt,
			TargetCommit:     full.TargetCommit,
			CollectedFromURI: s.CollectedFromURI,
			RoundNotes:       full.RoundNotes,
			Conclusions:      full.Conclusions,
			PositiveAbsences: full.PositiveAbsences,
			Observations:     full.Observations,
			MethodologyTrace: full.MethodologyTrace,
		})
	}

	related, err := a.Store.ListRelatedURIs(ctx, entity.ID)
	if err != nil {
		return nil, fmt.Errorf("list related URIs: %w", err)
	}
	// Exclude the entity's own URI in case the store surfaces it as
	// a self-link, and keep the order the store returned them in —
	// unlike Summary we don't re-sort, because analyst-facing content
	// benefits from stable ordering across runs for diff-ability but
	// doesn't need a cosmetic sort.
	for _, uri := range related {
		if uri == "" || uri == entity.CanonicalURI {
			continue
		}
		out.RelatedURIs = append(out.RelatedURIs, uri)
	}

	return out, nil
}
