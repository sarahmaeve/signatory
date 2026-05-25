package repofiles

import "github.com/sarahmaeve/signatory/internal/agentconfig"

// AgentConfigFamilies returns the AI-agent configuration file
// family list in deterministic order, derived from the canonical
// agentconfig.Loci() declaration.
//
// The mapping is one Locus -> one Family per call: the file-
// detector fields (Dirs, Detector, Preferred) carry over verbatim;
// the Locus's runtime-path prefixes are consumed by astfeature on
// the source-AST side, not here.
//
// Parallel to Families() (hygiene files like README, SECURITY,
// CODEOWNERS) but targets a distinct detection surface: AI-agent
// instruction surfaces with their own threat model
// (zero-width-Unicode prompt-injection carriers per the Trapdoor
// 2026-05 campaign).
//
// Returns a fresh slice on each call so callers cannot mutate the
// package-level declaration.
func AgentConfigFamilies() []Family {
	loci := agentconfig.Loci()
	out := make([]Family, len(loci))
	for i, l := range loci {
		out[i] = Family{
			Name:      l.Name,
			Dirs:      l.Dirs,
			Detector:  l.Detector,
			Preferred: l.Preferred,
		}
	}
	return out
}
