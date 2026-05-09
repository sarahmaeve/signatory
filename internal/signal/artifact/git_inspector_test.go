package artifact

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/gitenv"
)

// TestGitInspector_AgainstFixtureClone exercises Tags, PathsAtRef,
// and CommitForRef against a real local git repository created in
// a tempdir. No httptest, no network — just a few `git init`-flavored
// commands to set up a deterministic fixture.
//
// Three assertions, one fixture: the inspector's three methods
// share a clone path and a subprocess discipline (gitenv.NewCmd),
// so testing them together against one fixture is cheaper than
// three separate fixtures and exercises the same wiring once.
func TestGitInspector_AgainstFixtureClone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	gitInit(t, dir)

	// Two files at the tag we'll resolve, plus a third file added
	// AFTER the tag so we can verify PathsAtRef is ref-scoped (not
	// "current working tree") and would have caught a bug like
	// "we accidentally read from HEAD instead of the requested ref."
	writeFixtureFile(t, dir, "src/main.go", "package main\n")
	writeFixtureFile(t, dir, "README.md", "# fixture\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "initial")
	gitRun(t, dir, "tag", "v1.0.0")

	// Post-tag file: must NOT show up in PathsAtRef("v1.0.0").
	writeFixtureFile(t, dir, "src/post-tag.go", "// added later\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", "post tag")
	gitRun(t, dir, "tag", "v1.0.1")

	insp := NewGitInspector(dir)

	t.Run("Tags returns both tags in repo order", func(t *testing.T) {
		tags, err := insp.Tags(context.Background())
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"v1.0.0", "v1.0.1"}, tags,
			"both tags must appear; lexical/topological order is not "+
				"contractual but presence is")
	})

	t.Run("PathsAtRef is ref-scoped", func(t *testing.T) {
		paths, err := insp.PathsAtRef(context.Background(), "v1.0.0")
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"README.md", "src/main.go"}, paths,
			"only files committed BEFORE v1.0.0 may appear; the post-tag "+
				"file was committed after and would surface a "+
				"'reading-from-HEAD' bug if it leaked through")
	})

	t.Run("CommitForRef returns a 40-char SHA", func(t *testing.T) {
		sha, err := insp.CommitForRef(context.Background(), "v1.0.0")
		require.NoError(t, err)
		assert.Len(t, sha, 40,
			"git rev-parse returns full 40-char SHAs; a shorter return "+
				"means we got a name or an abbreviated form")
	})

	t.Run("PathsAtRef on missing ref returns error", func(t *testing.T) {
		_, err := insp.PathsAtRef(context.Background(), "nonexistent-tag")
		assert.Error(t, err,
			"missing ref must surface as error so the collector records "+
				"absence with a useful reason rather than emitting an "+
				"empty divergence signal")
	})
}

// gitInit + writeFixtureFile + gitRun mirror the analyze-level
// helpers in cmd/signatory/analyze_git_functional_test.go. We
// duplicate here because those helpers are package-private to the
// cmd/signatory tests.

func gitInit(t *testing.T, dir string) {
	t.Helper()
	gitRun(t, dir, "init", "-b", "main", "-q")
	gitRun(t, dir, "config", "user.email", "test@example.invalid")
	gitRun(t, dir, "config", "user.name", "Test")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	gitRun(t, dir, "config", "tag.gpgSign", "false")
}

func writeFixtureFile(t *testing.T, repo, relPath, body string) {
	t.Helper()
	full := filepath.Join(repo, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	full := append([]string{"-C", repo}, args...)
	cmd := gitenv.NewCmd(t.Context(), full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, repo, err, stderr.String())
	}
}
