package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Path B end-to-end: with Path A populating owner entities and the
// cascade resolver in place, burning an operator's identity must
// surface as a "burned via owner" rendering on every repo that
// owner publishes — without per-repo manual burn add.
//
// This is the BufferZone case from countercampaign.md, finally
// closed end-to-end:
//
//   1. analyze --refresh repo:github/bufferzonecorp/grpc-client
//      → minted identity:github/bufferzonecorp + owner_profile signal
//   2. burn add identity:github/bufferzonecorp
//      → burn row on the identity entity
//   3. summary repo:github/bufferzonecorp/grpc-client
//      → "BURNED ... via owner identity:github/bufferzonecorp"
//
// Driven through the actual CLI surface via runCLI (same helper
// path_d_cli_test.go uses) so exit codes, stdout/stderr placement,
// and the kong parsing layer are all exercised.
//
// burn list (the audit surface) MUST keep showing the literal burn
// on the identity, NOT the cascaded one on the repo. That's pinned
// here too — countercampaign.md §7.7 split: display callers move to
// EffectiveBurn, audit callers stay on GetBurn/ListBurns.

// seedCascadeScenario sets up the standard "operator burned, repo
// they published is queried" state in a fresh DB:
//
//   - identity:github/bufferzonecorp entity exists and is burned
//   - repo:github/bufferzonecorp/grpc-client entity exists
//   - owner_profile signal on the repo points at bufferzonecorp
//
// Returns the DB path so tests can drive runCLI against it.
func seedCascadeScenario(t *testing.T) string {
	t.Helper()

	dbPath := newCLITestDB(t)

	// Mint the identity entity via burn add.
	add := runCLI(t, dbPath,
		"burn", "add", "identity:github/bufferzonecorp",
		"--reason", "campaign-shaped account, 17 throwaway repos",
	)
	require.Equal(t, 0, add.exitCode, "burn add must succeed; stderr=%q", add.stderr)

	// Open the store directly to seed the repo entity + signal.
	// AnalyzeCmd would be the production path but it requires a
	// mocked github API; this test focuses on the cascade side, so
	// we synthesize the owner_profile signal directly.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	// The Path-A integration tests already cover the "analyze run
	// mints + emits the signal" case end-to-end. Here we want the
	// post-Path-A cascade behaviour, so we put the row + signal in
	// directly via the public store API.
	now := time.Now().UTC().Truncate(time.Second)
	repo := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: "repo:github/bufferzonecorp/grpc-client",
		Type:         profile.EntityProject,
		ShortName:    "bufferzonecorp/grpc-client",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, s.PutEntity(t.Context(), repo))

	ownerProfile, err := json.Marshal(map[string]any{
		"login": "bufferzonecorp",
		"type":  "User",
	})
	require.NoError(t, err)
	sig := profile.Signal{
		ID:                profile.NewEntityID(),
		EntityID:          repo.ID,
		Type:              "owner_profile",
		Group:             profile.SignalGroupGovernance,
		Source:            "github",
		ForgeryResistance: profile.ForgeryVeryHigh,
		Value:             ownerProfile,
		CollectedAt:       now,
		ExpiresAt:         now.Add(time.Hour),
	}
	require.NoError(t, s.AppendSignals(t.Context(), []profile.Signal{sig}))

	return dbPath
}

// TestPathB_CLI_Summary_Repo_ShowsCascadeFromBurnedOwner is the
// load-bearing end-to-end test for Path B. With the operator burned
// and Path A's owner_profile signal in place, summary on a repo the
// operator publishes must show:
//
//   - the BURNED marker (the cascade fired),
//   - the cascaded reason in the rendered output,
//   - some indication the burn cascaded from the owner (so the
//     user knows which ledger entry caused the degradation).
func TestPathB_CLI_Summary_Repo_ShowsCascadeFromBurnedOwner(t *testing.T) {
	dbPath := seedCascadeScenario(t)

	r := runCLI(t, dbPath,
		"summary", "repo:github/bufferzonecorp/grpc-client",
	)

	require.Equal(t, 0, r.exitCode, "summary must exit 0; stderr=%q", r.stderr)
	assert.Contains(t, r.stdout, "BURNED",
		"summary must surface the burn marker even though the burn is on the owner, not the repo itself")
	assert.Contains(t, r.stdout, "campaign-shaped",
		"the cascaded burn reason must surface in the rendered output")
	assert.Contains(t, r.stdout, "identity:github/bufferzonecorp",
		"the rendering must name the cascade source so users can trace which ledger entry caused the degradation")
}

// TestPathB_CLI_Summary_Identity_ShowsDirectBurn is the parallel
// pin: summary on the operator's identity entity itself shows the
// direct burn (Direct=true path), not via-owner phrasing. Same
// machinery, different code path through EffectiveBurn — this
// guards against a renderer that mistakenly applies the cascade-
// rendering branch to direct burns.
func TestPathB_CLI_Summary_Identity_ShowsDirectBurn(t *testing.T) {
	dbPath := seedCascadeScenario(t)

	r := runCLI(t, dbPath,
		"summary", "identity:github/bufferzonecorp",
	)

	require.Equal(t, 0, r.exitCode, "summary must exit 0; stderr=%q", r.stderr)
	assert.Contains(t, r.stdout, "BURNED")
	assert.Contains(t, r.stdout, "campaign-shaped",
		"the direct-burn reason surfaces verbatim")
	// The "via owner" cascade phrase must NOT appear when the burn
	// is direct on the queried entity. That's how the rendering
	// distinguishes the two cases.
	assert.NotContains(t, r.stdout, "via owner",
		"direct burn rendering must NOT include the cascade phrase — Direct=true is its own case")
}

// TestPathB_CLI_BurnList_ShowsLiteralRowsNotCascaded pins the
// audit-surface contract: burn list must keep showing what's
// LITERALLY in the burns table — one row, on the identity. The
// repo gets its cascade through EffectiveBurn at display time
// elsewhere (summary, analyze), but the audit surface stays
// faithful to the rows. countercampaign.md §7.7.
func TestPathB_CLI_BurnList_ShowsLiteralRowsNotCascaded(t *testing.T) {
	dbPath := seedCascadeScenario(t)

	r := runCLI(t, dbPath, "burn", "list")
	require.Equal(t, 0, r.exitCode)

	assert.Contains(t, r.stdout, "identity:github/bufferzonecorp",
		"burn list must include the literal burn row on the operator")

	// The repo URI must NOT appear in burn list, even though
	// summary on the repo shows it as cascaded-burned. burn list
	// is the audit surface; cascade lives at the display layer.
	assert.NotContains(t, r.stdout, "repo:github/bufferzonecorp/grpc-client",
		"burn list must NOT include the cascaded repo — the audit surface stays faithful to literal table rows; the cascade lives at the display layer")
}
