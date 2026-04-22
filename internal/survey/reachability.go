package survey

import (
	"github.com/sarahmaeve/signatory/internal/manifest"
)

// computeReachability walks the dep graph and records, for each
// node, the set of direct deps that can reach it via some path.
// Returned map keys are canonical URIs; values carry the
// FromDirects set. Direct deps themselves are NOT keyed in the
// result — reachability is meaningful only for indirects, and
// directs are handled separately by tier rather than by graph
// position.
//
// Algorithm: BFS from each direct's subtree. For each newly-
// reached node, union the direct's URI into that node's
// reachability set. O(D × (N+E)) worst case where D is the
// number of directs; for typical projects (D < 30, N < 500)
// this is well under a millisecond and runs once per survey.
//
// The "max-pessimism" diamond rule discussed in planning is
// implemented at the bucketing layer (categorizeIndirect), not
// here — this function reports the FULL set of reaching directs
// faithfully so a future "show me which directs reach this
// indirect" verb has the same data without re-walking.
func computeReachability(g manifest.Graph, directs []manifest.Dep) map[string]*Reachability {
	if len(g.Edges) == 0 || len(directs) == 0 {
		return nil
	}

	// Adjacency list: parent URI → list of child URIs. Built
	// once and reused across the per-direct BFS.
	adj := make(map[string][]string, len(g.Edges))
	for _, e := range g.Edges {
		adj[e.Parent] = append(adj[e.Parent], e.Child)
	}

	result := make(map[string]*Reachability)
	for _, d := range directs {
		// BFS from this direct. The starting node itself
		// (the direct) is marked visited but not added to the
		// reachability map — directs aren't indirects.
		visited := map[string]bool{d.CanonicalURI: true}
		queue := []string{d.CanonicalURI}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			for _, next := range adj[cur] {
				if visited[next] {
					continue
				}
				visited[next] = true
				queue = append(queue, next)
				r, ok := result[next]
				if !ok {
					r = &Reachability{}
					result[next] = r
				}
				r.FromDirects = append(r.FromDirects, d.CanonicalURI)
			}
		}
	}
	return result
}

// bucketIndirects partitions resolved indirect deps into the
// three reachability buckets (OwnResolved, ViaResolved,
// ViaUnresolved) per the rule documented on
// IndirectReachabilityBreakdown.
//
// The "max-pessimism" diamond rule lives here: if an indirect
// is reachable via multiple directs and ANY of them is
// unresolved, the indirect is bucketed as ViaUnresolved.
// Defer-safety requires every reaching path to be safe.
//
// Inputs:
//   - deps: the full DepResult slice from Run, after resolveDep
//     has populated each Tier
//   - directTier: lookup map from direct's canonical URI to its
//     resolved survey.Tier
//
// Returns a zero-valued breakdown when there are no indirects
// to bucket — renderers detect this via HasData() and degrade.
func bucketIndirects(deps []DepResult, directTier map[string]Tier) IndirectReachabilityBreakdown {
	var b IndirectReachabilityBreakdown
	for _, d := range deps {
		if d.Dep.Direct {
			continue
		}
		// Bucket 1: indirect has its own resolved tier — the
		// indirect's verdict overrides any reachability story.
		if IsResolvedTier(d.Tier) {
			b.OwnResolved++
			continue
		}
		// Bucket 2 vs 3 depends on reachability. If we don't
		// have reachability data for this indirect (graph was
		// missing or the indirect wasn't reached by any direct
		// — the latter is unusual but tolerated), don't double-
		// count it. The breakdown's HasData() will be false in
		// the "no graph" case so renderers fall back to the
		// generic count.
		if d.Reachability == nil || len(d.Reachability.FromDirects) == 0 {
			continue
		}
		// Max-pessimism: any unresolved reaching direct flips
		// the bucket to ViaUnresolved. Only when EVERY reaching
		// direct is resolved does the indirect count as
		// ViaResolved.
		allResolved := true
		for _, parentURI := range d.Reachability.FromDirects {
			parentTier, ok := directTier[parentURI]
			if !ok || !IsResolvedTier(parentTier) {
				allResolved = false
				break
			}
		}
		if allResolved {
			b.ViaResolved++
		} else {
			b.ViaUnresolved++
		}
	}
	return b
}
