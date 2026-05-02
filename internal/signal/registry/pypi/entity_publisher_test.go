package pypi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Path C-for-pypi: the pypi collector mints identity:pypi/<login>
// entities for every login extractable from a project's
// publisher-supplied metadata (Info.Maintainer, Info.Author legacy
// fields plus PEP 639 Info.Maintainers list). Mirrors the github
// (Path A) and npm (Path C) shapes; uses the same EntityStore
// narrow-interface pattern so production wiring threads the same
// *store.SQLite to all three.
//
// After this lands:
//   - Every analyze --refresh of pkg:pypi/X populates rows for the
//     login-shaped maintainers PyPI exposes.
//   - signatory burn add identity:pypi/<login> attaches to a row
//     that already exists rather than minting an empty stub.
//   - The cascade resolver's pypi-registry source dispatch (Phase D)
//     can find these identities on first contact, so a brand-new
//     analyze of any package the burned operator publishes refuses
//     collection at the gate.

// fakePublisherStore satisfies pypi.EntityStore for tests, recording
// every EnsureEntityByCanonicalURI call. Mirrors github's and npm's
// fakeEntityStore — three identical patterns is the threshold; if a
// fourth collector needs the same shape, hoist this to a shared
// helper.
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

// TestCollector_PublisherEntities_MintedForMaintainer pins the
// dominant case: a single login-shaped value in info.maintainer
// produces one identity:pypi/<login> entity row.
func TestCollector_PublisherEntities_MintedForMaintainer(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{Maintainer: "ofek"})
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), pypiEntity("hatch"))
	require.NoError(t, err)

	_, minted := store.existing["identity:pypi/ofek"]
	assert.True(t, minted,
		"identity:pypi/ofek must be minted from the Maintainer field; existing=%v", existingURIs(store.existing))
}

// TestCollector_PublisherEntities_MintedForMultipleMaintainers
// pins the multi-login case: a comma-separated maintainer field
// AND a PEP 639 Maintainers list together produce one entity per
// distinct login (deduplicated when the same login appears in both
// sources).
func TestCollector_PublisherEntities_MintedForMultipleMaintainers(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{
		Maintainer:  "alice, bob",
		Maintainers: []Person{{Name: "alice"}, {Name: "charlie"}},
	})
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), pypiEntity("multi"))
	require.NoError(t, err)

	for _, login := range []string{"alice", "bob", "charlie"} {
		_, ok := store.existing["identity:pypi/"+login]
		assert.True(t, ok, "identity:pypi/%s must be minted; existing=%v",
			login, existingURIs(store.existing))
	}
}

// TestCollector_PublisherEntities_TypeIsIdentity confirms minted
// rows carry EntityIdentity. The fake's EntityTypeForURI dispatch
// mirrors what production's EnsureEntityByCanonicalURI does.
func TestCollector_PublisherEntities_TypeIsIdentity(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{Maintainer: "ofek"})
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), pypiEntity("hatch"))
	require.NoError(t, err)

	for uri, ent := range store.existing {
		assert.Equal(t, profile.EntityIdentity, ent.Type,
			"pypi publisher entity %s must carry Type=EntityIdentity", uri)
	}
}

// TestCollector_PublisherEntities_NilStore_DoesNotMint pins the
// nil-safety contract: a collector constructed without
// WithEntityStore (the existing default for any test that doesn't
// care about minting) must not panic and must not mint. Critical
// for backwards-compat with the broader test suite.
func TestCollector_PublisherEntities_NilStore_DoesNotMint(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{Maintainer: "ofek"})
	defer srv.Close()

	c := newTestCollector(srv) // no .WithEntityStore call

	_, err := c.Collect(context.Background(), pypiEntity("hatch"))
	require.NoError(t, err,
		"collector with no entity store must complete without error or panic")
}

// TestCollector_PublisherEntities_DisplayNameDoesNotMint pins the
// "best-effort, conservative" stance: a project whose info.author
// is a free-text display name ("Saurabh Kumar") must not mint
// identity:pypi/saurabh kumar. Refusing to mint when the value is
// ambiguous beats fabricating non-existent identities — a future
// burn add against the actual login should hit a row that doesn't
// exist yet rather than collide with a polluted display-name row.
func TestCollector_PublisherEntities_DisplayNameDoesNotMint(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{Author: "Saurabh Kumar"})
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), pypiEntity("python-dotenv"))
	require.NoError(t, err)

	assert.Empty(t, store.existing,
		"a display-name-only project must not produce any minted entities; existing=%v", existingURIs(store.existing))
}

// TestCollector_PublisherEntities_LowercasesLogin pins the canonical
// URI lowercase rule: a Maintainer string with mixed-case content
// produces a lowercase URI. PyPI is case-insensitive on package
// names but the publisher metadata is publisher-typed and may
// preserve case; canonical URIs collapse case to prevent
// fragmentation across analyses.
func TestCollector_PublisherEntities_LowercasesLogin(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{Maintainer: "Konstin"})
	defer srv.Close()

	store := newFakePublisherStore()
	c := newTestCollector(srv).WithEntityStore(store)

	_, err := c.Collect(context.Background(), pypiEntity("ruff"))
	require.NoError(t, err)

	_, mintedLower := store.existing["identity:pypi/konstin"]
	_, mintedRaw := store.existing["identity:pypi/Konstin"]
	assert.True(t, mintedLower,
		"canonical URI must be lowercased to prevent case fragmentation across analyses")
	assert.False(t, mintedRaw,
		"the wire-form publisher name must NOT land as the canonical URI")
}

// existingURIs is a tiny formatter helper — surfaces the keys of
// the existing-entities map in error messages without dragging
// fmt.Sprintf chains into every assertion.
func existingURIs(m map[string]*profile.Entity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
