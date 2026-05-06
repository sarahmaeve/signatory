package survey

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
	pypimanifest "github.com/sarahmaeve/signatory/internal/manifest/pypi"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// openTestStore creates an empty SQLite store in a temp file and
// returns it. Mirrors the helper used in internal/mcp/tools;
// duplicated here because the survey package is tested with a
// real store rather than a mock to exercise the interface
// faithfully.
func openTestStore(t *testing.T) *store.SQLite {
	t.Helper()
	s, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "survey.db"))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// writeGoMod writes a small valid go.mod to a temp dir and
// returns its path. Used by the Run-level tests below.
func writeGoMod(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	return path
}

// seedEntity inserts a minimal entity into the store for lookup.
// Short-name is derived from the URI's last path segment.
func seedEntity(t *testing.T, s store.Store, uri string) *profile.Entity {
	t.Helper()
	e := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: uri,
		Type:         profile.EntityProject,
		ShortName:    lastSegment(uri),
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, s.PutEntity(context.Background(), e))
	return e
}

// seedPosture attaches a posture to an entity. Tests use this to
// construct the various "store state" scenarios survey must
// resolve correctly.
func seedPosture(t *testing.T, s store.Store, entityID, version string, tier profile.PostureTier, rationale string) {
	t.Helper()
	p := &profile.Posture{
		EntityID:  entityID,
		Tier:      tier,
		Version:   version,
		Rationale: rationale,
		SetBy:     "test",
		SetAt:     time.Now().UTC(),
	}
	require.NoError(t, s.SetPosture(context.Background(), p))
}

// seedPostureAt is the explicit-timestamp variant of seedPosture.
// Used by tests that need deterministic SetAt ordering — the
// default seedPosture uses time.Now() which may produce identical
// timestamps for back-to-back calls at sub-second resolution,
// making "most recent wins" tiebreaks non-deterministic.
func seedPostureAt(t *testing.T, s store.Store, entityID, version string, tier profile.PostureTier, rationale string, setAt time.Time) {
	t.Helper()
	p := &profile.Posture{
		EntityID:  entityID,
		Tier:      tier,
		Version:   version,
		Rationale: rationale,
		SetBy:     "test",
		SetAt:     setAt,
	}
	require.NoError(t, s.SetPosture(context.Background(), p))
}

// seedBurn attaches a burn record. Per design/trust-policy-v1.md
// burns are Layer-0 and override anything — survey's resolver
// should reflect that.
func seedBurn(t *testing.T, s store.Store, entityID, reason string) {
	t.Helper()
	require.NoError(t, s.SetBurn(context.Background(), &profile.Burn{
		EntityID: entityID,
		Reason:   reason,
		Source:   profile.BurnSourceLocal,
		BurnedAt: time.Now().UTC(),
		BurnedBy: "test",
	}))
}

// TestRun_AllUnexaminedWhenStoreEmpty is the baseline: empty
// store, every dep reports not-in-store, direct deps land in
// NeedsReview, indirect deps don't.
func TestRun_AllUnexaminedWhenStoreEmpty(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/empty-store

go 1.25.1

require (
	github.com/alecthomas/kong v1.15.0
	gopkg.in/yaml.v3 v3.0.1
)

require github.com/davecgh/go-spew v1.1.1 // indirect
`)

	s := openTestStore(t)

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	// Project info.
	assert.Equal(t, "github.com/example/empty-store", r.Project.Name)
	assert.Equal(t, "go", r.Project.Ecosystem)
	assert.Equal(t, "1.25.1", r.Project.EcoVersion)

	// Summary.
	assert.Equal(t, 3, r.Summary.Total)
	assert.Equal(t, 2, r.Summary.Direct)
	assert.Equal(t, 1, r.Summary.Indirect)
	assert.Equal(t, 3, r.Summary.ByTier[TierNotInStore])

	// Direct deps should be in NeedsReview; indirect should NOT.
	assert.ElementsMatch(t,
		[]string{"repo:github/alecthomas/kong", "pkg:golang/gopkg.in/yaml.v3"},
		r.Summary.NeedsReview,
		"direct deps in NeedsReview, indirect deps excluded")

	// Every dep's tier is TierNotInStore.
	for _, d := range r.Deps {
		assert.Equal(t, TierNotInStore, d.Tier, "%s tier", d.Dep.Name)
	}
}

// TestRun_ExactVersionPostureMatches covers the happy-path
// posture lookup: store has a posture for the exact version
// pinned in go.mod → survey reports that tier.
func TestRun_ExactVersionPostureMatches(t *testing.T) {
	t.Parallel()

	// Using kong v1.15.0 as the fixture target. Real structlog
	// uses CalVer (25.5.0) which would need a `/v25` import-path
	// suffix under Go semantic-import-versioning; modfile rejects
	// bare v25 versions. The specific module name doesn't matter
	// for this test — we're exercising exact-version-match logic.
	path := writeGoMod(t, `module github.com/example/posture-match

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/alecthomas/kong")
	seedPosture(t, s, e.ID, "v1.15.0", profile.PostureVettedFrozen, "strong publish chain")

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierVettedFrozen, d.Tier)
	assert.Equal(t, "v1.15.0", d.PostureVersion)
	assert.Contains(t, d.PostureRationale, "strong publish chain")
	assert.Nil(t, d.OtherVersions,
		"exact-match posture short-circuits before other-versions aggregation — summary must be nil")

	// NeedsReview should be empty — vetted-frozen is a
	// resolved-to-positive decision, not something to review.
	assert.Empty(t, r.Summary.NeedsReview)
	assert.Equal(t, 1, r.Summary.ByTier[TierVettedFrozen])
}

// TestRun_PostureForOtherVersion_PopulatesSummary — pinned v1.15.0
// but the store has a posture for v1.14.0 only. Tier is unexamined
// and OtherVersions carries the prior-posture metadata so the
// renderer can surface "(v1.14.0 vetted-frozen; 1 posture on
// record)" without a second store round-trip.
func TestRun_PostureForOtherVersion_PopulatesSummary(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/diff-version

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/alecthomas/kong")
	seedPosture(t, s, e.ID, "v1.14.0", profile.PostureVettedFrozen, "old version vetted")

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierUnexamined, d.Tier)
	assert.Empty(t, d.PostureVersion,
		"PostureVersion is only populated on an exact match")

	require.NotNil(t, d.OtherVersions, "other-version posture must be surfaced")
	require.NotNil(t, d.OtherVersions.MostRecent)
	assert.Equal(t, "v1.14.0", d.OtherVersions.MostRecent.Version)
	assert.Equal(t, TierVettedFrozen, d.OtherVersions.MostRecent.Tier)
	assert.Equal(t, "old version vetted", d.OtherVersions.MostRecent.Rationale)
	assert.Equal(t, 1, d.OtherVersions.TotalPostures)

	// The pinned version is still in NeedsReview because it lacks
	// an exact posture decision.
	assert.Contains(t, r.Summary.NeedsReview, "repo:github/alecthomas/kong")
}

// TestRun_OtherVersions_MostRecentWins seeds three postures on
// different versions with controlled SetAt timestamps and
// confirms the aggregation picks the largest-SetAt winner as
// MostRecent. The three versions are NOT in set_at order — the
// most recent is v1.14.0 (middle version) to catch an
// implementation that naively returns the first or last slice
// element.
//
// Revert proof: drop the `After(...)` comparison and return
// postures[0] unconditionally; this test fails because v1.10.0
// would become MostRecent instead of v1.14.0.
func TestRun_OtherVersions_MostRecentWins(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/multi-posture

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/alecthomas/kong")

	// Deliberately non-monotonic seeding order so the test
	// doesn't accidentally pass by taking the last-inserted row.
	// Timestamps are explicit and well-separated (one minute
	// apart) so the ordering is unambiguous.
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	seedPostureAt(t, s, e.ID, "v1.10.0", profile.PostureUnknownProvenance,
		"early review — inconclusive", base)
	seedPostureAt(t, s, e.ID, "v1.14.0", profile.PostureVettedFrozen,
		"most recent full review", base.Add(2*time.Minute))
	seedPostureAt(t, s, e.ID, "v1.12.0", profile.PostureTrustedForNow,
		"intermediate review", base.Add(1*time.Minute))

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	require.NotNil(t, d.OtherVersions)
	require.NotNil(t, d.OtherVersions.MostRecent)

	assert.Equal(t, "v1.14.0", d.OtherVersions.MostRecent.Version,
		"MostRecent must be the largest-SetAt posture, not first/last by seed order")
	assert.Equal(t, TierVettedFrozen, d.OtherVersions.MostRecent.Tier)
	assert.Equal(t, "most recent full review", d.OtherVersions.MostRecent.Rationale)
	assert.Equal(t, 3, d.OtherVersions.TotalPostures,
		"TotalPostures counts every posture on the entity regardless of version")
}

// TestRun_VersionedEntityFallback covers the testify-class M1
// violation surfaced by 2026-04-27 dogfood: store has the entity at a
// versioned URI (e.g. `repo:github/stretchr/testify@v1.11.1`), survey
// passes the unversioned base URI (the gomod parser's output), and
// the exact-match lookup misses. LookupEntity's versioned-base-scan
// fallback must turn this into a hit so survey reports the entity's
// trust state instead of `not-in-store`.
//
// Revert proof: replace the LookupEntity call in resolveDep with a
// direct FindEntityByURI; this test fails because the M1-violation
// entity is invisible.
func TestRun_VersionedEntityFallback(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/m1-fallback

go 1.25.1

require github.com/stretchr/testify v1.11.1
`)

	s := openTestStore(t)
	// Entity row sits at the versioned URI (M1 violation). Posture is
	// attached so the row counts as "resolved" for the user.
	e := seedEntity(t, s, "repo:github/stretchr/testify@v1.11.1")
	seedPosture(t, s, e.ID, "v1.11.1", profile.PostureTrustedForNow,
		"vetted at this version")

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierTrustedForNow, d.Tier,
		"the @V entity must be discoverable from a base-URI lookup; otherwise the row is invisible to survey")
}

// TestRun_VanityGoPathFallback covers entry 3 for golang.org/x/mod:
// signal-collection pipelines vanity-resolve the import path at
// ingest time and store the entity at `repo:github/golang/Y`, but
// gomod's parser produces the import-path form `pkg:go/golang.org/x/Y`.
// LookupEntity's alternate-walk must bridge those forms so survey
// finds the entity regardless of which writer recorded it.
func TestRun_VanityGoPathFallback(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/vanity-fallback

go 1.25.1

require golang.org/x/mod v0.35.0
`)

	s := openTestStore(t)
	// Store has the vanity-resolved github form; survey will look up
	// the import-path form and must reach this row via alternates.
	e := seedEntity(t, s, "repo:github/golang/mod")
	seedPosture(t, s, e.ID, "v0.35.0", profile.PostureTrustedForNow,
		"vanity-resolved by signal pipeline")

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierTrustedForNow, d.Tier,
		"vanity-resolved github entity must be reachable from the import-path form survey passes")
}

// TestRun_NoVersionPosture_PromotedAsVerdict — pinned v1.15.0 and
// the store has a single posture with an empty Version field. A
// no-version posture is a global verdict that covers all versions
// (see the trust-policy doc: "version==” is the unconstrained-
// version posture, applies whenever no exact match exists"), so
// survey must report its tier directly rather than falling
// through to "unexamined" with an OtherVersions parenthetical.
//
// Surfaced by 2026-04-27 dogfood: kong rendered as "[?]
// unexamined ( trusted-for-now; 1 posture on record)" — both
// checks were running, the version-matched check won the tier
// slot, and the any-posture check populated the parenthetical.
// The result was a contradictory two-faced row that undermines
// trust in the survey output.
//
// Revert proof: remove the no-version-posture branch in
// resolveDep; this test fails because d.Tier would be
// TierUnexamined and d.OtherVersions would be non-nil.
func TestRun_NoVersionPosture_PromotedAsVerdict(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/no-version-posture

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/alecthomas/kong")
	seedPosture(t, s, e.ID, "", profile.PostureTrustedForNow,
		"broad endorsement covers all versions")

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierTrustedForNow, d.Tier,
		"no-version posture covers all versions; tier must reflect it, not 'unexamined'")
	assert.Equal(t, "", d.PostureVersion,
		"PostureVersion mirrors the matched posture's Version field — empty for the no-version case")
	assert.Contains(t, d.PostureRationale, "broad endorsement",
		"rationale from the no-version posture must surface so renderers can show 'why'")
	assert.Nil(t, d.OtherVersions,
		"a matched no-version posture is the verdict, not 'another version'; surfacing OtherVersions reproduces the dogfood contradiction")

	// NeedsReview must NOT include this entity — the verdict is
	// resolved (trusted-for-now), not pending review.
	assert.Empty(t, r.Summary.NeedsReview,
		"resolved-tier dep must not appear in NeedsReview")
}

// TestRun_NoVersionPosture_VersionedMatchStillWins — when the store
// has BOTH a no-version posture AND a version-specific posture for
// the queried version, the version-specific posture wins. The
// no-version branch is the fallback, not the override.
func TestRun_NoVersionPosture_VersionedMatchStillWins(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/both-postures

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/alecthomas/kong")
	seedPosture(t, s, e.ID, "", profile.PostureTrustedForNow,
		"broad endorsement (no version)")
	seedPosture(t, s, e.ID, "v1.15.0", profile.PostureVettedFrozen,
		"deep review of this exact version")

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierVettedFrozen, d.Tier,
		"version-specific posture must beat the no-version fallback when both are present")
	assert.Equal(t, "v1.15.0", d.PostureVersion)
	assert.Contains(t, d.PostureRationale, "deep review")
}

// TestRun_OtherVersions_NilWhenNoPostures covers the baseline:
// an entity in the store with NO postures resolves to
// TierUnexamined with OtherVersions == nil. Distinct from the
// "postures exist but none match" case above.
//
// Revert proof: change summarizeOtherVersionPostures to always
// return a non-nil summary even on empty input; this test fails
// because d.OtherVersions would be non-nil.
func TestRun_OtherVersions_NilWhenNoPostures(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/entity-only

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	s := openTestStore(t)
	seedEntity(t, s, "repo:github/alecthomas/kong")
	// No postures seeded.

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierUnexamined, d.Tier)
	assert.Nil(t, d.OtherVersions,
		"entity with no postures must leave OtherVersions nil — distinct from 'postures exist but for other versions'")
}

// TestRun_OtherVersions_NilOnExactMatch covers the other
// short-circuit: when the queried version has an exact-match
// posture, we return at the exact-match branch and never build
// OtherVersionsSummary. Guards against a future refactor that
// unconditionally aggregates regardless of exact-match.
//
// Revert proof: move the summarizeOtherVersionPostures call
// ABOVE the exact-match loop; this test fails because
// OtherVersions would be populated even when the exact match
// was found.
func TestRun_OtherVersions_NilOnExactMatch(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/exact-match

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/alecthomas/kong")
	// Two postures: one for the queried version (exact match)
	// and one for a different version. Exact match wins; the
	// different-version one is NOT surfaced because the exact
	// match's rationale is what applies.
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	seedPostureAt(t, s, e.ID, "v1.15.0", profile.PostureVettedFrozen,
		"current version is good", base.Add(time.Minute))
	seedPostureAt(t, s, e.ID, "v1.14.0", profile.PostureTrustedForNow,
		"older version was trusted-for-now", base)

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierVettedFrozen, d.Tier,
		"exact-match posture wins")
	assert.Equal(t, "v1.15.0", d.PostureVersion)
	assert.Nil(t, d.OtherVersions,
		"exact match short-circuits before aggregation — other-version postures must not be surfaced")
}

// TestRun_BurnOverridesPosture covers the trust-policy Layer 0
// rule: a burn record makes the resolved tier TierBurned
// regardless of what posture is recorded.
func TestRun_BurnOverridesPosture(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/burn-over-posture

go 1.25.1

require github.com/compromised/lib v1.0.0
`)

	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/compromised/lib")
	// Posture says vetted-frozen. Burn says this version was
	// compromised post-audit. Survey must prefer the burn.
	seedPosture(t, s, e.ID, "v1.0.0", profile.PostureVettedFrozen, "was vetted")
	seedBurn(t, s, e.ID, "build pipeline compromised 2026-04-18")

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierBurned, d.Tier, "burn must override posture")
	assert.Equal(t, "build pipeline compromised 2026-04-18", d.BurnReason)
	assert.Equal(t, 1, r.Summary.ByTier[TierBurned])

	// NeedsReview should NOT include a burned dep — the action
	// there isn't "review" but "remove."
	assert.Empty(t, r.Summary.NeedsReview)
}

// TestRun_RejectedTierSurfacesCorrectly covers the rejected
// posture we just introduced. Verifies end-to-end that rejected
// postures flow from profile to survey's Tier enum without loss.
func TestRun_RejectedTierSurfacesCorrectly(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/reject-demo

go 1.25.1

require github.com/nvbn/thefuck v0.0.0-yolo
`)

	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/nvbn/thefuck")
	seedPosture(t, s, e.ID, "v0.0.0-yolo", profile.PostureRejected,
		"unmaintained + high-severity design-intent RCE surface")

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierRejected, d.Tier)
	assert.Contains(t, d.PostureRationale, "unmaintained")
	assert.Equal(t, 1, r.Summary.ByTier[TierRejected])
}

// TestRun_LocalReplaceTreatedSpecially covers the replace
// directive pointing at a local path — the dep can't be resolved
// against the store and should surface as TierLocalReplace.
func TestRun_LocalReplaceTreatedSpecially(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/local-replace-demo

go 1.25.1

require github.com/upstream/lib v1.0.0

replace github.com/upstream/lib => ../local/fork
`)

	s := openTestStore(t)

	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	require.Len(t, r.Deps, 1)
	d := r.Deps[0]
	assert.Equal(t, TierLocalReplace, d.Tier)
	assert.Equal(t, 1, r.Summary.ByTier[TierLocalReplace])
	// Local replaces aren't "needs-review" in the analyze-me sense.
	assert.Empty(t, r.Summary.NeedsReview)
}

// TestRun_UnrecognizedManifest errors out when path has a name
// survey doesn't know how to parse. Protects against silent
// nothing-happens on typos like `survey --manifest build.gradle`.
func TestRun_UnrecognizedManifest(t *testing.T) {
	t.Parallel()

	// Write a fake content to a path with an unrecognized filename.
	dir := t.TempDir()
	gradle := filepath.Join(dir, "build.gradle")
	require.NoError(t, os.WriteFile(gradle, []byte("apply plugin: 'java'\n"), 0o600))

	s := openTestStore(t)
	_, err := Run(context.Background(), s, gradle)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized manifest")
	assert.Contains(t, err.Error(), "go.mod")
}

// TestRun_NpmManifest_RoutesToNpmParser covers Phase C's survey
// integration: a package.json manifest reaches the npm parser,
// produces pkg:npm/ canonical URIs, and tiers resolve against the
// store just like go.mod dependencies do.
func TestRun_NpmManifest_RoutesToNpmParser(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
	  "name": "example-npm-project",
	  "version": "1.0.0",
	  "engines": {"node": ">=18.0.0"},
	  "dependencies": {
	    "express": "^4.18.2",
	    "@types/node": "^20.0.0"
	  }
	}`), 0o600))

	s := openTestStore(t)
	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	// Project info plumbed through from the parser.
	assert.Equal(t, "example-npm-project", r.Project.Name)
	assert.Equal(t, "npm", r.Project.Ecosystem)
	assert.Equal(t, ">=18.0.0", r.Project.EcoVersion)

	// Both deps should surface as direct, unexamined, and the
	// scoped package must keep its scope in the canonical URI.
	assert.Equal(t, 2, r.Summary.Total)
	assert.Equal(t, 2, r.Summary.Direct)
	assert.Equal(t, 2, r.Summary.ByTier[TierNotInStore])

	assert.ElementsMatch(t,
		[]string{"pkg:npm/express", "pkg:npm/@types/node"},
		r.Summary.NeedsReview,
		"both direct deps need review; scope preserved on @types/node")
}

func TestRun_EmptyManifestPath(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	_, err := Run(context.Background(), s, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

// TestRun_RequirementsTxt_RoutesToPyPIParser covers PyPI's survey
// integration: a requirements.txt manifest reaches the pypi parser,
// produces pkg:pypi/ canonical URIs (PEP 503 normalized), and tiers
// resolve against the store like other ecosystems do.
func TestRun_RequirementsTxt_RoutesToPyPIParser(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.txt")
	// Includes a mixed-case name to exercise PEP 503 normalization
	// of the canonical URI through the full pipeline.
	require.NoError(t, os.WriteFile(path, []byte(
		"requests==2.31.0\n"+
			"Python-Dotenv==1.0.0\n",
	), 0o600))

	s := openTestStore(t)
	r, err := Run(context.Background(), s, path)
	require.NoError(t, err)

	// Project info: requirements.txt has no project identity, so
	// Name and EcoVersion are empty; only the ecosystem and
	// manifest path get populated.
	assert.Empty(t, r.Project.Name, "requirements.txt declares no project name")
	assert.Equal(t, "pypi", r.Project.Ecosystem)
	assert.Empty(t, r.Project.EcoVersion)

	assert.Equal(t, 2, r.Summary.Total)
	assert.Equal(t, 2, r.Summary.Direct)
	assert.Equal(t, 2, r.Summary.ByTier[TierNotInStore])

	assert.ElementsMatch(t,
		[]string{"pkg:pypi/requests", "pkg:pypi/python-dotenv"},
		r.Summary.NeedsReview,
		"PEP 503 normalization must apply to the canonical URI surfaced for review")
}

// TestRun_PyProjectTOML_NoModernFormat_FailsWithSentinel verifies
// that a pyproject.toml without either [project] or
// [dependency-groups] (e.g., a Poetry-only file with just
// [tool.poetry]) still surfaces the not-yet-supported sentinel.
// Once Commit 6 lands the Poetry parser this test will need to
// be updated again — for now the sentinel firing here is the
// "no Poetry fallback yet" message.
func TestRun_PyProjectTOML_NoModernFormat_FailsWithSentinel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pyproject.toml")
	require.NoError(t, os.WriteFile(path, []byte(
		"[build-system]\nrequires = [\"setuptools>=61.0\"]\nbuild-backend = \"setuptools.build_meta\"\n",
	), 0o600))

	s := openTestStore(t)
	_, err := Run(context.Background(), s, path)
	require.Error(t, err)
	assert.ErrorIs(t, err, pypimanifest.ErrPyProjectTOMLNotYetSupported,
		"pypi sentinel must remain in the chain so callers can render a specific message")
}

// TestRun_SetupPy_FailsWithRedirect verifies that setup.py errors
// out clearly and points the user at parseable alternatives.
// signatory will never parse setup.py — it's executable Python by
// design — so this isn't a temporary state, it's a permanent
// "use a different file" message.
func TestRun_SetupPy_FailsWithRedirect(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "setup.py")
	require.NoError(t, os.WriteFile(path, []byte("from setuptools import setup\nsetup()\n"), 0o600))

	s := openTestStore(t)
	_, err := Run(context.Background(), s, path)
	require.Error(t, err)
	assert.ErrorIs(t, err, pypimanifest.ErrSetupPyNotParseable)
	assert.Contains(t, err.Error(), "pyproject.toml",
		"setup.py error must redirect users to a statically parseable alternative")
	assert.Contains(t, err.Error(), "requirements.txt")
}

// TestRun_PyProjectTOML_PEP621AndPEP735_SmokeFixture is the
// permanent smoke test for the PyPI pipeline. It parses a
// realistic-shape pyproject.toml fixture end-to-end (Detect →
// Parse → ProjectInfo + Deps → tier resolution → Summary) and
// asserts on the result structure. Catches regressions across
// the full pipeline rather than any one layer in isolation.
//
// The fixture file at internal/manifest/pypi/testdata/
// pep621-and-pep735.toml carries inline comments documenting
// the expected dep count and which sub-table each dep comes
// from, so debugging a future failure can read the fixture and
// understand the contract without re-deriving it from this test.
func TestRun_PyProjectTOML_PEP621AndPEP735_SmokeFixture(t *testing.T) {
	t.Parallel()

	fixturePath, err := filepath.Abs("../manifest/pypi/testdata/pep621-pep735/pyproject.toml")
	require.NoError(t, err)

	s := openTestStore(t)
	r, err := Run(context.Background(), s, fixturePath)
	require.NoError(t, err)

	// Project metadata flows through from PEP 621.
	assert.Equal(t, "smoke-pkg", r.Project.Name)
	assert.Equal(t, "pypi", r.Project.Ecosystem)
	assert.Equal(t, ">=3.10", r.Project.EcoVersion)

	// Dep count: 11 per the fixture's documented breakdown
	// (4 from [project].dependencies + 1 docs + 1 test +
	//  2 dev group + 1 test group + 2 all-test group).
	// PEP 735 forbids deduplication during include expansion,
	// so the all-test group's pytest (via include) and the
	// test group's own pytest both surface — that's the spec.
	assert.Equal(t, 11, r.Summary.Total,
		"flat dep total counts every declaration; cross-table dedup is not the parser's job")
	assert.Equal(t, 11, r.Summary.Direct,
		"every pyproject.toml dep is Direct; no transitives at this layer")

	// Spot-check key shapes through the full pipeline:
	// PEP 503 normalization must apply ("Python-Dotenv" → lowercased)
	// and environment markers must be stripped (pywin32 surfaces
	// regardless of sys_platform).
	needsReview := make(map[string]bool, len(r.Summary.NeedsReview))
	for _, uri := range r.Summary.NeedsReview {
		needsReview[uri] = true
	}
	assert.True(t, needsReview["pkg:pypi/python-dotenv"],
		"PEP 503 normalization applied through full pipeline: Python-Dotenv → python-dotenv")
	assert.True(t, needsReview["pkg:pypi/pywin32"],
		"environment marker on pywin32 stripped; dep surfaces regardless of platform")
	assert.True(t, needsReview["pkg:pypi/pytest"],
		"pytest from the test group AND from the all-test include both surface")
	assert.True(t, needsReview["pkg:pypi/pytest-cov"],
		"PEP 735 group entries surface alongside PEP 621 deps")
	assert.True(t, needsReview["pkg:pypi/sphinx"],
		"PEP 621 optional-dependencies surface alongside main dependencies")
}

// TestRun_PyProjectTOML_PEP735OnlyApp_SmokeFixture is the
// permanent smoke test for the PEP 735 application/CLI use case
// — a Python project with no [project] table at all. This was
// the gap PEP 735 was designed to close (pre-PEP-735, applications
// had nowhere standard to declare dev/test deps).
func TestRun_PyProjectTOML_PEP735OnlyApp_SmokeFixture(t *testing.T) {
	t.Parallel()

	fixturePath, err := filepath.Abs("../manifest/pypi/testdata/pep735-app/pyproject.toml")
	require.NoError(t, err)

	s := openTestStore(t)
	r, err := Run(context.Background(), s, fixturePath)
	require.NoError(t, err)

	// No [project] → no project identity. Ecosystem still set,
	// because the file IS recognized as a PyPI manifest.
	assert.Empty(t, r.Project.Name, "no [project] table; no project name")
	assert.Empty(t, r.Project.EcoVersion, "no [project] table; no requires-python")
	assert.Equal(t, "pypi", r.Project.Ecosystem)

	assert.Equal(t, 3, r.Summary.Total)
	assert.Equal(t, 3, r.Summary.Direct)

	assert.ElementsMatch(t,
		[]string{"pkg:pypi/click", "pkg:pypi/pyyaml", "pkg:pypi/pytest"},
		r.Summary.NeedsReview,
		"all three deps from [dependency-groups] surface for review")
}

// TestRun_PyProjectTOML_Poetry_PureLegacy_SmokeFixture is the
// permanent smoke test for pure Poetry with the legacy
// [tool.poetry.dev-dependencies] form. The fixture mirrors the
// shape of Textualize/rich, verified 2026-04-26 via WebFetch.
func TestRun_PyProjectTOML_Poetry_PureLegacy_SmokeFixture(t *testing.T) {
	t.Parallel()

	fixturePath, err := filepath.Abs("../manifest/pypi/testdata/poetry-pure-legacy/pyproject.toml")
	require.NoError(t, err)

	s := openTestStore(t)
	r, err := Run(context.Background(), s, fixturePath)
	require.NoError(t, err)

	// Metadata flows from [tool.poetry] when PEP 621 is absent.
	assert.Equal(t, "pure-legacy-pkg", r.Project.Name)
	assert.Equal(t, "^3.10", r.Project.EcoVersion)
	assert.Equal(t, "pypi", r.Project.Ecosystem)

	// 4 deps per the fixture's documented breakdown:
	// requests + click (main) + pytest + black (dev-dependencies).
	// python is the runtime pin and not surfaced as a dep.
	assert.Equal(t, 4, r.Summary.Total)
	assert.Equal(t, 4, r.Summary.Direct)

	assert.ElementsMatch(t,
		[]string{"pkg:pypi/requests", "pkg:pypi/click", "pkg:pypi/pytest", "pkg:pypi/black"},
		r.Summary.NeedsReview)
}

// TestRun_PyProjectTOML_Poetry_PureModern_SmokeFixture is the
// permanent smoke test for pure Poetry with the modern
// [tool.poetry.group.*.dependencies] form. No confirmed real-world
// target in this exact shape — projects that adopted modern groups
// generally also migrated to PEP 621 (becoming hybrid).
func TestRun_PyProjectTOML_Poetry_PureModern_SmokeFixture(t *testing.T) {
	t.Parallel()

	fixturePath, err := filepath.Abs("../manifest/pypi/testdata/poetry-pure-modern/pyproject.toml")
	require.NoError(t, err)

	s := openTestStore(t)
	r, err := Run(context.Background(), s, fixturePath)
	require.NoError(t, err)

	assert.Equal(t, "pure-modern-pkg", r.Project.Name)
	assert.Equal(t, "^3.10", r.Project.EcoVersion)

	assert.Equal(t, 4, r.Summary.Total)
	assert.ElementsMatch(t,
		[]string{"pkg:pypi/requests", "pkg:pypi/pytest", "pkg:pypi/coverage", "pkg:pypi/sphinx"},
		r.Summary.NeedsReview)
}

// TestRun_PyProjectTOML_Poetry_Hybrid_SmokeFixture is the permanent
// smoke test for the hybrid configuration that broke v1/v2's
// "Poetry as fallback" framing. Mirrors the shape of
// python-poetry/poetry itself, verified 2026-04-26 via WebFetch.
//
// Pre-v3 the parser would have routed [project]-bearing files
// through PEP 621 only and silently dropped the
// [tool.poetry.group.*.dependencies] dev/test deps. The v3
// architecture (Poetry as third independent handler) catches them.
func TestRun_PyProjectTOML_Poetry_Hybrid_SmokeFixture(t *testing.T) {
	t.Parallel()

	fixturePath, err := filepath.Abs("../manifest/pypi/testdata/poetry-hybrid/pyproject.toml")
	require.NoError(t, err)

	s := openTestStore(t)
	r, err := Run(context.Background(), s, fixturePath)
	require.NoError(t, err)

	// PEP 621 wins for metadata in the hybrid case.
	assert.Equal(t, "hybrid-pkg", r.Project.Name)
	assert.Equal(t, ">=3.10", r.Project.EcoVersion)

	// 4 deps total: 2 from [project].dependencies (requests, click)
	// AND 2 from [tool.poetry.group.dev.dependencies] (pytest, mypy).
	// The Poetry deps surfacing here is the regression catch — pre-v3
	// the parser would have stopped after PEP 621 and missed them.
	assert.Equal(t, 4, r.Summary.Total)
	assert.ElementsMatch(t,
		[]string{"pkg:pypi/requests", "pkg:pypi/click", "pkg:pypi/pytest", "pkg:pypi/mypy"},
		r.Summary.NeedsReview,
		"both PEP 621 main deps AND Poetry group dev deps surface (the v3 architecture's whole point)")
}

// TestParseGraph_PyPIReturnsGraphUnavailable covers parseGraph's
// dispatch for the three PyPI filenames. Graph extraction isn't
// implemented for any PyPI manifest yet (lockfile parsing —
// poetry.lock / uv.lock — is the path). All three should land on
// ErrGraphUnavailable with the pypi-specific message rather than
// the "unrecognized" default.
func TestParseGraph_PyPIReturnsGraphUnavailable(t *testing.T) {
	t.Parallel()
	for _, base := range []string{"requirements.txt", "pyproject.toml", "setup.py"} {
		t.Run(base, func(t *testing.T) {
			t.Parallel()
			_, err := parseGraph(context.Background(), filepath.Join("/some/dir", base))
			require.Error(t, err)
			assert.ErrorIs(t, err, manifest.ErrGraphUnavailable)
			assert.Contains(t, err.Error(), "pypi",
				"error should attribute the gap to pypi, not 'unrecognized'")
		})
	}
}

// TestPostureTierMapping locks in the profile → survey enum
// mapping. Any future rename in either enum must update this
// test too; the explicit assertion surfaces the drift.
func TestPostureTierMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   profile.PostureTier
		want Tier
	}{
		{profile.PostureVettedFrozen, TierVettedFrozen},
		{profile.PostureTrustedForNow, TierTrustedForNow},
		{profile.PostureUnexamined, TierUnexamined},
		{profile.PostureUnknownProvenance, TierUnknownProvenance},
		{profile.PostureRejected, TierRejected},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, postureTierToSurveyTier(tc.in))
		})
	}

	// Unknown posture tier (e.g., from a future schema) falls
	// back to unexamined rather than panicking.
	assert.Equal(t, TierUnexamined, postureTierToSurveyTier(profile.PostureTier("future-tier")))
}

// ---- helpers ----

func lastSegment(uri string) string {
	for i := len(uri) - 1; i >= 0; i-- {
		if uri[i] == '/' || uri[i] == ':' {
			return uri[i+1:]
		}
	}
	return uri
}

// ---- mock store for error-injection tests ----

// burnErrStore wraps a real SQLite store but overrides GetBurn AND
// EffectiveBurn to return an injected non-ErrNotFound error for a
// specific entityID. All other methods delegate to the real store so
// entity and posture setup still works.
//
// Survey moved to EffectiveBurn at Path B; the GetBurn override is
// kept for symmetry (any caller still on GetBurn would be exercising
// the same fault contract). EffectiveBurn is the production path
// survey actually uses, and this is what the propagation test pins.
type burnErrStore struct {
	store.Store
	burnEntityID string
	burnErr      error
}

func (b *burnErrStore) GetBurn(ctx context.Context, entityID string) (*profile.Burn, error) {
	if entityID == b.burnEntityID {
		return nil, b.burnErr
	}
	return b.Store.GetBurn(ctx, entityID)
}

func (b *burnErrStore) EffectiveBurn(ctx context.Context, entityID string) (*profile.Burn, *store.EffectiveBurnContext, error) {
	if entityID == b.burnEntityID {
		return nil, nil, b.burnErr
	}
	return b.Store.EffectiveBurn(ctx, entityID)
}

// findErrStore wraps a real SQLite store and overrides
// FindEntityByURI to return an injected non-ErrNotFound error.
type findErrStore struct {
	store.Store
	targetURI string
	findErr   error
}

func (f *findErrStore) FindEntityByURI(ctx context.Context, uri string) (*profile.Entity, error) {
	if uri == f.targetURI {
		return nil, f.findErr
	}
	return f.Store.FindEntityByURI(ctx, uri)
}

// postureErrStore wraps a real SQLite store and overrides
// GetPostures to return an injected non-ErrNotFound error.
type postureErrStore struct {
	store.Store
	postureEntityID string
	postureErr      error
}

func (p *postureErrStore) GetPostures(ctx context.Context, entityID string) ([]profile.Posture, error) {
	if entityID == p.postureEntityID {
		return nil, p.postureErr
	}
	return p.Store.GetPostures(ctx, entityID)
}

// TestRun_GetBurnStorageError_PropagatesError verifies that a
// non-ErrNotFound error from GetBurn is surfaced by Run rather
// than silently falling through to a benign tier (the bug).
//
// Before the fix this test fails: Run returns (result, nil) with
// the dep rendered as TierNotInStore instead of propagating the
// storage error.
func TestRun_GetBurnStorageError_PropagatesError(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/burn-err-test

go 1.25.1

require github.com/compromised/lib v1.0.0
`)

	real := openTestStore(t)
	e := seedEntity(t, real, "repo:github/compromised/lib")

	storageErr := errors.New("disk I/O failure")
	s := &burnErrStore{
		Store:        real,
		burnEntityID: e.ID,
		burnErr:      storageErr,
	}

	_, err := Run(context.Background(), s, path)
	require.Error(t, err, "storage error from GetBurn must propagate; got nil")
	assert.ErrorIs(t, err, storageErr)
}

// TestRun_FindEntityStorageError_PropagatesError verifies that a
// non-ErrNotFound error from FindEntityByURI is surfaced by Run.
func TestRun_FindEntityStorageError_PropagatesError(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/find-err-test

go 1.25.1

require github.com/some/dep v1.0.0
`)

	real := openTestStore(t)
	storageErr := errors.New("connection pool exhausted")
	s := &findErrStore{
		Store:     real,
		targetURI: "repo:github/some/dep",
		findErr:   storageErr,
	}

	_, err := Run(context.Background(), s, path)
	require.Error(t, err, "storage error from FindEntityByURI must propagate; got nil")
	assert.ErrorIs(t, err, storageErr)
}

// TestRun_GetPosturesStorageError_PropagatesError verifies that a
// non-ErrNotFound error from GetPostures is surfaced by Run.
func TestRun_GetPosturesStorageError_PropagatesError(t *testing.T) {
	t.Parallel()

	path := writeGoMod(t, `module github.com/example/posture-err-test

go 1.25.1

require github.com/some/dep v1.5.0
`)

	real := openTestStore(t)
	e := seedEntity(t, real, "repo:github/some/dep")

	storageErr := errors.New("schema migration pending")
	s := &postureErrStore{
		Store:           real,
		postureEntityID: e.ID,
		postureErr:      storageErr,
	}

	_, err := Run(context.Background(), s, path)
	require.Error(t, err, "storage error from GetPostures must propagate; got nil")
	assert.ErrorIs(t, err, storageErr)
}
