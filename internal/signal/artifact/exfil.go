package artifact

import (
	"strings"

	"github.com/sarahmaeve/signatory/internal/signal/exfilwatch"
)

// ArtifactExfilHit is one exfil-host literal found in the published
// artifact's source, reported in the divergence signal alongside the
// file-set diff.
//
// PathInRepo is the load-bearing field for the trust read: a hit whose
// path is NOT in the git tree (PathInRepo=false) is the CVE-2024-3094 /
// xz shape — an exfiltration sink present in what was published but
// absent from source. A hit whose path IS in the repo is weaker on its
// own (the file exists in source too; we only know the published copy
// contains the literal, not whether the source copy does), but a known
// no-legitimate-purpose host like a Discord webhook is notable wherever
// it appears.
type ArtifactExfilHit struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Host       string `json:"host"`
	PathInRepo bool   `json:"path_in_repo"`
}

// classifyExfilHits converts raw walker hits (verbatim archive paths)
// into divergence-frame hits: the StrippedTopDir is trimmed so paths
// match the diff's git-relative view, and each is tagged by whether its
// path is present in the git tree. Input order (archive order, which is
// deterministic) is preserved for stable JSON. Returns nil for no hits
// so the omitempty field stays absent on the common clean case.
func classifyExfilHits(hits []exfilwatch.Hit, stripPrefix string, gitPaths []string) []ArtifactExfilHit {
	if len(hits) == 0 {
		return nil
	}
	gitSet := make(map[string]struct{}, len(gitPaths))
	for _, p := range gitPaths {
		gitSet[p] = struct{}{}
	}
	out := make([]ArtifactExfilHit, 0, len(hits))
	for _, h := range hits {
		path := strings.TrimPrefix(h.File, stripPrefix)
		_, inRepo := gitSet[path]
		out = append(out, ArtifactExfilHit{
			Path:       path,
			Line:       h.Line,
			Host:       h.Host,
			PathInRepo: inRepo,
		})
	}
	return out
}
