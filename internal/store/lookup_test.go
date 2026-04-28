package store

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// fakeLookuper is an in-memory EntityLookuper for hermetic tests of
// LookupEntity. Two map-driven dispatch tables let each test
// declaratively seed which URIs resolve to which entities, plus an
// optional injected error to exercise the propagation paths.
type fakeLookuper struct {
	byURI map[string]*profile.Entity
	// versioned maps a BASE URI to the entity that the
	// FindEntityByVersionedBaseURI scan should return for that base.
	// Empty / missing means "no @V sibling" → ErrNotFound.
	versioned map[string]*profile.Entity
	// injectErr, when non-nil, is returned from every lookup. Used
	// to exercise non-NotFound error propagation.
	injectErr error
	// counts track how many times each method ran — lets tests assert
	// the helper short-circuits on first hit and doesn't make extra
	// round-trips after success.
	findByURICalls, findVersionedCalls int
}

func (f *fakeLookuper) FindEntityByURI(_ context.Context, canonicalURI string) (*profile.Entity, error) {
	f.findByURICalls++
	if f.injectErr != nil {
		return nil, f.injectErr
	}
	if e, ok := f.byURI[canonicalURI]; ok {
		return e, nil
	}
	return nil, ErrNotFound
}

func (f *fakeLookuper) FindEntityByVersionedBaseURI(_ context.Context, baseURI string) (*profile.Entity, error) {
	f.findVersionedCalls++
	if f.injectErr != nil {
		return nil, f.injectErr
	}
	if e, ok := f.versioned[baseURI]; ok {
		return e, nil
	}
	return nil, ErrNotFound
}

func TestLookupEntity_ExactMatch(t *testing.T) {
	t.Parallel()
	want := &profile.Entity{ID: "ent-1", CanonicalURI: "repo:github/alecthomas/kong"}
	f := &fakeLookuper{byURI: map[string]*profile.Entity{
		"repo:github/alecthomas/kong": want,
	}}

	got, err := LookupEntity(context.Background(), f, "repo:github/alecthomas/kong")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 1, f.findByURICalls,
		"exact match must hit FindEntityByURI exactly once and short-circuit")
	assert.Equal(t, 0, f.findVersionedCalls,
		"versioned scan must NOT run when an exact match was found")
}

// TestLookupEntity_CrossSchemeGitHubAlternate covers the kong-class
// fragmentation: caller passes one canonical form, store has the
// equivalent under a different scheme. AlternateURIs encodes the
// equivalence; LookupEntity walks it.
func TestLookupEntity_CrossSchemeGitHubAlternate(t *testing.T) {
	t.Parallel()
	want := &profile.Entity{ID: "ent-real", CanonicalURI: "repo:github/alecthomas/kong"}
	f := &fakeLookuper{byURI: map[string]*profile.Entity{
		"repo:github/alecthomas/kong": want,
		// Note: pkg:go/github.com/alecthomas/kong is NOT in the map —
		// the helper must fall through to repo:github/ as an alternate.
	}}

	got, err := LookupEntity(context.Background(), f, "pkg:go/github.com/alecthomas/kong")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestLookupEntity_VanityResolution covers golang.org/x/Y →
// repo:github/golang/Y — the organizational-vanity case where signal-
// collection pipelines store the github-resolved form even though the
// caller's input is the import path.
func TestLookupEntity_VanityResolution(t *testing.T) {
	t.Parallel()
	want := &profile.Entity{ID: "ent-mod", CanonicalURI: "repo:github/golang/mod"}
	f := &fakeLookuper{byURI: map[string]*profile.Entity{
		"repo:github/golang/mod": want,
	}}

	got, err := LookupEntity(context.Background(), f, "pkg:go/golang.org/x/mod")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestLookupEntity_VersionedFallback covers the testify-class M1
// violation: caller passes the unversioned base, the store has the
// versioned-entity row. AlternateURIs doesn't include @V variants for
// unversioned input (it doesn't know what version), so the helper
// must fall through to the versioned-base scan.
func TestLookupEntity_VersionedFallback(t *testing.T) {
	t.Parallel()
	want := &profile.Entity{ID: "ent-tv", CanonicalURI: "repo:github/stretchr/testify@v1.11.1"}
	f := &fakeLookuper{
		byURI: map[string]*profile.Entity{},
		versioned: map[string]*profile.Entity{
			"repo:github/stretchr/testify": want,
		},
	}

	got, err := LookupEntity(context.Background(), f, "repo:github/stretchr/testify")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.True(t, f.findVersionedCalls >= 1,
		"versioned scan must run when no exact-match alternate matches")
}

func TestLookupEntity_NotFoundOnAllPaths(t *testing.T) {
	t.Parallel()
	f := &fakeLookuper{} // empty maps → every lookup returns ErrNotFound

	_, err := LookupEntity(context.Background(), f, "repo:github/never/seen")
	assert.ErrorIs(t, err, ErrNotFound,
		"true misses must surface as ErrNotFound — same sentinel as a direct FindEntityByURI miss")
}

// TestLookupEntity_RawInputThroughResolver — caller passes a non-
// canonical input (github shorthand, vanity Go path); the helper
// routes through ResolveTarget before walking alternates. Verifies
// the resolver and the alternates layer integrate correctly.
func TestLookupEntity_RawInputThroughResolver(t *testing.T) {
	t.Parallel()
	want := &profile.Entity{ID: "ent-r", CanonicalURI: "repo:github/alecthomas/kong"}
	f := &fakeLookuper{byURI: map[string]*profile.Entity{
		"repo:github/alecthomas/kong": want,
	}}

	cases := []string{
		"alecthomas/kong",
		"github.com/alecthomas/kong",
		"https://github.com/alecthomas/kong",
		"AlecThomas/Kong", // case folding via ResolveTarget
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			got, err := LookupEntity(context.Background(), f, in)
			require.NoError(t, err, "input %q should resolve and find the kong entity", in)
			assert.Equal(t, want, got)
		})
	}
}

// TestLookupEntity_PropagatesNonNotFoundError — DB-closed, FK error,
// or any other non-sentinel error must short-circuit the walk and
// surface verbatim. Without this, a transient I/O error would be
// silently treated as "entity missing" and the caller would never
// know the lookup didn't actually run.
func TestLookupEntity_PropagatesNonNotFoundError(t *testing.T) {
	t.Parallel()
	boom := errors.New("database is closed")
	f := &fakeLookuper{injectErr: boom}

	_, err := LookupEntity(context.Background(), f, "repo:github/alecthomas/kong")
	assert.ErrorIs(t, err, boom,
		"a non-ErrNotFound error must propagate verbatim, not be swallowed as 'try the next alternate'")
}

func TestLookupEntity_MalformedInputErrors(t *testing.T) {
	t.Parallel()
	f := &fakeLookuper{}

	_, err := LookupEntity(context.Background(), f, "")
	assert.Error(t, err,
		"empty target is malformed; ResolveTarget rejects it and the error must propagate")
	assert.NotErrorIs(t, err, ErrNotFound,
		"malformed input is distinct from a clean miss; do not collapse the two")
}
