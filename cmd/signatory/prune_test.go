package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// pruneAnalystOutput builds a minimally-valid analyst output for
// prune test fixtures. Distinct from ingestSynthesisForAccept's
// helper so the prune tests don't share state with posture accept.
func pruneAnalystOutput(target, analystID, invokedAt string) *exchange.AnalystOutput {
	lineStart := 1
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: analystID,
			Model:     "test-model",
			InvokedAt: invokedAt,
		},
		Target: target,
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "c",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: &lineStart},
				},
			},
		},
	}
}

// ingestForPrune is a one-liner that commits an analyst_output and
// returns the entity ID for the test to operate against.
func ingestForPrune(t *testing.T, g *Globals, target, analystID, invokedAt string) string {
	t.Helper()
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	res, err := s.IngestAnalystOutput(ctx, pruneAnalystOutput(target, analystID, invokedAt), "test")
	require.NoError(t, err)
	return res.EntityID
}

// TestPruneEntity_DryRunNoWrites: without --yes, the command must
// NOT mutate the store. The plan is printed; the data stays.
// Regression: operator runs `signatory prune entity X` to inspect
// the plan; if that command deletes, they lose data without
// confirming.
func TestPruneEntity_DryRunNoWrites(t *testing.T) {
	g := newTestGlobals(t)
	entityID := ingestForPrune(t, g, "pkg:npm/dry-run-test", "external-sec-v1", "2026-04-23T09:00:00Z")

	cmd := &PruneEntityCmd{
		Target: "pkg:npm/dry-run-test",
		// Yes deliberately omitted.
	}
	require.NoError(t, cmd.Run(g), "dry-run must exit cleanly")

	// Entity must still exist.
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/dry-run-test")
	require.NoError(t, err, "dry-run must NOT delete the entity")
	assert.Equal(t, entityID, entity.ID)
}

// TestPruneEntity_YesAppliesDelete: with --yes, the entity and
// every child row goes away.
func TestPruneEntity_YesAppliesDelete(t *testing.T) {
	g := newTestGlobals(t)
	_ = ingestForPrune(t, g, "pkg:npm/apply-test", "external-sec-v1", "2026-04-23T10:00:00Z")

	cmd := &PruneEntityCmd{
		Target: "pkg:npm/apply-test",
		Yes:    true,
	}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.FindEntityByURI(ctx, "pkg:npm/apply-test")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"--yes must delete the entity")
}

// TestPruneEntity_UnknownTarget errors cleanly with a message that
// names the target and hints at recovery. Important for the skill's
// error-surfacing UX — "no entity matches" is a user-fixable state,
// not a crash.
func TestPruneEntity_UnknownTarget(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &PruneEntityCmd{Target: "pkg:npm/does-not-exist", Yes: true}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no entity matches")
	assert.Contains(t, err.Error(), "pkg:npm/does-not-exist",
		"error must name the target so the operator sees which argument failed")
}

// TestPruneEntity_ByUUID: operator can pass an entity UUID
// directly, bypassing the URI parser. Useful for cleanup of rows
// with malformed canonical URIs (if any exist post-v10).
func TestPruneEntity_ByUUID(t *testing.T) {
	g := newTestGlobals(t)
	entityID := ingestForPrune(t, g, "pkg:npm/by-uuid-test", "external-sec-v1", "2026-04-23T11:00:00Z")

	cmd := &PruneEntityCmd{Target: entityID, Yes: true}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.GetEntity(ctx, entityID)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"UUID-targeted prune must delete the entity")
}

// TestPruneVersioned_NoVersionedRows prints a helpful message and
// exits cleanly when the store is already clean. Important: no
// error — "nothing to do" is a valid state, not a failure.
func TestPruneVersioned_NoVersionedRows(t *testing.T) {
	g := newTestGlobals(t)
	// Ingest an unversioned target so the entity table isn't empty.
	_ = ingestForPrune(t, g, "pkg:npm/already-clean", "external-sec-v1", "2026-04-23T12:00:00Z")

	cmd := &PruneVersionedCmd{Yes: true}
	require.NoError(t, cmd.Run(g), "clean store must produce no-op success, not an error")
}

// TestPruneVersioned_DeletesLegacyRows: a pre-v10-shaped versioned
// entity (created manually to simulate dogfood data) gets removed
// by `prune versioned`. Scoped npm packages in the same store are
// preserved.
func TestPruneVersioned_DeletesLegacyRows(t *testing.T) {
	g := newTestGlobals(t)
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)

	// Simulate a pre-v10 versioned entity by injecting one at the
	// store level — v10 ingest would strip @V, so we can't produce
	// this shape via IngestAnalystOutput anymore.
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID:           "legacy-pkg-1",
		CanonicalURI: "pkg:npm/legacy-versioned@9.9.9",
		Type:         profile.EntityPackage,
		ShortName:    "legacy-versioned@9.9.9",
	}))

	// A scoped npm package co-exists — must NOT be touched.
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID:           "scoped-1",
		CanonicalURI: "pkg:npm/@types/node",
		Type:         profile.EntityPackage,
		ShortName:    "node",
	}))
	s.Close() //nolint:errcheck // test cleanup

	cmd := &PruneVersionedCmd{Yes: true}
	require.NoError(t, cmd.Run(g))

	s, err = g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	_, err = s.FindEntityByURI(ctx, "pkg:npm/legacy-versioned@9.9.9")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"versioned entity must be pruned")
	_, err = s.FindEntityByURI(ctx, "pkg:npm/@types/node")
	require.NoError(t, err,
		"scoped npm package (non-versioned, scope `@`) must survive `prune versioned`")
}
