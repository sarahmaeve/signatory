package git

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Path F (entity-burn1.md "Pending work #2", GPG-only slice): the
// git collector mints identity:gpg/<keyid> entity rows for every
// distinct per-developer key that signed a commit in the
// observation window, and emits a commit_signing_keys signal
// listing those keys so the cascade resolver can walk them at
// read time.
//
// Web-flow keys (GitHub's managed signing key) are EXCLUDED from
// minting — they're platform-managed credentials, not per-
// developer identities, and minting an entity for them would
// conflate platform trust with individual signer trust.
//
// Mirrors the github (Path A), npm (Path C), and pypi (Path E)
// patterns: same EntityStore narrow-interface, same
// WithEntityStore setter, same nil-safe contract.

// TestExtractPerDeveloperKeyIDs covers the helper that walks
// classified signing rows and returns the distinct per-developer
// key IDs. Deterministic ordering — the slice's stability matters
// for both signal-row content (downstream consumers compare) and
// test reproducibility.
func TestExtractPerDeveloperKeyIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rows []commitSigningRow
		want []string
	}{
		{
			name: "empty input → empty result, not nil",
			rows: nil,
			want: nil,
		},
		{
			name: "single per-developer key",
			rows: []commitSigningRow{
				{SignatureStatus: "G", KeyID: "DEADBEEFCAFEBABE"},
			},
			want: []string{"deadbeefcafebabe"},
		},
		{
			name: "multiple distinct per-developer keys, lexicographic order",
			rows: []commitSigningRow{
				{SignatureStatus: "G", KeyID: "BBBB000011112222"},
				{SignatureStatus: "G", KeyID: "AAAA000011112222"},
				{SignatureStatus: "G", KeyID: "CCCC000011112222"},
			},
			want: []string{
				"aaaa000011112222",
				"bbbb000011112222",
				"cccc000011112222",
			},
		},
		{
			name: "duplicates dedupe",
			rows: []commitSigningRow{
				{SignatureStatus: "G", KeyID: "DEADBEEFCAFEBABE"},
				{SignatureStatus: "G", KeyID: "DEADBEEFCAFEBABE"},
				{SignatureStatus: "G", KeyID: "DEADBEEFCAFEBABE"},
			},
			want: []string{"deadbeefcafebabe"},
		},
		{
			name: "lowercased canonical form (git emits uppercase hex)",
			rows: []commitSigningRow{
				{SignatureStatus: "G", KeyID: "ABCDEF0123456789"},
			},
			want: []string{"abcdef0123456789"},
		},
		{
			name: "web-flow keys excluded",
			rows: []commitSigningRow{
				{SignatureStatus: "G", KeyID: "B5690EEEBB952194"}, // current web-flow
				{SignatureStatus: "G", KeyID: "4AEE18F83AFDEB23"}, // older web-flow
				{SignatureStatus: "G", KeyID: "DEADBEEFCAFEBABE"}, // per-dev
			},
			want: []string{"deadbeefcafebabe"},
		},
		{
			name: "unsigned commits contribute no key",
			rows: []commitSigningRow{
				{SignatureStatus: "N", KeyID: ""},
				{SignatureStatus: "B", KeyID: "TAMPEREDXXXXXXXX"}, // bad sig, key ignored
				{SignatureStatus: "E", KeyID: "UNCHECKABLEXXXXX"}, // can't check, key ignored
				{SignatureStatus: "R", KeyID: "REVOKEDXXXXXXXXX"}, // revoked, explicit no-trust
				{SignatureStatus: "G", KeyID: "VALIDXXXXXXXXXXX"},
			},
			want: []string{"validxxxxxxxxxxx"},
		},
		{
			name: "empty key ID with G status produces no candidate",
			rows: []commitSigningRow{
				{SignatureStatus: "G", KeyID: ""}, // odd git output — falls to per-dev classification but no key
				{SignatureStatus: "G", KeyID: "REALKEYXXXXXXXXX"},
			},
			want: []string{"realkeyxxxxxxxxx"},
		},
		{
			name: "key ID surrounded by whitespace gets trimmed",
			rows: []commitSigningRow{
				{SignatureStatus: "G", KeyID: "  DEADBEEFCAFEBABE  "},
			},
			want: []string{"deadbeefcafebabe"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractPerDeveloperKeyIDs(tc.rows)
			assert.Equal(t, tc.want, got)
		})
	}
}

// fakeEntityStore satisfies git.EntityStore for tests, recording
// every EnsureEntityByCanonicalURI call. Mirrors the github / npm /
// pypi fakeEntityStore pattern — three identical patterns is the
// hoist threshold; if a fifth collector needs the same shape, hoist
// to a shared test helper.
type fakeEntityStore struct {
	calls    []ensureCall
	existing map[string]*profile.Entity
}

type ensureCall struct {
	URI       string
	ShortName string
}

func newFakeEntityStore() *fakeEntityStore {
	return &fakeEntityStore{existing: map[string]*profile.Entity{}}
}

func (f *fakeEntityStore) EnsureEntityByCanonicalURI(_ context.Context, uri, shortName string) (*profile.Entity, bool, error) {
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

// TestEnsureSignerEntities_MintsIdentityGpgRows pins the basic
// minting case: a list of distinct per-developer key IDs produces
// one identity:gpg/<keyid> entity row per key, with EntityIdentity
// type, lowercased canonical URI.
func TestEnsureSignerEntities_MintsIdentityGpgRows(t *testing.T) {
	t.Parallel()

	store := newFakeEntityStore()
	c := NewCollector("/test").WithEntityStore(store)

	c.ensureSignerEntities(context.Background(),
		[]string{"deadbeefcafebabe", "0123456789abcdef"})

	for _, keyID := range []string{"deadbeefcafebabe", "0123456789abcdef"} {
		uri := "identity:gpg/" + keyID
		minted, ok := store.existing[uri]
		require.True(t, ok, "%s must be minted; existing=%v", uri, existingURIs(store.existing))
		assert.Equal(t, profile.EntityIdentity, minted.Type,
			"GPG signer entities must carry Type=EntityIdentity")
		assert.Equal(t, keyID, minted.ShortName,
			"the key ID flows through as the entity's short name")
	}
}

// TestEnsureSignerEntities_NilStore_DoesNotPanic pins the nil-
// safety contract: pre-EntityStore tests construct collectors
// without a store and must continue to pass.
func TestEnsureSignerEntities_NilStore_DoesNotPanic(t *testing.T) {
	t.Parallel()

	c := NewCollector("/test") // no .WithEntityStore call

	// Should not panic, should not error.
	c.ensureSignerEntities(context.Background(),
		[]string{"deadbeefcafebabe"})
}

// TestEnsureSignerEntities_EmptyKeyList_NoCalls pins that an empty
// key list produces zero EnsureEntityByCanonicalURI calls — the
// collector must not speculate.
func TestEnsureSignerEntities_EmptyKeyList_NoCalls(t *testing.T) {
	t.Parallel()

	store := newFakeEntityStore()
	c := NewCollector("/test").WithEntityStore(store)

	c.ensureSignerEntities(context.Background(), nil)
	assert.Empty(t, store.calls)

	c.ensureSignerEntities(context.Background(), []string{})
	assert.Empty(t, store.calls)
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
