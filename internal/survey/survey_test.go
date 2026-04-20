package survey

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		[]string{"repo:github/alecthomas/kong", "pkg:go/gopkg.in/yaml.v3"},
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
	assert.False(t, d.HasOtherVersions)

	// NeedsReview should be empty — vetted-frozen is a
	// resolved-to-positive decision, not something to review.
	assert.Empty(t, r.Summary.NeedsReview)
	assert.Equal(t, 1, r.Summary.ByTier[TierVettedFrozen])
}

// TestRun_PostureForOtherVersionSetsFlag — pinned v1.15.0 but
// the store has a posture for v1.14.0. Tier is unexamined with
// HasOtherVersions=true, so renderers can show the "but v1.14 is
// vetted-frozen" hint.
func TestRun_PostureForOtherVersionSetsFlag(t *testing.T) {
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
	assert.True(t, d.HasOtherVersions,
		"version mismatch should flip HasOtherVersions to true")
	assert.Empty(t, d.PostureVersion,
		"PostureVersion is only populated on an exact match")

	// The pinned version is still in NeedsReview because it lacks
	// an exact posture decision.
	assert.Contains(t, r.Summary.NeedsReview, "repo:github/alecthomas/kong")
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
// nothing-happens on typos like `survey --manifest Gemfile`.
func TestRun_UnrecognizedManifest(t *testing.T) {
	t.Parallel()

	// Write a fake Gemfile content to a path — doesn't matter if
	// the content is valid, parseManifest rejects at the filename.
	dir := t.TempDir()
	gemfile := filepath.Join(dir, "Gemfile")
	require.NoError(t, os.WriteFile(gemfile, []byte("source 'https://rubygems.org'\n"), 0o600))

	s := openTestStore(t)
	_, err := Run(context.Background(), s, gemfile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unrecognized manifest")
	assert.Contains(t, err.Error(), "go.mod")
}

func TestRun_EmptyManifestPath(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	_, err := Run(context.Background(), s, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required")
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
