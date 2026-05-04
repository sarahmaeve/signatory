package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EffectiveBurnByURI is the pre-collection gate primitive: given
// only a canonical URI, decide whether to refuse running analyze
// because a related identity is already burned.
//
// Two layers it walks:
//
//  1. URI-derived candidates — for repo:github/X/Y, derive
//     identity:github/X AND org:github/X (we don't know User vs
//     Organization without a collect; check both). Lets us catch
//     a brand-new repo by a burned operator before doing ANY work.
//
//  2. Signal-derived candidates (when an entity row exists at the
//     URI) — delegate to EffectiveBurn(entity.ID), which walks
//     the owner_profile / maintainer_count / publish_origin_
//     consistency signals as Path B already does.
//
// Companion to EffectiveBurn (entity-id keyed) for the case where
// we don't yet have an entity to query against.

// TestEffectiveBurnByURI_GitHubRepo_BurnedUserOwner_PreCollectionGate
// pins the BufferZone use case directly: analyze a brand-new repo
// whose github owner is already burned at identity:github/<login>.
// The store knows nothing about the repo (no entity row, no
// signals) — only the operator burn and the URI structure tell us
// it's unsafe.
func TestEffectiveBurnByURI_GitHubRepo_BurnedUserOwner_PreCollectionGate(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	ownerID := seedOwnerEntity(t, s, "identity:github/bufferzonecorp", "bufferzonecorp")
	seedBurn(t, s, ownerID, "campaign-shaped account, 17 throwaway repos")

	// Note: NO repo entity, NO owner_profile signal. The only
	// breadcrumb is the URI structure itself: repo:github/
	// bufferzonecorp/<anything> means the github user is
	// bufferzonecorp.
	burn, ctx, err := s.EffectiveBurnByURI(t.Context(), "repo:github/bufferzonecorp/never-seen-repo")
	require.NoError(t, err,
		"the URI structure plus the operator burn must be enough to decide — no signals required")
	require.NotNil(t, burn)
	require.NotNil(t, ctx)
	assert.False(t, ctx.Direct, "URI-derived case is always cascade, never direct")
	assert.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "identity:github/bufferzonecorp", ctx.ViaOwner.CanonicalURI)
	assert.Equal(t, "publisher", ctx.ViaRole)
	assert.Contains(t, burn.Reason, "campaign-shaped")
}

// TestEffectiveBurnByURI_GitHubRepo_BurnedOrgOwner_PreCollectionGate
// is the Organization-flavour parallel: the URI scheme doesn't
// distinguish User from Organization (both share repo:github/X/Y),
// so the resolver checks BOTH identity:github/X and org:github/X
// for burns. This test pins that org-side coverage.
func TestEffectiveBurnByURI_GitHubRepo_BurnedOrgOwner_PreCollectionGate(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	orgID := seedOwnerEntity(t, s, "org:github/some-malicious-org", "some-malicious-org")
	seedBurn(t, s, orgID, "test: org-level compromise")

	burn, ctx, err := s.EffectiveBurnByURI(t.Context(), "repo:github/some-malicious-org/some-repo")
	require.NoError(t, err)
	require.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "org:github/some-malicious-org", ctx.ViaOwner.CanonicalURI,
		"the resolver checks BOTH identity:github/X AND org:github/X candidates from the URI")
	assert.Equal(t, "test: org-level compromise", burn.Reason)
}

// TestEffectiveBurnByURI_DirectBurnOnEntity covers the case where
// the queried URI itself has an entity row with a direct burn.
// EffectiveBurnByURI delegates to EffectiveBurn for the entity-
// keyed checks, so this is a thin wrapper test confirming the
// composition holds.
func TestEffectiveBurnByURI_DirectBurnOnEntity(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/x/y", "x/y")
	seedBurn(t, s, repoID, "test: direct burn on the repo")

	burn, ctx, err := s.EffectiveBurnByURI(t.Context(), "repo:github/x/y")
	require.NoError(t, err)
	assert.True(t, ctx.Direct, "direct burn on the entity itself wins over URI-derived cascade")
	assert.Equal(t, "test: direct burn on the repo", burn.Reason)
}

// TestEffectiveBurnByURI_SignalDerivedCascade exercises the
// pre-existing-entity path: a repo entity exists with an
// owner_profile signal, the owner is burned, and the resolver
// surfaces the cascade. Same shape as EffectiveBurn's
// cascade-via-signal tests; the URI-keyed wrapper must compose
// cleanly with it.
func TestEffectiveBurnByURI_SignalDerivedCascade(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	repoID := seedRepoEntity(t, s, "repo:github/x/y", "x/y")
	ownerID := seedOwnerEntity(t, s, "identity:github/x", "x")
	seedSignal(t, s, repoID, "owner_profile", "github", map[string]any{
		"login": "x", "type": "User",
	})
	seedBurn(t, s, ownerID, "test: cascade via signal")

	burn, ctx, err := s.EffectiveBurnByURI(t.Context(), "repo:github/x/y")
	require.NoError(t, err)
	assert.False(t, ctx.Direct)
	assert.Equal(t, "identity:github/x", ctx.ViaOwner.CanonicalURI)
	assert.Equal(t, "test: cascade via signal", burn.Reason)
}

// TestEffectiveBurnByURI_NoBurnAnywhere returns ErrNotFound for
// the healthy-target case: no direct burn, no URI-derived owner
// burn, no signal-derived owner burn. Symmetric with the existing
// EffectiveBurn contract.
func TestEffectiveBurnByURI_NoBurnAnywhere(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	// Nothing seeded — fresh store.
	_, _, err := s.EffectiveBurnByURI(t.Context(), "repo:github/healthy-org/healthy-repo")
	require.ErrorIs(t, err, ErrNotFound,
		"a target with no related-identity burn must surface ErrNotFound — same contract as EffectiveBurn / GetBurn")
}

// TestEffectiveBurnByURI_NpmScopedPackage_BurnedScope: scoped npm
// package URIs (pkg:npm/@scope/name) encode the scope, which often
// maps to an npm org. Pre-collection gate works for these too.
func TestEffectiveBurnByURI_NpmScopedPackage_BurnedScope(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	scopeID := seedOwnerEntity(t, s, "org:npm/evilcorp", "evilcorp")
	seedBurn(t, s, scopeID, "test: npm scope compromised")

	burn, ctx, err := s.EffectiveBurnByURI(t.Context(), "pkg:npm/@evilcorp/some-package")
	require.NoError(t, err)
	require.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "org:npm/evilcorp", ctx.ViaOwner.CanonicalURI,
		"scoped npm packages cascade through the scope owner — `@scope/name` → `org:npm/scope`")
	assert.Equal(t, "test: npm scope compromised", burn.Reason)
}

// TestEffectiveBurnByURI_NpmUnscopedPackage_NoURIDerivedCandidates
// pins the limit of URI-derived analysis: an unscoped npm package
// (pkg:npm/foo) has no owner encoded in its URI, so the resolver
// falls through to signal-derived cascade only. Without prior
// signals AND without a direct burn, the result is ErrNotFound —
// the gate doesn't fire. Subsequent runs after first-collection
// have signal data and the standard cascade applies.
func TestEffectiveBurnByURI_NpmUnscopedPackage_NoURIDerivedCandidates(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	// Seed a burned identity with a name that LOOKS like the
	// package — the resolver must NOT speculatively check
	// identity:npm/<package-name>; that's not what the URI means.
	speculativeID := seedOwnerEntity(t, s, "identity:npm/foo", "foo")
	seedBurn(t, s, speculativeID, "test: shouldn't cascade — name collision")

	_, _, err := s.EffectiveBurnByURI(t.Context(), "pkg:npm/foo")
	require.ErrorIs(t, err, ErrNotFound,
		"unscoped npm packages must NOT speculatively cascade through identity:npm/<package-name> — that mistakes the package name for a publisher login")
}

// TestEffectiveBurnByURI_MavenPackage_BurnedGroupNamespace pins the
// Maven cascade: pkg:maven/<groupId>/<artifactId> encodes the groupId,
// which is the verified namespace on Maven Central. A burned groupId
// (org:maven/<groupId>) cascades to every artifact under that group —
// same structural pattern as npm scoped packages, but universal for
// Maven (all Maven packages have a groupId, unlike npm where only
// scoped packages encode ownership).
func TestEffectiveBurnByURI_MavenPackage_BurnedGroupNamespace(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	groupID := seedOwnerEntity(t, s, "org:maven/com.zerobuffercorp", "com.zerobuffercorp")
	seedBurn(t, s, groupID, "test: compromised Maven namespace — typosquat campaign")

	burn, ctx, err := s.EffectiveBurnByURI(t.Context(), "pkg:maven/com.zerobuffercorp/grpc-client")
	require.NoError(t, err,
		"Maven groupId cascade: pkg:maven/<group>/<artifact> → org:maven/<group>")
	require.NotNil(t, ctx.ViaOwner)
	assert.Equal(t, "org:maven/com.zerobuffercorp", ctx.ViaOwner.CanonicalURI,
		"the cascade must resolve through the groupId namespace entity")
	assert.Equal(t, "publisher", ctx.ViaRole)
	assert.Contains(t, burn.Reason, "typosquat campaign")
}

// TestEffectiveBurnByURI_MavenPackage_NoBurn confirms that an
// unburned Maven groupId does not trigger a false-positive cascade.
func TestEffectiveBurnByURI_MavenPackage_NoBurn(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)

	// Seed the group entity but don't burn it.
	_ = seedOwnerEntity(t, s, "org:maven/org.apache.commons", "org.apache.commons")

	_, _, err := s.EffectiveBurnByURI(t.Context(), "pkg:maven/org.apache.commons/commons-lang3")
	require.ErrorIs(t, err, ErrNotFound,
		"unburned Maven namespace must not trigger cascade")
}
