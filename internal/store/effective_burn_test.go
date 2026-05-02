package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// EffectiveBurn is Path B's cascade resolver. It composes existing
// primitives:
//
//   - GetBurn(target) for the direct case
//   - GetLatestSignals(target) to walk the owner/maintainer/
//     publisher relations encoded in signal values
//   - FindEntityByURI + GetBurn for each candidate URI
//
// Direct burn beats cascade (countercampaign.md §7.7); the cascade
// walks one hop only (owner → target, not target → target's deps);
// first burned cascade candidate wins. No new table.
//
// These tests pin the resolver's contract for every signal-type the
// cascade reads today: owner_profile (github), maintainer_count and
// publish_origin_consistency (npm). When new producer collectors land
// (PyPI maintainers, git committers/signers, etc.), they extend the
// cascade by adding their signal type to the resolver's switch and a
// test here.

// seedOwnerEntity mints an identity:/org: row for the cascade test
// to walk through. Returns the entity ID for downstream burn-target
// references. Mirrors what the github collector does in production
// post-Path-A.
func seedOwnerEntity(t *testing.T, s *SQLite, uri, shortName string) string {
	t.Helper()
	e, _, err := s.EnsureEntityByCanonicalURI(t.Context(), uri, shortName)
	require.NoError(t, err)
	return e.ID
}

// seedRepoEntity mints a repo:github/X/Y row for cascade testing.
// Caller is responsible for attaching any signals it wants to walk.
func seedRepoEntity(t *testing.T, s *SQLite, uri, shortName string) string {
	t.Helper()
	e := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: uri,
		Type:         profile.EntityTypeForURI(uri),
		ShortName:    shortName,
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
		UpdatedAt:    time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, s.PutEntity(t.Context(), e))
	return e.ID
}

// seedSignal appends one latest-state signal of the given type/value
// to entityID. The cascade resolver reads these to find related
// owner/publisher URIs.
func seedSignal(t *testing.T, s *SQLite, entityID, signalType string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	now := time.Now().UTC()
	sig := profile.Signal{
		ID:                profile.NewEntityID(),
		EntityID:          entityID,
		Type:              signalType,
		Group:             profile.SignalGroupGovernance,
		Source:            "test",
		ForgeryResistance: profile.ForgeryHigh,
		Value:             raw,
		CollectedAt:       now,
		ExpiresAt:         now.Add(time.Hour),
	}
	require.NoError(t, s.AppendSignals(t.Context(), []profile.Signal{sig}))
}

// seedBurn attaches an active burn to entityID.
func seedBurn(t *testing.T, s *SQLite, entityID, reason string) {
	t.Helper()
	burn := &profile.Burn{
		EntityID: entityID,
		Reason:   reason,
		Source:   profile.BurnSourceLocal,
		BurnedAt: time.Now().UTC().Truncate(time.Second),
		BurnedBy: "team:test",
	}
	require.NoError(t, s.SetBurn(t.Context(), burn))
}

// TestEffectiveBurn_Direct covers the trivial case: a burn directly
// on the queried entity. Returns the burn with Direct=true; no
// cascade walk happens (and shouldn't — direct beats cascade).
func TestEffectiveBurn_Direct(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/x/y", "x/y")
	seedBurn(t, s, repoID, "test: direct burn")

	burn, ctx, err := s.EffectiveBurn(t.Context(), repoID)
	require.NoError(t, err)
	require.NotNil(t, burn)
	require.NotNil(t, ctx)
	assert.True(t, ctx.Direct, "direct burn must report Direct=true")
	assert.Nil(t, ctx.ViaOwner, "direct case must not populate ViaOwner")
	assert.Equal(t, "test: direct burn", burn.Reason)
}

// TestEffectiveBurn_NoBurn pins the absence shape: a healthy entity
// with no direct burn and no burned related-identity returns
// ErrNotFound. Symmetric with GetBurn's contract — callers handle
// "no burn" via errors.Is(err, ErrNotFound).
func TestEffectiveBurn_NoBurn(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/x/y", "x/y")
	// Owner entity exists but isn't burned.
	seedOwnerEntity(t, s, "identity:github/x", "x")
	seedSignal(t, s, repoID, "owner_profile", map[string]any{
		"login": "x", "type": "User",
	})

	_, _, err := s.EffectiveBurn(t.Context(), repoID)
	require.ErrorIs(t, err, ErrNotFound,
		"no burn anywhere must surface as ErrNotFound, matching GetBurn's contract")
}

// TestEffectiveBurn_CascadeViaGithubOwner_User: a repo whose
// owner_profile signal names a User-typed login that has been
// burned at identity:github/<login> reports the cascade.
func TestEffectiveBurn_CascadeViaGithubOwner_User(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/bufferzonecorp/grpc-client", "bufferzonecorp/grpc-client")
	ownerID := seedOwnerEntity(t, s, "identity:github/bufferzonecorp", "bufferzonecorp")
	seedSignal(t, s, repoID, "owner_profile", map[string]any{
		"login": "bufferzonecorp",
		"type":  "User",
	})
	seedBurn(t, s, ownerID, "campaign-shaped account, 17 throwaway repos")

	burn, ctx, err := s.EffectiveBurn(t.Context(), repoID)
	require.NoError(t, err)
	require.NotNil(t, burn)
	require.NotNil(t, ctx)
	assert.False(t, ctx.Direct, "cascade case must report Direct=false")
	require.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "identity:github/bufferzonecorp", ctx.ViaOwner.CanonicalURI)
	assert.Equal(t, "publisher", ctx.ViaRole)
	assert.Contains(t, burn.Reason, "campaign-shaped")
}

// TestEffectiveBurn_CascadeViaGithubOwner_Organization is the
// org: parallel: when the owner_profile names an Organization,
// the cascade walks org:github/<name>, not identity:.
func TestEffectiveBurn_CascadeViaGithubOwner_Organization(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/some-org/some-repo", "some-org/some-repo")
	orgID := seedOwnerEntity(t, s, "org:github/some-org", "some-org")
	seedSignal(t, s, repoID, "owner_profile", map[string]any{
		"login": "some-org",
		"type":  "Organization",
	})
	seedBurn(t, s, orgID, "test: org compromise")

	burn, ctx, err := s.EffectiveBurn(t.Context(), repoID)
	require.NoError(t, err)
	require.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "org:github/some-org", ctx.ViaOwner.CanonicalURI,
		"Organization owners cascade through org: URIs, not identity:")
	assert.Equal(t, "test: org compromise", burn.Reason)
}

// TestEffectiveBurn_CascadeViaNpmMaintainer pins the npm case:
// a package whose maintainer_count signal lists a burned
// identity:npm/<login> reports the cascade.
func TestEffectiveBurn_CascadeViaNpmMaintainer(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	pkgID := seedRepoEntity(t, s, "pkg:npm/lodash", "lodash")
	maintID := seedOwnerEntity(t, s, "identity:npm/jdalton", "jdalton")
	seedSignal(t, s, pkgID, "maintainer_count", map[string]any{
		"count":  1,
		"logins": []string{"jdalton"},
	})
	seedBurn(t, s, maintID, "test: maintainer account compromised")

	burn, ctx, err := s.EffectiveBurn(t.Context(), pkgID)
	require.NoError(t, err)
	require.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "identity:npm/jdalton", ctx.ViaOwner.CanonicalURI)
	assert.Equal(t, "maintainer", ctx.ViaRole,
		"the npm Maintainers list maps to ViaRole=maintainer (not publisher)")
	assert.Contains(t, burn.Reason, "compromised")
}

// TestEffectiveBurn_CascadeViaNpmPublisher: a package with
// publish_origin_consistency listing a burned historical publisher
// (not currently in Maintainers — the lodash takeover-pattern shape)
// still cascades. This is the case Path B specifically buys for npm:
// historical publishers stay identity-relevant.
func TestEffectiveBurn_CascadeViaNpmPublisher(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	pkgID := seedRepoEntity(t, s, "pkg:npm/lodash", "lodash")
	bnjID := seedOwnerEntity(t, s, "identity:npm/bnjmnt4n", "bnjmnt4n")
	seedSignal(t, s, pkgID, "publish_origin_consistency", map[string]any{
		"publishers":        []string{"bnjmnt4n", "jdalton", "mathias"},
		"unique_publishers": 3,
		"latest_publisher":  "jdalton",
	})
	seedBurn(t, s, bnjID, "test: historical publisher account takeover")

	burn, ctx, err := s.EffectiveBurn(t.Context(), pkgID)
	require.NoError(t, err)
	require.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "identity:npm/bnjmnt4n", ctx.ViaOwner.CanonicalURI)
	assert.Equal(t, "publisher", ctx.ViaRole)
	assert.Contains(t, burn.Reason, "historical publisher")
}

// TestEffectiveBurn_DirectBeatsCascade verifies the precedence rule:
// when both a direct burn AND a cascade-applicable owner-burn exist,
// the direct burn is returned as primary. ViaOwner stays nil so the
// renderer doesn't conflate the two stories.
func TestEffectiveBurn_DirectBeatsCascade(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/x/y", "x/y")
	ownerID := seedOwnerEntity(t, s, "identity:github/x", "x")
	seedSignal(t, s, repoID, "owner_profile", map[string]any{
		"login": "x", "type": "User",
	})
	seedBurn(t, s, ownerID, "owner-level burn")
	seedBurn(t, s, repoID, "direct burn — supersedes")

	burn, ctx, err := s.EffectiveBurn(t.Context(), repoID)
	require.NoError(t, err)
	assert.True(t, ctx.Direct, "direct must win over cascade")
	assert.Equal(t, "direct burn — supersedes", burn.Reason)
	assert.Nil(t, ctx.ViaOwner,
		"the direct case must not surface ViaOwner — that would conflate which burn 'caused' the effective state")
}

// TestEffectiveBurn_OwnerEntityNotMinted_NoCascade: a repo's
// owner_profile signal exists, but the corresponding identity entity
// hasn't been minted yet (e.g., pre-Path-A data, or an analyze run
// that errored before reaching collectOwnerProfile's mint branch).
// Cascade silently skips because there's no entity row to attach a
// burn to — the resolver returns ErrNotFound for "no effective burn",
// not an error for "owner entity missing." This is correct: a missing
// owner entity is functionally identical to "no burn on that owner."
func TestEffectiveBurn_OwnerEntityNotMinted_NoCascade(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/x/y", "x/y")
	// Note: NO seedOwnerEntity — the identity row doesn't exist.
	seedSignal(t, s, repoID, "owner_profile", map[string]any{
		"login": "x", "type": "User",
	})

	_, _, err := s.EffectiveBurn(t.Context(), repoID)
	require.ErrorIs(t, err, ErrNotFound,
		"owner_profile signal pointing at a non-existent identity must NOT error — it's just 'no effective burn', because there's no entity row to attach a burn to")
}

// TestEffectiveBurn_MultiplePublishers_BurnedOneCascades: an npm
// package with three publishers, one of which is burned. The cascade
// fires with the burned publisher as ViaOwner; the other two are
// irrelevant. Confirms the resolver walks the full publishers list,
// not just the first entry.
func TestEffectiveBurn_MultiplePublishers_BurnedOneCascades(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	pkgID := seedRepoEntity(t, s, "pkg:npm/lodash", "lodash")
	// Mint all three publishers; only burn one of them.
	seedOwnerEntity(t, s, "identity:npm/jdalton", "jdalton")
	mathiasID := seedOwnerEntity(t, s, "identity:npm/mathias", "mathias")
	seedOwnerEntity(t, s, "identity:npm/bnjmnt4n", "bnjmnt4n")
	seedSignal(t, s, pkgID, "publish_origin_consistency", map[string]any{
		"publishers": []string{"jdalton", "mathias", "bnjmnt4n"},
	})
	seedBurn(t, s, mathiasID, "test: only mathias is burned")

	burn, ctx, err := s.EffectiveBurn(t.Context(), pkgID)
	require.NoError(t, err)
	require.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "identity:npm/mathias", ctx.ViaOwner.CanonicalURI,
		"the cascade must find the burned publisher even when listed in the middle of the publishers array")
	assert.Equal(t, "test: only mathias is burned", burn.Reason)
}

// TestEffectiveBurn_WithdrawnOwnerBurn_NoCascade pins the soft-
// delete inheritance: when a previously-burned owner has the burn
// withdrawn (via WithdrawBurn), the cascade no longer fires on
// entities related to that owner. EffectiveBurn composes GetBurn,
// which already filters withdrawn rows; this test confirms the
// composition holds.
func TestEffectiveBurn_WithdrawnOwnerBurn_NoCascade(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/x/y", "x/y")
	ownerID := seedOwnerEntity(t, s, "identity:github/x", "x")
	seedSignal(t, s, repoID, "owner_profile", map[string]any{
		"login": "x", "type": "User",
	})
	seedBurn(t, s, ownerID, "test: premature burn")

	// Withdraw the owner burn before querying.
	require.NoError(t, s.WithdrawBurn(t.Context(), ownerID, "team:test", "false positive", time.Now().UTC()))

	_, _, err := s.EffectiveBurn(t.Context(), repoID)
	require.ErrorIs(t, err, ErrNotFound,
		"a withdrawn owner burn must NOT cascade — soft-delete inheritance through GetBurn keeps the unburn semantic clean")
}
