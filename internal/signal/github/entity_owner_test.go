package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Path A: the github collector mints identity:github/<login> or
// org:github/<login> entities for the owner of every analyzed repo,
// derived from the same `ownerUser` API response that already feeds
// the owner_profile signal. After this lands, every analyze run
// against a github-hosted target also populates the corresponding
// owner-entity row, so a subsequent `signatory burn add identity:
// github/<login>` attaches to a row that already exists rather than
// minting an empty stub.
//
// The collector takes an optional EntityStore via the WithEntityStore
// setter — nil-safe, so existing tests that don't wire a store
// continue to pass without modification.

// fakeEntityStore satisfies github.EntityStore for tests. It records
// every EnsureEntityByCanonicalURI call so assertions can pin the
// exact (uri, shortName) pairs the collector emitted.
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

// TestCollector_OwnerEntity_MintedForUserType pins the User-flavour
// of owner-entity minting: a repo whose owner is `Type:"User"` in
// the GitHub API response causes an `identity:github/<login>` row
// to be ensured. Uses the existing mockGitHubAPI fixture (kong /
// alecthomas / Type=User).
func TestCollector_OwnerEntity_MintedForUserType(t *testing.T) {
	store := newFakeEntityStore()
	c := newTestCollector(t, mockGitHubAPI()).WithEntityStore(store)

	entity := &profile.Entity{
		ID:        "test-kong",
		Type:      profile.EntityProject,
		ShortName: "alecthomas/kong",
		URL:       "https://github.com/alecthomas/kong",
	}
	_, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Owner entity ensure-call must have fired with the canonical
	// identity URI (lowercased per CanonicalIdentityURI's contract).
	var ownerCall *ensureCall
	for i := range store.calls {
		if store.calls[i].URI == "identity:github/alecthomas" {
			ownerCall = &store.calls[i]
			break
		}
	}
	require.NotNil(t, ownerCall,
		"expected EnsureEntityByCanonicalURI call for identity:github/alecthomas; got calls: %+v", store.calls)
	assert.Equal(t, "alecthomas", ownerCall.ShortName,
		"the github login flows through as the entity's short name")

	// Verify the minted entity carries EntityIdentity (post-PR0
	// EntityTypeForURI dispatch).
	minted, ok := store.existing["identity:github/alecthomas"]
	require.True(t, ok, "owner entity must land in the store")
	assert.Equal(t, profile.EntityIdentity, minted.Type,
		"User-type owners must produce EntityIdentity rows")
}

// orgOwnerHandler returns an httptest handler serving the minimum
// endpoints owner-entity collection needs, with Owner.Type set to
// "Organization". Bypasses the rich mockGitHubAPI fixture — we
// only care about the owner-entity branch here, not the full
// signal panel.
func orgOwnerHandler(repoOwnerLogin, repoName string) http.Handler {
	mux := http.NewServeMux()
	repoPath := "/repos/" + repoOwnerLogin + "/" + repoName
	mux.HandleFunc(repoPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(repo{
			Name:     repoName,
			FullName: repoOwnerLogin + "/" + repoName,
			Owner:    repoOwner{Login: repoOwnerLogin, Type: "Organization"},
		})
	})
	mux.HandleFunc("/users/"+repoOwnerLogin, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(user{
			Login:     repoOwnerLogin,
			Type:      "Organization",
			CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	})
	// Stub other endpoints with empty responses so the collector
	// records absences rather than failing the test on unrelated
	// signals.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

// TestCollector_OwnerEntity_MintedForOrgType is the Organization
// parallel to the User test. The same code path picks the URI
// scheme from ownerUser.Type — User → identity:, Organization → org:.
func TestCollector_OwnerEntity_MintedForOrgType(t *testing.T) {
	store := newFakeEntityStore()
	srv := httptest.NewServer(orgOwnerHandler("vitest-dev", "vitest"))
	t.Cleanup(srv.Close)

	client := NewClientWithBaseURLAndToken(srv.URL, "test-token")
	c := NewCollectorWithClient(client).WithEntityStore(store)

	entity := &profile.Entity{
		ID:        "test-vitest",
		Type:      profile.EntityProject,
		ShortName: "vitest-dev/vitest",
		URL:       "https://github.com/vitest-dev/vitest",
	}
	_, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	var ownerCall *ensureCall
	for i := range store.calls {
		if store.calls[i].URI == "org:github/vitest-dev" {
			ownerCall = &store.calls[i]
			break
		}
	}
	require.NotNil(t, ownerCall,
		"expected EnsureEntityByCanonicalURI call for org:github/vitest-dev; got calls: %+v", store.calls)
	assert.Equal(t, "vitest-dev", ownerCall.ShortName)

	minted, ok := store.existing["org:github/vitest-dev"]
	require.True(t, ok, "org entity must land in the store")
	assert.Equal(t, profile.EntityOrg, minted.Type,
		"Organization-type owners must produce EntityOrg rows, not EntityIdentity")
}

// TestCollector_OwnerEntity_NilStore_DoesNotMint pins the
// nil-safety contract: a collector constructed without
// WithEntityStore (the existing default for every test that
// pre-dates Path A) does not panic and does not attempt to
// mint owner entities. Critical for backwards-compat — every
// prior test in this package constructs collectors without a
// store, and they must continue to work.
func TestCollector_OwnerEntity_NilStore_DoesNotMint(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI()) // no .WithEntityStore call

	entity := &profile.Entity{
		ID:        "test-kong",
		Type:      profile.EntityProject,
		ShortName: "alecthomas/kong",
		URL:       "https://github.com/alecthomas/kong",
	}
	// The test passes if Collect completes without panic. The
	// existing TestCollector_Collect already exercises this path;
	// repeating the assertion here makes the nil-store contract
	// explicit and traceable.
	_, err := c.Collect(context.Background(), entity)
	require.NoError(t, err,
		"collector with no entity store must complete without error or panic")
}

// TestCollector_OwnerEntity_LowercasesLogin pins the canonicalisation
// contract: github logins on the wire preserve case (e.g.,
// "BufferZoneCorp"), but the canonical URI form lowercases per
// profile.CanonicalIdentityURI. Important because case fragmentation
// would create separate entity rows for the same logical operator
// (BufferZoneCorp / bufferzonecorp).
func TestCollector_OwnerEntity_LowercasesLogin(t *testing.T) {
	store := newFakeEntityStore()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/BufferZoneCorp/grpc-client", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(repo{
			Name:     "grpc-client",
			FullName: "BufferZoneCorp/grpc-client",
			Owner:    repoOwner{Login: "BufferZoneCorp", Type: "User"},
		})
	})
	mux.HandleFunc("/users/BufferZoneCorp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(user{
			Login:     "BufferZoneCorp",
			Type:      "User",
			CreatedAt: time.Date(2026, 4, 20, 12, 6, 24, 0, time.UTC),
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := NewClientWithBaseURLAndToken(srv.URL, "test-token")
	c := NewCollectorWithClient(client).WithEntityStore(store)

	entity := &profile.Entity{
		ID:        "test-bz",
		Type:      profile.EntityProject,
		ShortName: "BufferZoneCorp/grpc-client",
		URL:       "https://github.com/BufferZoneCorp/grpc-client",
	}
	_, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// The URI is lowercased — bufferzonecorp, not BufferZoneCorp.
	_, mintedLower := store.existing["identity:github/bufferzonecorp"]
	_, mintedRaw := store.existing["identity:github/BufferZoneCorp"]
	assert.True(t, mintedLower,
		"canonical URI must be lowercased to prevent case fragmentation across analyses")
	assert.False(t, mintedRaw,
		"the wire-form login must NOT land as the canonical URI — that would split one operator across two entity rows")
}
