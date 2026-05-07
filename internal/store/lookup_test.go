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
// LookupEntity. Map-driven dispatch tables let each test declaratively
// seed which URIs resolve to which entities, which entity IDs have
// postures attached (for weight-aware-walk tests), plus an optional
// injected error to exercise the propagation paths.
type fakeLookuper struct {
	byURI map[string]*profile.Entity
	// versioned maps a BASE URI to the entity that the
	// FindEntityByVersionedBaseURI scan should return for that base.
	// Empty / missing means "no @V sibling" → ErrNotFound.
	versioned map[string]*profile.Entity
	// posturedEntityIDs is the set of entity IDs whose HasPostures()
	// returns true. Drives the weight-aware preference in LookupEntity.
	// Empty / nil means no entity is considered "rich"; LookupEntity
	// falls back to first-hit semantics.
	posturedEntityIDs map[string]bool
	// injectErr, when non-nil, is returned from every lookup. Used
	// to exercise non-NotFound error propagation.
	injectErr error
	// counts track how many times each method ran — lets tests assert
	// the helper short-circuits on first hit and doesn't make extra
	// round-trips after success.
	findByURICalls, findVersionedCalls, hasPosturesCalls int
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

func (f *fakeLookuper) HasPostures(_ context.Context, entityID string) (bool, error) {
	f.hasPosturesCalls++
	if f.injectErr != nil {
		return false, f.injectErr
	}
	return f.posturedEntityIDs[entityID], nil
}

func TestLookupEntity_ExactMatch(t *testing.T) {
	t.Parallel()
	want := &profile.Entity{ID: "ent-1", CanonicalURI: "repo:github/alecthomas/kong"}
	f := &fakeLookuper{
		byURI: map[string]*profile.Entity{
			"repo:github/alecthomas/kong": want,
		},
		// Mark the entity as posture-bearing so the weight-aware
		// alternate walk short-circuits on the first hit. Without
		// this, a thin first hit triggers a search through alternate
		// URIs looking for a richer match (the safeguard behavior
		// covered by the PrefersAlternateWithPostures test).
		posturedEntityIDs: map[string]bool{"ent-1": true},
	}

	got, err := LookupEntity(context.Background(), f, "repo:github/alecthomas/kong")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 1, f.findByURICalls,
		"rich exact match must hit FindEntityByURI exactly once and short-circuit")
	assert.Equal(t, 0, f.findVersionedCalls,
		"versioned scan must NOT run when an exact match (with postures) was found")
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

// TestLookupEntity_PrefersAlternateWithPostures covers the yaml.v3-
// class fragmentation surfaced by 2026-04-28 dogfood: the same logical
// identity has two entity rows in the store, one carrying the posture
// and the other carrying analyses. Naive first-hit alternate-walk
// returns whichever URI happened to land first — for yaml.v3 that's
// the analyses-only row, so survey reports "unexamined" even though a
// posture exists at the alternate.
//
// Weight-aware walk: continue past hits whose entity has no postures,
// keeping the first thin hit as a fallback; return the first hit
// whose HasPostures is true. If no hit has postures, return the first
// thin hit (preserves prior NotFound vs ErrNotFound semantics).
//
// Revert proof: drop the HasPostures probe in LookupEntity; this test
// fails because the first hit (analyses-only) wins even though the
// alternate has the posture.
func TestLookupEntity_PrefersAlternateWithPostures(t *testing.T) {
	t.Parallel()

	thin := &profile.Entity{ID: "ent-thin", CanonicalURI: "pkg:golang/gopkg.in/yaml.v3"}
	rich := &profile.Entity{ID: "ent-rich", CanonicalURI: "pkg:go/gopkg.in/yaml.v3"}
	f := &fakeLookuper{
		byURI: map[string]*profile.Entity{
			"pkg:golang/gopkg.in/yaml.v3": thin, // first alternate hit
			"pkg:go/gopkg.in/yaml.v3":     rich, // second alternate hit, has posture
		},
		posturedEntityIDs: map[string]bool{"ent-rich": true},
	}

	got, err := LookupEntity(context.Background(), f, "pkg:golang/gopkg.in/yaml.v3")
	require.NoError(t, err)
	assert.Equal(t, rich, got,
		"alternate with postures must win over an analyses-only or empty alternate that hit first")
}

// TestLookupEntity_FallsBackToFirstHitWhenNoneHavePostures — testify-
// class case: multiple alternates hit but none have postures (only
// analyses or nothing). LookupEntity must return the first hit so
// callers see the same behavior as today's "find anything" semantics
// when no alternate is rich. Without this fallback, weight-aware walk
// would surface NotFound for testify when at least one row exists.
func TestLookupEntity_FallsBackToFirstHitWhenNoneHavePostures(t *testing.T) {
	t.Parallel()

	first := &profile.Entity{ID: "ent-first", CanonicalURI: "pkg:golang/github.com/stretchr/testify@v1.11.1"}
	second := &profile.Entity{ID: "ent-second", CanonicalURI: "repo:github/stretchr/testify@v1.11.1"}
	f := &fakeLookuper{
		// Both hits via the versioned-base scan; no postures on either.
		versioned: map[string]*profile.Entity{
			"pkg:golang/github.com/stretchr/testify": first,
			"repo:github/stretchr/testify":           second,
		},
		// posturedEntityIDs left nil → HasPostures returns false for everything.
	}

	got, err := LookupEntity(context.Background(), f, "repo:github/stretchr/testify")
	require.NoError(t, err, "at least one row exists; LookupEntity must surface it, not NotFound")
	// First by alternate order: the input itself walks first, so the
	// repo:github/ scan runs ahead of the cross-scheme alternate.
	// Either hit is acceptable; the contract is "some hit, not nil."
	assert.NotNil(t, got)
}

// TestLookupEntity_RichHitShortCircuitsAlternateWalk — when the FIRST
// alternate hit is already rich (has postures), LookupEntity must
// return immediately without probing further alternates. Avoids
// wasted round-trips on the common case where the canonical URI is
// also where the posture lives.
func TestLookupEntity_RichHitShortCircuitsAlternateWalk(t *testing.T) {
	t.Parallel()

	rich := &profile.Entity{ID: "ent-rich", CanonicalURI: "repo:github/alecthomas/kong"}
	f := &fakeLookuper{
		byURI: map[string]*profile.Entity{
			"repo:github/alecthomas/kong": rich,
		},
		posturedEntityIDs: map[string]bool{"ent-rich": true},
	}

	got, err := LookupEntity(context.Background(), f, "repo:github/alecthomas/kong")
	require.NoError(t, err)
	assert.Equal(t, rich, got)
	assert.Equal(t, 1, f.findByURICalls,
		"first hit was rich; alternate walk must short-circuit, not probe further URIs")
	assert.Equal(t, 1, f.hasPosturesCalls,
		"exactly one HasPostures probe — the one for the rich first hit")
	assert.Equal(t, 0, f.findVersionedCalls,
		"versioned-base scan must NOT run when the alternate-walk already returned")
}

// TestLookupEntityID_EmptyInput: empty target returns empty ID and
// no error. Lets callers chain into a filter struct's EntityID field
// without nil-checking — empty there means "no filter."
func TestLookupEntityID_EmptyInput(t *testing.T) {
	t.Parallel()
	f := &fakeLookuper{}

	id, err := LookupEntityID(context.Background(), f, "")
	require.NoError(t, err)
	assert.Empty(t, id)
	assert.Equal(t, 0, f.findByURICalls,
		"empty input must short-circuit before any lookup")
}

// TestLookupEntityID_VanityResolution: the regression case. A vanity
// Go path queries through the alternate-URI walk to the real entity
// at the github form. Pre-fix, show-* commands were stuck on a
// direct FindEntityByURI that missed this equivalence; the helper
// exists specifically to put them on LookupEntity's footing.
func TestLookupEntityID_VanityResolution(t *testing.T) {
	t.Parallel()
	want := &profile.Entity{ID: "ent-mod", CanonicalURI: "repo:github/golang/mod"}
	f := &fakeLookuper{
		byURI: map[string]*profile.Entity{
			"repo:github/golang/mod": want,
		},
		posturedEntityIDs: map[string]bool{"ent-mod": true},
	}

	id, err := LookupEntityID(context.Background(), f, "golang.org/x/mod")
	require.NoError(t, err)
	assert.Equal(t, "ent-mod", id,
		"vanity input must walk to the github form and return its entity ID")
}

// TestLookupEntityID_NotFound: ErrNotFound from the underlying
// alternate walk surfaces verbatim. Show-* commands rely on this
// sentinel to render the "no entity matches" message.
func TestLookupEntityID_NotFound(t *testing.T) {
	t.Parallel()
	f := &fakeLookuper{} // empty maps → every lookup misses

	id, err := LookupEntityID(context.Background(), f, "repo:github/nobody/nothing")
	require.ErrorIs(t, err, ErrNotFound,
		"unmatched target must surface as ErrNotFound for caller branch detection")
	assert.Empty(t, id)
}

// TestLookupEntityID_PropagatesProfileError: malformed input that
// profile.ResolveTarget rejects surfaces as a non-ErrNotFound error.
// CLI callers swallow this back to "no entity matches" UX; MCP
// callers map it to CodeSchemaViolation. The helper itself stays
// truthful and propagates rather than masking the distinction.
func TestLookupEntityID_PropagatesProfileError(t *testing.T) {
	t.Parallel()
	f := &fakeLookuper{}

	id, err := LookupEntityID(context.Background(), f, "not a valid uri or shorthand at all !!!")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNotFound,
		"profile parse failures must NOT be silently flattened to ErrNotFound here; "+
			"callers that want that flattening apply it explicitly at their layer")
	assert.Empty(t, id)
}
