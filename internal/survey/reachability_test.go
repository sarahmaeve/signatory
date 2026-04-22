package survey

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// --- computeReachability ---------------------------------------------------

// TestComputeReachability_SingleChainAttributesToDirect builds a
// trivial graph (root → D → I) and confirms I gets exactly one
// FromDirects entry pointing at D. Baseline test: if this fails,
// no other reachability test will work.
func TestComputeReachability_SingleChainAttributesToDirect(t *testing.T) {
	t.Parallel()
	g := manifest.Graph{
		RootURI: "repo:github/me/proj",
		Edges: []manifest.Edge{
			{Parent: "repo:github/me/proj", Child: "repo:github/dep/D"},
			{Parent: "repo:github/dep/D", Child: "repo:github/dep/I"},
		},
	}
	directs := []manifest.Dep{
		{CanonicalURI: "repo:github/dep/D", Direct: true},
	}
	r := computeReachability(g, directs)
	require.Contains(t, r, "repo:github/dep/I")
	assert.Equal(t, []string{"repo:github/dep/D"}, r["repo:github/dep/I"].FromDirects,
		"I is reached via D and only D")

	// D itself must NOT appear in the result — directs are not
	// indirects and the function deliberately excludes them.
	assert.NotContains(t, r, "repo:github/dep/D",
		"directs must not appear in the reachability map")
}

// TestComputeReachability_DiamondListsBothParents covers the
// case the bucketing's max-pessimism rule depends on: an
// indirect reached via two different directs gets BOTH listed
// in FromDirects. Without this, the diamond rule would have no
// data to operate on.
func TestComputeReachability_DiamondListsBothParents(t *testing.T) {
	t.Parallel()
	g := manifest.Graph{
		RootURI: "repo:github/me/proj",
		Edges: []manifest.Edge{
			{Parent: "repo:github/me/proj", Child: "repo:github/dep/A"},
			{Parent: "repo:github/me/proj", Child: "repo:github/dep/B"},
			{Parent: "repo:github/dep/A", Child: "repo:github/dep/I"},
			{Parent: "repo:github/dep/B", Child: "repo:github/dep/I"},
		},
	}
	directs := []manifest.Dep{
		{CanonicalURI: "repo:github/dep/A", Direct: true},
		{CanonicalURI: "repo:github/dep/B", Direct: true},
	}
	r := computeReachability(g, directs)
	require.Contains(t, r, "repo:github/dep/I")
	got := append([]string(nil), r["repo:github/dep/I"].FromDirects...)
	sort.Strings(got)
	assert.Equal(t, []string{"repo:github/dep/A", "repo:github/dep/B"}, got,
		"both reaching directs must appear in FromDirects")
}

// TestComputeReachability_TransitiveAttribution confirms that an
// indirect two hops down still attributes to the right direct.
// The BFS must not stop at the first hop.
func TestComputeReachability_TransitiveAttribution(t *testing.T) {
	t.Parallel()
	g := manifest.Graph{
		Edges: []manifest.Edge{
			{Parent: "repo:github/me/proj", Child: "repo:github/dep/A"},
			{Parent: "repo:github/dep/A", Child: "repo:github/dep/B"},
			{Parent: "repo:github/dep/B", Child: "repo:github/dep/C"},
		},
	}
	directs := []manifest.Dep{
		{CanonicalURI: "repo:github/dep/A", Direct: true},
	}
	r := computeReachability(g, directs)
	for _, hop := range []string{"repo:github/dep/B", "repo:github/dep/C"} {
		require.Contains(t, r, hop, "BFS must reach %s transitively", hop)
		assert.Equal(t, []string{"repo:github/dep/A"}, r[hop].FromDirects)
	}
}

// TestComputeReachability_EmptyInputsReturnNil documents the
// degenerate cases. Renderers and bucketing rely on nil-checking
// the result; if this returned an empty non-nil map, the
// fallback rendering wouldn't fire.
func TestComputeReachability_EmptyInputsReturnNil(t *testing.T) {
	t.Parallel()
	assert.Nil(t, computeReachability(manifest.Graph{}, nil))
	assert.Nil(t, computeReachability(manifest.Graph{}, []manifest.Dep{
		{CanonicalURI: "repo:github/dep/A", Direct: true},
	}), "empty graph yields nil even with directs present")
	assert.Nil(t, computeReachability(manifest.Graph{
		Edges: []manifest.Edge{{Parent: "x", Child: "y"}},
	}, nil), "empty directs yields nil even with edges present")
}

// --- bucketIndirects ------------------------------------------------------

// makeIndirect builds a DepResult for an indirect dep with the
// given URI, tier, and reachability-from-directs list. Helper
// since bucketIndirects tests construct lots of these.
func makeIndirect(uri string, tier Tier, fromDirects ...string) DepResult {
	d := DepResult{
		Dep:  manifest.Dep{CanonicalURI: uri, Direct: false},
		Tier: tier,
	}
	if len(fromDirects) > 0 {
		d.Reachability = &Reachability{FromDirects: append([]string(nil), fromDirects...)}
	}
	return d
}

// TestBucketIndirects_OwnResolvedWinsOverReachability confirms
// the precedence rule: an indirect's own resolved tier overrides
// reachability. Even if the indirect would be ViaUnresolved by
// graph position, its own verdict puts it in OwnResolved.
//
// Revert proof: move the IsResolvedTier(d.Tier) check below the
// reachability check; this test fails because the indirect
// would land in ViaUnresolved instead.
func TestBucketIndirects_OwnResolvedWinsOverReachability(t *testing.T) {
	t.Parallel()
	deps := []DepResult{
		// Indirect with its own vetted-frozen tier, reached
		// only via an unresolved direct — the own-tier rule
		// must put it in OwnResolved despite the graph.
		makeIndirect("repo:github/dep/I", TierVettedFrozen, "repo:github/dep/D"),
	}
	directTier := map[string]Tier{
		"repo:github/dep/D": TierUnexamined, // unresolved
	}
	got := bucketIndirects(deps, directTier)
	assert.Equal(t, IndirectReachabilityBreakdown{
		OwnResolved: 1,
	}, got)
}

// TestBucketIndirects_AllReachingDirectsResolvedYieldsViaResolved
// is the defer-safe path: an indirect with no own tier, reached
// via directs that are all resolved, lands in ViaResolved.
func TestBucketIndirects_AllReachingDirectsResolvedYieldsViaResolved(t *testing.T) {
	t.Parallel()
	deps := []DepResult{
		makeIndirect("repo:github/dep/I", TierNotInStore,
			"repo:github/dep/A", "repo:github/dep/B"),
	}
	directTier := map[string]Tier{
		"repo:github/dep/A": TierVettedFrozen,
		"repo:github/dep/B": TierTrustedForNow,
	}
	got := bucketIndirects(deps, directTier)
	assert.Equal(t, IndirectReachabilityBreakdown{
		ViaResolved: 1,
	}, got)
}

// TestBucketIndirects_AnyUnresolvedReachingDirectYieldsViaUnresolved
// is the max-pessimism diamond rule: an indirect reachable via
// BOTH a resolved AND an unresolved direct lands in ViaUnresolved.
// The conservative bucketing means defer-safety requires EVERY
// path to be safe.
//
// Revert proof: change the inner break to a continue; this test
// fails because a single unresolved parent would no longer flip
// the allResolved flag and the indirect would mis-bucket as
// ViaResolved.
func TestBucketIndirects_AnyUnresolvedReachingDirectYieldsViaUnresolved(t *testing.T) {
	t.Parallel()
	deps := []DepResult{
		makeIndirect("repo:github/dep/I", TierNotInStore,
			"repo:github/dep/RESOLVED", "repo:github/dep/UNRESOLVED"),
	}
	directTier := map[string]Tier{
		"repo:github/dep/RESOLVED":   TierVettedFrozen,
		"repo:github/dep/UNRESOLVED": TierUnexamined,
	}
	got := bucketIndirects(deps, directTier)
	assert.Equal(t, IndirectReachabilityBreakdown{
		ViaUnresolved: 1,
	}, got, "max-pessimism: any unresolved path → ViaUnresolved")
}

// TestBucketIndirects_UnknownProvenanceCountsAsUnresolved pins
// the decision from the planning chat: unknown-provenance is a
// tier-with-a-name but the verdict it carries ("identity not
// confidently confirmed") is itself pending. Treat as unresolved
// for both own-tier classification (doesn't get OwnResolved) and
// reaching-direct classification (an unknown-provenance direct
// flips ViaResolved → ViaUnresolved).
func TestBucketIndirects_UnknownProvenanceCountsAsUnresolved(t *testing.T) {
	t.Parallel()
	// Half 1: indirect with own tier = unknown-provenance must
	// NOT bucket as OwnResolved.
	deps := []DepResult{
		makeIndirect("repo:github/dep/I1", TierUnknownProvenance,
			"repo:github/dep/D"),
	}
	directTier := map[string]Tier{
		"repo:github/dep/D": TierVettedFrozen,
	}
	got := bucketIndirects(deps, directTier)
	assert.Zero(t, got.OwnResolved,
		"unknown-provenance is not a resolved verdict — must not land in OwnResolved")
	assert.Equal(t, 1, got.ViaResolved,
		"with no own resolved tier and a vetted reaching direct, indirect is via-resolved")

	// Half 2: indirect reached via an unknown-provenance direct
	// must bucket as ViaUnresolved (not ViaResolved).
	deps = []DepResult{
		makeIndirect("repo:github/dep/I2", TierNotInStore,
			"repo:github/dep/UP"),
	}
	directTier = map[string]Tier{
		"repo:github/dep/UP": TierUnknownProvenance,
	}
	got = bucketIndirects(deps, directTier)
	assert.Equal(t, IndirectReachabilityBreakdown{
		ViaUnresolved: 1,
	}, got, "unknown-provenance direct must NOT count as resolved for the indirect")
}

// TestBucketIndirects_DirectsAreNotCounted confirms direct deps
// pass through bucketIndirects without contributing — only
// indirect rows are bucketed. Otherwise the breakdown counts
// would over-count.
func TestBucketIndirects_DirectsAreNotCounted(t *testing.T) {
	t.Parallel()
	deps := []DepResult{
		{Dep: manifest.Dep{CanonicalURI: "repo:github/dep/A", Direct: true}, Tier: TierVettedFrozen},
		{Dep: manifest.Dep{CanonicalURI: "repo:github/dep/B", Direct: true}, Tier: TierUnexamined},
	}
	got := bucketIndirects(deps, nil)
	assert.Zero(t, got.OwnResolved+got.ViaResolved+got.ViaUnresolved,
		"direct deps must not contribute to any indirect bucket")
}

// TestBucketIndirects_IndirectWithoutReachabilityIsSilent
// covers the graceful-degradation path: when graph data wasn't
// available (Reachability is nil on indirects) AND the indirect
// has no own resolved tier, the indirect simply isn't bucketed.
// The breakdown's HasData() returns false; renderers fall back
// to the no-graph rendering.
func TestBucketIndirects_IndirectWithoutReachabilityIsSilent(t *testing.T) {
	t.Parallel()
	deps := []DepResult{
		// No Reachability set — simulates the "graph parser
		// returned ErrGraphUnavailable" case.
		{Dep: manifest.Dep{CanonicalURI: "repo:github/dep/I", Direct: false},
			Tier: TierNotInStore},
	}
	got := bucketIndirects(deps, nil)
	assert.Equal(t, IndirectReachabilityBreakdown{}, got)
	assert.False(t, got.HasData(),
		"no reachability + no own tier → zero-valued breakdown → fallback rendering")
}

// --- populateReachability (integration of the two helpers) ----------------

// TestPopulateReachability_EndToEnd assembles a Result that
// looks like one resolveDep would have produced (directs and
// indirects each with tiers), runs populateReachability against
// a manufactured graph, and asserts both halves of the output:
// per-indirect Reachability fields are populated AND the
// Summary.IndirectByReachability buckets are correct.
//
// This is the wiring test — if the two helpers work in isolation
// (covered above) but the wiring forgets to set Reachability on
// the DepResult, this test catches that gap.
func TestPopulateReachability_EndToEnd(t *testing.T) {
	t.Parallel()

	// Project with two directs (one resolved, one unresolved)
	// and three indirects:
	//   I1: reached only via the resolved direct → ViaResolved
	//   I2: reached only via the unresolved direct → ViaUnresolved
	//   I3: own posture (vetted-frozen) → OwnResolved
	g := manifest.Graph{
		RootURI: "repo:github/me/proj",
		Edges: []manifest.Edge{
			{Parent: "repo:github/me/proj", Child: "repo:github/dep/RESOLVED"},
			{Parent: "repo:github/me/proj", Child: "repo:github/dep/UNRESOLVED"},
			{Parent: "repo:github/dep/RESOLVED", Child: "repo:github/dep/I1"},
			{Parent: "repo:github/dep/UNRESOLVED", Child: "repo:github/dep/I2"},
			{Parent: "repo:github/dep/RESOLVED", Child: "repo:github/dep/I3"},
		},
	}
	out := &Result{
		Summary: Summary{Direct: 2, Indirect: 3, ByTier: map[Tier]int{}},
		Deps: []DepResult{
			{Dep: manifest.Dep{CanonicalURI: "repo:github/dep/RESOLVED", Direct: true},
				Tier: TierVettedFrozen},
			{Dep: manifest.Dep{CanonicalURI: "repo:github/dep/UNRESOLVED", Direct: true},
				Tier: TierUnexamined},
			{Dep: manifest.Dep{CanonicalURI: "repo:github/dep/I1", Direct: false},
				Tier: TierNotInStore},
			{Dep: manifest.Dep{CanonicalURI: "repo:github/dep/I2", Direct: false},
				Tier: TierNotInStore},
			{Dep: manifest.Dep{CanonicalURI: "repo:github/dep/I3", Direct: false},
				Tier: TierVettedFrozen},
		},
	}

	populateReachability(g, out)

	// Per-indirect Reachability annotations.
	for _, d := range out.Deps {
		if d.Dep.Direct {
			assert.Nil(t, d.Reachability,
				"directs must not get Reachability set")
			continue
		}
		require.NotNilf(t, d.Reachability,
			"indirect %s must have Reachability populated", d.Dep.CanonicalURI)
	}

	// Per-indirect FromDirects content.
	depByURI := map[string]DepResult{}
	for _, d := range out.Deps {
		depByURI[d.Dep.CanonicalURI] = d
	}
	assert.Equal(t, []string{"repo:github/dep/RESOLVED"},
		depByURI["repo:github/dep/I1"].Reachability.FromDirects)
	assert.Equal(t, []string{"repo:github/dep/UNRESOLVED"},
		depByURI["repo:github/dep/I2"].Reachability.FromDirects)
	assert.Equal(t, []string{"repo:github/dep/RESOLVED"},
		depByURI["repo:github/dep/I3"].Reachability.FromDirects)

	// Bucket counts.
	assert.Equal(t, IndirectReachabilityBreakdown{
		OwnResolved:   1, // I3
		ViaResolved:   1, // I1
		ViaUnresolved: 1, // I2
	}, out.Summary.IndirectByReachability)
}
