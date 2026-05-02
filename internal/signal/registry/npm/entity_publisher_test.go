package npm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Path C: the npm collector mints identity:npm/<login> entities for
// every maintainer (top-level Maintainers list) and every per-version
// publisher (_npmUser.name) it observes. Mirrors Path A's github-side
// minting; uses the same EntityStore narrow-interface pattern so
// production wiring threads the same *store.SQLite to both.
//
// After this lands:
//   - Every analyze --refresh of pkg:npm/X populates rows for the
//     maintainers npm exposes via the registry.
//   - Lodash's three historical publishers (jdalton, bnjmnt4n,
//     mathias from publish_origin_consistency) become first-class
//     queryable entities, so an account-takeover-style burn on
//     jdalton has a row to attach to.

// fakePublisherStore satisfies npm.EntityStore for tests, recording
// every EnsureEntityByCanonicalURI call. Mirrors github's
// fakeEntityStore — could be hoisted to a shared test helper later
// if a third collector needs the same shape.
type fakePublisherStore struct {
	calls    []ensureCall
	existing map[string]*profile.Entity
}

type ensureCall struct {
	URI       string
	ShortName string
}

func newFakePublisherStore() *fakePublisherStore {
	return &fakePublisherStore{existing: map[string]*profile.Entity{}}
}

func (f *fakePublisherStore) EnsureEntityByCanonicalURI(_ context.Context, uri, shortName string) (*profile.Entity, bool, error) {
	f.calls = append(f.calls, ensureCall{URI: uri, ShortName: shortName})
	if e, ok := f.existing[uri]; ok {
		return e, false, nil
	}
	e := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: uri,
		Type:         profile.EntityTypeForURI(uri),
		ShortName:    shortName,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	f.existing[uri] = e
	return e, true, nil
}

const lodashLikeRegistryBody = `{
  "name": "lodash",
  "dist-tags": {"latest": "4.17.21"},
  "time": {
    "created": "2010-01-01T00:00:00Z",
    "4.17.21": "2021-02-20T00:00:00Z",
    "4.17.20": "2020-08-13T00:00:00Z",
    "4.17.19": "2020-07-08T00:00:00Z"
  },
  "maintainers": [
    {"name": "jdalton", "email": "j@example.com"}
  ],
  "versions": {
    "4.17.21": {"scripts": {}, "dist": {"attestations": null}, "_npmUser": {"name": "jdalton"}},
    "4.17.20": {"scripts": {}, "dist": {"attestations": null}, "_npmUser": {"name": "bnjmnt4n"}},
    "4.17.19": {"scripts": {}, "dist": {"attestations": null}, "_npmUser": {"name": "mathias"}}
  }
}`

const lodashLikeDownloadsBody = `{"downloads":50000000,"start":"2026-04-13","end":"2026-04-20","package":"lodash"}`

// TestCollector_PublisherEntities_MintedForMaintainers pins the
// top-level-Maintainers branch: every login in pkg.Maintainers
// produces an identity:npm/<login> entity row.
func TestCollector_PublisherEntities_MintedForMaintainers(t *testing.T) {
	srv := newMultiEndpointServer(t, lodashLikeRegistryBody, lodashLikeDownloadsBody)
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), npmEntity("lodash"))
	require.NoError(t, err)

	// jdalton appears in both Maintainers and as a version publisher;
	// the helper's idempotency means it gets minted once.
	_, mintedJDalton := store.existing["identity:npm/jdalton"]
	assert.True(t, mintedJDalton,
		"identity:npm/jdalton must be minted from the Maintainers list; existing=%v", existingURIs(store.existing))
}

// TestCollector_PublisherEntities_MintedForPerVersionPublishers
// pins the cross-version branch: every distinct _npmUser.name across
// the recent versions window produces an entity row, even when the
// login isn't in the current Maintainers list (the lodash shape:
// jdalton is the only current maintainer, but bnjmnt4n and mathias
// published older versions and remain identity-relevant for cascade).
func TestCollector_PublisherEntities_MintedForPerVersionPublishers(t *testing.T) {
	srv := newMultiEndpointServer(t, lodashLikeRegistryBody, lodashLikeDownloadsBody)
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), npmEntity("lodash"))
	require.NoError(t, err)

	// All three lodash publishers must materialise. The helper does
	// not deduplicate at the call layer — the fake's idempotency
	// (find-or-mint) absorbs repeats — so we assert on the unique
	// set rather than the call count.
	for _, login := range []string{"jdalton", "bnjmnt4n", "mathias"} {
		_, ok := store.existing["identity:npm/"+login]
		assert.True(t, ok, "identity:npm/%s must be minted from a version publisher; existing=%v",
			login, existingURIs(store.existing))
	}
}

// TestCollector_PublisherEntities_TypeIsIdentity confirms the
// EntityTypeForURI dispatch lands EntityIdentity on the minted rows
// (the fake stamps Type via the same helper, mirroring production).
func TestCollector_PublisherEntities_TypeIsIdentity(t *testing.T) {
	srv := newMultiEndpointServer(t, lodashLikeRegistryBody, lodashLikeDownloadsBody)
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), npmEntity("lodash"))
	require.NoError(t, err)

	for uri, ent := range store.existing {
		assert.Equal(t, profile.EntityIdentity, ent.Type,
			"npm publisher entity %s must carry Type=EntityIdentity", uri)
	}
}

// TestCollector_PublisherEntities_NilStore_DoesNotMint pins the
// nil-safety contract: a collector constructed without WithEntityStore
// (the existing default for every npm test that pre-dates Path C)
// does not panic and does not attempt to mint publisher entities.
// Critical for backwards-compat — every prior test in this package
// constructs collectors without a store via newTestCollector(srv),
// and they must continue to work unchanged.
func TestCollector_PublisherEntities_NilStore_DoesNotMint(t *testing.T) {
	srv := newMultiEndpointServer(t, lodashLikeRegistryBody, lodashLikeDownloadsBody)
	defer srv.Close()

	c := newTestCollector(srv) // no .WithEntityStore call

	_, err := c.Collect(context.Background(), npmEntity("lodash"))
	require.NoError(t, err,
		"collector with no entity store must complete without error or panic")
}

// TestCollector_PublisherEntities_NoMaintainersField pins behaviour
// when the registry response lacks a Maintainers list (some bare or
// orphaned packages omit the field entirely). The collector must
// still mint publisher entities for the per-version _npmUser
// branches, and must not panic on the nil maintainers slice.
func TestCollector_PublisherEntities_NoMaintainersField(t *testing.T) {
	bodyWithoutMaintainers := `{
	  "name": "lonely-pkg",
	  "dist-tags": {"latest": "1.0.0"},
	  "time": {"created": "2024-01-01T00:00:00Z", "1.0.0": "2024-01-01T00:00:00Z"},
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {"attestations": null}, "_npmUser": {"name": "solo-publisher"}}
	  }
	}`
	srv := newMultiEndpointServer(t, bodyWithoutMaintainers, `{"downloads":1}`)
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), npmEntity("lonely-pkg"))
	require.NoError(t, err)

	_, mintedSolo := store.existing["identity:npm/solo-publisher"]
	assert.True(t, mintedSolo,
		"per-version publisher must still mint when Maintainers is absent")
}

// existingURIs is a tiny formatter helper — surfaces the keys of
// the existing-entities map in error messages without dragging
// fmt.Sprintf chains into every assertion. Named to dodge the
// stdlib `maps` package the collector imports.
func existingURIs(m map[string]*profile.Entity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
