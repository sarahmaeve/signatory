package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// EnsureEntityByCanonicalURI is the "find-or-mint" helper Path A
// adds so collectors can populate identity:/org: entity rows for
// the publishers/owners they observe, without each call site
// reimplementing the find-then-create dance.
//
// The helper derives Type from the URI scheme via
// profile.EntityTypeForURI (the single source of truth landed in
// PR0); callers don't pass an EntityType. This decouples collector
// code from "is github User → identity, Organization → org" type
// reasoning — the URI scheme tells the helper everything.

// TestSQLite_EnsureEntityByCanonicalURI_FreshURI_MintsRow covers
// the create branch: a URI not in the store gets a fresh entity
// row with EntityTypeForURI-derived Type, the supplied short name,
// and the bool return signals creation.
func TestSQLite_EnsureEntityByCanonicalURI_FreshURI_MintsRow(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	entity, created, err := s.EnsureEntityByCanonicalURI(t.Context(),
		"identity:github/operator-x", "operator-x")
	require.NoError(t, err)
	require.NotNil(t, entity)

	assert.True(t, created,
		"first call on a never-seen URI must report created=true so audit-loggers know which side fired")
	assert.Equal(t, "identity:github/operator-x", entity.CanonicalURI)
	assert.Equal(t, profile.EntityIdentity, entity.Type,
		"Type must derive from the URI scheme via EntityTypeForURI — `identity:` → EntityIdentity")
	assert.Equal(t, "operator-x", entity.ShortName)
	assert.NotEmpty(t, entity.ID, "minted entity must carry a fresh UUID")
	assert.False(t, entity.CreatedAt.IsZero(), "CreatedAt must be stamped")
	assert.False(t, entity.UpdatedAt.IsZero(), "UpdatedAt must be stamped")

	// Re-read via FindEntityByURI to confirm the row landed in the
	// store (not just returned in-memory).
	loaded, err := s.FindEntityByURI(t.Context(), "identity:github/operator-x")
	require.NoError(t, err)
	assert.Equal(t, entity.ID, loaded.ID,
		"persisted row must match the freshly-minted entity's id")
	assert.Equal(t, profile.EntityIdentity, loaded.Type,
		"persisted Type must match — store row, not just in-memory struct")
}

// TestSQLite_EnsureEntityByCanonicalURI_OrgURI_MintsOrgType is the
// org: scheme parallel — confirms the URI-scheme→Type mapping
// works for organization entities too.
func TestSQLite_EnsureEntityByCanonicalURI_OrgURI_MintsOrgType(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	entity, created, err := s.EnsureEntityByCanonicalURI(t.Context(),
		"org:github/some-org", "some-org")
	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, profile.EntityOrg, entity.Type,
		"`org:` URIs must produce EntityOrg rows")
	assert.Equal(t, "some-org", entity.ShortName)
}

// TestSQLite_EnsureEntityByCanonicalURI_ExistingURI_ReturnsRow
// covers the find branch: a URI already in the store returns the
// existing row with created=false. No mutation.
func TestSQLite_EnsureEntityByCanonicalURI_ExistingURI_ReturnsRow(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	first, created1, err := s.EnsureEntityByCanonicalURI(t.Context(),
		"identity:github/operator-x", "operator-x")
	require.NoError(t, err)
	require.True(t, created1, "first call mints")

	second, created2, err := s.EnsureEntityByCanonicalURI(t.Context(),
		"identity:github/operator-x", "operator-x")
	require.NoError(t, err)
	assert.False(t, created2,
		"second call must return created=false — the entity already exists")
	assert.Equal(t, first.ID, second.ID,
		"must return the same UUID, not a fresh one")
	assert.Equal(t, first.CreatedAt, second.CreatedAt,
		"CreatedAt must NOT change on a find — only the original creation timestamp matters")
}

// TestSQLite_EnsureEntityByCanonicalURI_DifferentShortName_PreservesExisting
// pins the contract that the helper is "find OR mint", not "find AND
// update". A second call with a different shortName must return the
// existing row unchanged — the shortName is a creation-time hint, not
// an update directive. Callers that want to refresh metadata use
// PutEntity directly.
func TestSQLite_EnsureEntityByCanonicalURI_DifferentShortName_PreservesExisting(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	first, _, err := s.EnsureEntityByCanonicalURI(t.Context(),
		"identity:github/operator-x", "first-name")
	require.NoError(t, err)

	second, created, err := s.EnsureEntityByCanonicalURI(t.Context(),
		"identity:github/operator-x", "second-name")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, "first-name", second.ShortName,
		"existing ShortName must be preserved — the helper does not update on rediscovery")
}

// TestSQLite_EnsureEntityByCanonicalURI_InvalidURI_RejectsBeforeMint
// asserts the validation boundary: a URI that fails
// profile.ValidateCanonicalURI must return an error rather than
// minting a malformed row. Defense for the same persistence-boundary
// invariant PutEntity enforces directly.
func TestSQLite_EnsureEntityByCanonicalURI_InvalidURI_RejectsBeforeMint(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	// ValidateCanonicalURI's actual rejections (uri.go:99-140):
	// empty, over-length, leading/trailing whitespace, control chars,
	// non-ASCII, unknown scheme, empty body after scheme. Internal
	// spaces in the path segment are NOT rejected at this layer
	// (they're a structural-validity concern handled higher up by
	// the per-scheme constructors); the helper's contract is "fail
	// at the canonical-URI persistence boundary," which is exactly
	// the set of rules ValidateCanonicalURI enforces.
	cases := []string{
		"",                           // empty
		"not-a-scheme/foo",           // unknown scheme
		"identity:",                  // empty body after scheme
		"identity:github/føø",        // non-ASCII byte
		"identity:github/foo\x01bar", // control character
	}
	for _, uri := range cases {
		t.Run(uri, func(t *testing.T) {
			t.Parallel()
			_, _, err := s.EnsureEntityByCanonicalURI(t.Context(), uri, "x")
			require.Error(t, err,
				"invalid URI must return an error rather than mint a malformed row")
		})
	}
}
