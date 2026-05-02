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

// Path F end-to-end: with the git collector emitting
// commit_signing_keys + minting identity:gpg/<keyid> rows, burning
// a GPG key must surface as a "BURNED ... via signer
// identity:gpg/<keyid>" rendering on `signatory summary` for any
// repo whose recent commits include a signature from that key.
//
// Seed pattern mirrors path_b_cascade_test.go's seedCascadeScenario:
// rather than producing real GPG-signed commits in a test repo
// (which would require provisioning a test GPG key + signing
// configuration into every test environment), the scenario writes
// the signal + entity rows directly via the public store API. The
// unit-level tests in internal/signal/git/ (TestExtractPer
// DeveloperKeyIDs + TestEnsureSignerEntities_*) cover the
// extract-and-mint side; these tests cover the cascade-and-render
// side end-to-end through the binary surface.

// gpgTestKeyID is the canonical lowercased 16-hex-char form a key
// ID lands in once the collector has lowercased it for canonical-
// URI construction. Used across the tests so the scenario is
// pinnable from a single location.
const gpgTestKeyID = "deadbeefcafebabe"

// seedGPGCascadeScenario sets up the standard "GPG signer burned,
// repo signed by them is queried" state in a fresh DB:
//
//   - identity:gpg/<keyID> entity exists (minted via burn add) and
//     is burned with the supplied reason.
//   - repo:github/some-org/some-repo entity exists.
//   - commit_signing_keys signal on the repo lists the burned key.
//
// Returns the DB path so tests can drive runCLI against it.
func seedGPGCascadeScenario(t *testing.T) string {
	t.Helper()

	dbPath := newCLITestDB(t)

	// Mint the identity entity via burn add — exercises the same
	// CLI path a user would type at the shell.
	add := runCLI(t, dbPath,
		"burn", "add", "identity:gpg/"+gpgTestKeyID,
		"--reason", "test: gpg signing key compromised",
	)
	require.Equal(t, 0, add.exitCode, "burn add must succeed; stderr=%q", add.stderr)

	// Open the store directly to seed the repo entity + signal.
	// AnalyzeCmd would be the production path but it requires a
	// real local clone with signed commits; this test focuses on
	// the cascade side, so we synthesise the commit_signing_keys
	// signal directly. (Same trade-off path_b_cascade_test.go made
	// for owner_profile.)
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	now := time.Now().UTC().Truncate(time.Second)
	repo := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: "repo:github/some-org/some-repo",
		Type:         profile.EntityProject,
		ShortName:    "some-org/some-repo",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, s.PutEntity(t.Context(), repo))

	signingKeys, err := json.Marshal(map[string]any{
		"count":   1,
		"key_ids": []string{gpgTestKeyID},
		"window":  "8760h0m0s",
	})
	require.NoError(t, err)
	sig := profile.Signal{
		ID:                profile.NewEntityID(),
		EntityID:          repo.ID,
		Type:              "commit_signing_keys",
		Group:             profile.SignalGroupGovernance,
		Source:            "git",
		ForgeryResistance: profile.ForgeryVeryHigh,
		Value:             signingKeys,
		CollectedAt:       now,
		ExpiresAt:         now.Add(time.Hour),
	}
	require.NoError(t, s.AppendSignals(t.Context(), []profile.Signal{sig}))

	return dbPath
}

// TestPathF_CLI_Summary_Repo_ShowsCascadeFromBurnedSigner is the
// load-bearing end-to-end test for Path F. With the GPG key burned
// and the commit_signing_keys signal in place, summary on a repo
// signed by that key must show:
//
//   - the BURNED marker (the cascade fired),
//   - the cascaded reason in the rendered output,
//   - some indication the burn cascaded from the signer (so the
//     user knows which ledger entry caused the degradation).
func TestPathF_CLI_Summary_Repo_ShowsCascadeFromBurnedSigner(t *testing.T) {
	dbPath := seedGPGCascadeScenario(t)

	r := runCLI(t, dbPath,
		"summary", "repo:github/some-org/some-repo",
	)
	require.Equal(t, 0, r.exitCode, "summary must exit 0; stderr=%q", r.stderr)

	assert.Contains(t, r.stdout, "BURNED",
		"summary must surface the burn marker even though the burn is on the signing key, not the repo itself")
	assert.Contains(t, r.stdout, "compromised",
		"the cascaded burn reason must surface in the rendered output")
	assert.Contains(t, r.stdout, "identity:gpg/"+gpgTestKeyID,
		"the rendering must name the cascade source so users can trace which ledger entry caused the degradation")
}

// TestPathF_CLI_Summary_Identity_ShowsDirectBurn is the parallel
// pin: summary on the GPG identity itself shows the direct burn
// (Direct=true path), not via-signer phrasing. Same machinery,
// different code path through EffectiveBurn — guards against a
// renderer that mistakenly applies the cascade-rendering branch
// to direct burns. Mirrors path_b_cascade_test.go's
// TestPathB_CLI_Summary_Identity_ShowsDirectBurn.
func TestPathF_CLI_Summary_Identity_ShowsDirectBurn(t *testing.T) {
	dbPath := seedGPGCascadeScenario(t)

	r := runCLI(t, dbPath,
		"summary", "identity:gpg/"+gpgTestKeyID,
	)
	require.Equal(t, 0, r.exitCode, "summary must exit 0; stderr=%q", r.stderr)

	assert.Contains(t, r.stdout, "BURNED")
	assert.Contains(t, r.stdout, "compromised",
		"the direct-burn reason surfaces verbatim")
	// The "via" cascade phrase must NOT appear when the burn is
	// direct on the queried entity. That's how the rendering
	// distinguishes the two cases.
	assert.NotContains(t, r.stdout, "via signer",
		"direct burn rendering must NOT include the cascade phrase — Direct=true is its own case")
	assert.NotContains(t, r.stdout, "via owner",
		"same — wrong cascade phrase should not appear either")
}

// TestPathF_CLI_BurnList_ShowsLiteralGPGRow pins the audit-surface
// contract for Path F: burn list shows the literal burn row on
// the identity:gpg, NOT the cascaded one on the repo. Same split
// as the Path B and Path E equivalents. countercampaign.md §7.7.
func TestPathF_CLI_BurnList_ShowsLiteralGPGRow(t *testing.T) {
	dbPath := seedGPGCascadeScenario(t)

	r := runCLI(t, dbPath, "burn", "list")
	require.Equal(t, 0, r.exitCode)

	assert.Contains(t, r.stdout, "identity:gpg/"+gpgTestKeyID,
		"burn list must include the literal burn row on the GPG identity")
	assert.NotContains(t, r.stdout, "repo:github/some-org/some-repo",
		"burn list must NOT include the cascaded repo — the audit surface stays faithful to literal table rows; the cascade lives at the display layer")
}

// TestPathF_CLI_Summary_HealthyRepo_NoBanner is the regression
// guard: a repo with NO commit_signing_keys signal at all (or with
// signed commits but no burned key) must NOT show a BURNED banner.
// Mirrors path_b_cascade_test.go's healthy-target negative test.
func TestPathF_CLI_Summary_HealthyRepo_NoBanner(t *testing.T) {
	dbPath := newCLITestDB(t)

	// No burns, no signals — fresh store. The summary should print
	// the standard "no entity matches" / cached-summary path
	// without any BURNED banner.
	r := runCLI(t, dbPath, "summary", "repo:github/healthy-org/healthy-repo")
	require.Equal(t, 0, r.exitCode)
	assert.NotContains(t, r.stdout, "BURNED",
		"healthy target with no GPG burn must NOT produce a BURNED banner")
}

// TestPathF_CLI_Summary_HealthyKeyAlsoSigning_NoCascade pins the
// "key signed but not burned" regression: a repo whose
// commit_signing_keys lists a key that EXISTS as an entity row
// but is NOT burned must NOT trip the cascade. Catches a future
// resolver bug where the existence of an identity:gpg row alone
// is enough to surface as a cascade.
func TestPathF_CLI_Summary_HealthyKeyAlsoSigning_NoCascade(t *testing.T) {
	dbPath := newCLITestDB(t)

	// Mint a GPG identity row WITHOUT burning it.
	add := runCLI(t, dbPath,
		"burn", "add", "identity:gpg/0000111122223333",
		"--reason", "set up for the test then immediately remove",
	)
	require.Equal(t, 0, add.exitCode)
	rm := runCLI(t, dbPath,
		"burn", "remove", "identity:gpg/0000111122223333",
		"--reason", "removed so the identity row exists but isn't burned",
	)
	require.Equal(t, 0, rm.exitCode, "burn remove must succeed; stderr=%q", rm.stderr)

	// Now seed a repo with a commit_signing_keys signal naming the
	// (un-burned) key.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	now := time.Now().UTC().Truncate(time.Second)
	repo := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: "repo:github/healthy/repo",
		Type:         profile.EntityProject,
		ShortName:    "healthy/repo",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, s.PutEntity(t.Context(), repo))
	value, err := json.Marshal(map[string]any{
		"count":   1,
		"key_ids": []string{"0000111122223333"},
	})
	require.NoError(t, err)
	require.NoError(t, s.AppendSignals(t.Context(), []profile.Signal{{
		ID:                profile.NewEntityID(),
		EntityID:          repo.ID,
		Type:              "commit_signing_keys",
		Group:             profile.SignalGroupGovernance,
		Source:            "git",
		ForgeryResistance: profile.ForgeryVeryHigh,
		Value:             value,
		CollectedAt:       now,
		ExpiresAt:         now.Add(time.Hour),
	}}))

	r := runCLI(t, dbPath, "summary", "repo:github/healthy/repo")
	require.Equal(t, 0, r.exitCode)
	assert.NotContains(t, r.stdout, "BURNED",
		"a repo signed by a healthy (un-burned) key must NOT trigger a cascade banner — only burned identities cascade")
}
