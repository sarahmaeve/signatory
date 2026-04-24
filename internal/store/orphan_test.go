package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// TestOrphan_AppendResolution_EntityID is the Phase 3 regression
// guard for the orphan-prevention audit (design/orphanage.md).
//
// Phase 2 finding: signal_resolutions.entity_id is the sole
// entity_id column in the schema without REFERENCES entities(id).
// AppendResolution's only validation is a non-empty-string check
// on the value; no lookup against entities(id) occurs before the
// INSERT. Combined with the table's append-only trigger, any
// orphan that lands is permanent.
//
// This test exercises the real (*SQLite).AppendResolution method
// — not raw SQL — because that's the surface every future caller
// (CLI command, MCP handler, direct library user) invokes. The
// kept/superseded signal FKs ARE enforced today, so the test
// seeds real signals for those columns, isolating the
// orphan-entity case.
//
// Assertion shape: the post-fix behavior is what we're testing
// FOR. An AppendResolution call with an entity_id that doesn't
// exist in entities must return an error, and that error must
// identify as ErrOrphanedEntity via errors.Is so programmatic
// callers (ingest paths, dry-run checks) can distinguish this
// rejection from other failures.
//
// Skipped during Phase 3 because the fix lands in Phase 5.
// Running this test today — by commenting out the t.Skip —
// produces the failure documented in design/orphanage.md
// §"Phase 3 findings". The Phase 5 commit removes the t.Skip,
// at which point this test passes through the fix's validation
// + FK enforcement.
func TestOrphan_AppendResolution_EntityID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	// Seed a legitimate parent entity. This entity's ID is NOT
	// the one the orphan resolution will reference — it exists
	// only to own the seeded signals below, which satisfy the
	// kept/superseded FKs that ARE enforced.
	parent := testEntity("ent-parent", "pkg:npm/parent-for-orphan-test", "parent-for-orphan-test", now)
	require.NoError(t, s.PutEntity(ctx, parent))

	// Seed two signals for parent. These satisfy
	// signal_resolutions.kept_signal_id and
	// signal_resolutions.superseded_signal_id, which carry
	// REFERENCES signals(id). Without them, the test's INSERT
	// would fail on those FKs and mask the entity_id bug we're
	// isolating.
	keptSignal := profile.Signal{
		ID:                "sig-kept",
		EntityID:          parent.ID,
		Type:              "stars",
		Group:             profile.SignalGroupCriticality,
		Source:            "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{"count":1}`),
		CollectedAt:       now,
		ExpiresAt:         now.Add(24 * time.Hour),
	}
	supersededSignal := profile.Signal{
		ID:                "sig-superseded",
		EntityID:          parent.ID,
		Type:              "stars",
		Group:             profile.SignalGroupCriticality,
		Source:            "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{"count":2}`),
		CollectedAt:       now,
		ExpiresAt:         now.Add(24 * time.Hour),
	}
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{keptSignal, supersededSignal}))

	// The orphan: a resolution whose entity_id names nothing in
	// entities. The literal value is chosen to be obviously-fake
	// so a future reader grepping the store sees the intent.
	const ghostEntityID = "ghost-entity-id-99999"
	orphan := &profile.SignalResolution{
		ID:                 "orphan-res-1",
		EntityID:           ghostEntityID,
		SignalType:         "stars",
		KeptSignalID:       keptSignal.ID,
		SupersededSignalID: supersededSignal.ID,
		Action:             "keep_previous",
		ResolvedBy:         "team:test",
		ResolvedAt:         now,
	}

	err := s.AppendResolution(ctx, orphan)

	require.Error(t, err,
		"AppendResolution must reject a resolution whose entity_id "+
			"does not exist in the entities table — the schema's "+
			"missing REFERENCES entities(id) plus the absent Go-side "+
			"validation produces a silently-landed orphan today; the "+
			"Phase 5 fix closes both halves.")
	assert.ErrorIs(t, err, ErrOrphanedEntity,
		"rejection must be a typed error so ingest paths, dry-run "+
			"checks, and other programmatic callers can distinguish "+
			"orphan-rejection from other write failures without "+
			"string-matching on the error message.")

	// Defense-in-depth observation: even if the Go-side
	// validation were bypassed, the Phase 5 migration's
	// REFERENCES entities(id) would reject the INSERT at the
	// SQLite layer. So post-fix, the orphan row must NOT be
	// observable — regardless of which defense layer fired.
	var count int
	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signal_resolutions WHERE entity_id = ?`,
		ghostEntityID)
	require.NoError(t, row.Scan(&count))
	assert.Equal(t, 0, count,
		"rejected orphan must not have landed in signal_resolutions; "+
			"non-zero count here means the write path returned an "+
			"error but wrote the row anyway, which would indicate a "+
			"transaction-management bug in the fix.")
}
