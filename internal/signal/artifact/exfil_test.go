package artifact

import (
	"testing"

	"github.com/sarahmaeve/signatory/internal/signal/exfilwatch"
	"github.com/stretchr/testify/require"
)

func TestClassifyExfilHits_StripsTopDirAndTagsPathInRepo(t *testing.T) {
	// Two exfil literals in the published sdist: one in a file that also
	// exists in the git tree, one in a file that does NOT (the
	// CVE-2024-3094 / xz shape — a sink present only in what was
	// published). Paths are reported post-strip, matching the divergence
	// diff's frame, and tagged by repo presence.
	hits := []exfilwatch.Hit{
		{File: "pkg-1.0/pkg/__init__.py", Line: 2, Host: "discord.com/api/webhooks"},
		{File: "pkg-1.0/pkg/injected.py", Line: 5, Host: "webhook.site"},
	}
	gitPaths := []string{"pkg/__init__.py"} // injected.py absent from repo

	got := classifyExfilHits(hits, "pkg-1.0/", gitPaths)

	require.Equal(t, []ArtifactExfilHit{
		{Path: "pkg/__init__.py", Line: 2, Host: "discord.com/api/webhooks", PathInRepo: true},
		{Path: "pkg/injected.py", Line: 5, Host: "webhook.site", PathInRepo: false},
	}, got)
}

func TestClassifyExfilHits_EmptyReturnsNil(t *testing.T) {
	require.Nil(t, classifyExfilHits(nil, "pkg-1.0/", []string{"a"}))
}
