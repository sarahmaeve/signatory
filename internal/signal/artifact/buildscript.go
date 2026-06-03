package artifact

import (
	"strings"

	"github.com/sarahmaeve/signatory/internal/signal/buildscript"
)

// ArtifactBuildScriptFinding is one build-script content finding in the
// published artifact, in the divergence frame: path stripped of the
// manifest top-dir and tagged by repo presence. A strong finding whose
// path is absent from the git tree (PathInRepo=false) is the
// CVE-2024-3094 / xz shape — a weaponized build input present only in
// what was published.
type ArtifactBuildScriptFinding struct {
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Kind       string `json:"kind"`
	Severity   string `json:"severity"`
	PathInRepo bool   `json:"path_in_repo"`
	Snippet    string `json:"snippet"`
}

// BuildScriptConcern is the build_script_concern signal payload:
// content findings from scanning the published artifact's build /
// install scripts. The collector emits it only when at least one
// finding is present; the synthesist weights by per-finding severity.
type BuildScriptConcern struct {
	ArtifactURL string                       `json:"artifact_url"`
	Findings    []ArtifactBuildScriptFinding `json:"findings"`
}

// classifyBuildScriptFindings converts raw matcher findings (verbatim
// archive paths) into divergence-frame findings: top-dir stripped, repo
// presence tagged. Always returns a non-nil slice so the emitted signal
// carries "findings": [] rather than null on the clean case.
func classifyBuildScriptFindings(findings []buildscript.Finding, stripPrefix string, gitPaths []string) []ArtifactBuildScriptFinding {
	out := make([]ArtifactBuildScriptFinding, 0, len(findings))
	gitSet := gitPathSet(gitPaths)
	for _, f := range findings {
		path := strings.TrimPrefix(f.File, stripPrefix)
		_, inRepo := gitSet[path]
		out = append(out, ArtifactBuildScriptFinding{
			Path:       path,
			Line:       f.Line,
			Kind:       string(f.Kind),
			Severity:   string(f.Severity),
			PathInRepo: inRepo,
			Snippet:    f.Snippet,
		})
	}
	return out
}
