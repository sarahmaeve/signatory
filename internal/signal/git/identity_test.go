package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- parser / helper unit tests ----

func TestParseMailmap(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		data string
		want int
	}{
		{"empty file", "", 0},
		{"only newlines", "\n\n\n", 0},
		{"only comments", "# top\n# middle\n# bottom\n", 0},
		{
			name: "single entry",
			data: "Alice <alice@example.com>\n",
			want: 1,
		},
		{
			name: "four entries with comments and blanks mixed",
			data: `# Mapping file for example/project
Alice Smith <alice@example.com>

# The original had a typo
<bob@example.com> <bob.old@example.com>
Carol Kim <carol@example.com> <carol@old-employer.com>
Dave <dave@example.com>
`,
			want: 4,
		},
		{
			name: "leading-whitespace comment still counts as comment",
			data: "   # this is indented\nAlice <a@x>\n",
			want: 1,
		},
		{
			name: "trailing whitespace on entry line still counts",
			data: "Alice <a@x>   \n",
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseMailmap([]byte(tc.data))
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestExtractEmailDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"alice@example.com", "example.com"},
		{"bob@Example.COM", "example.com"}, // lowercased
		{"Trailing.Space@Example.com   ", "example.com"},
		{"   leading.space@example.com", "example.com"},
		{"multi@at@example.com", "example.com"}, // take after last @
		{"", ""},
		{"no-at-sign", ""},
		{"@", ""},
		{"user@", ""},
		{"@example.com", "example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, extractEmailDomain(tc.in))
		})
	}
}

func TestParseAuthorshipLog(t *testing.T) {
	t.Parallel()

	const us = "\x1f"
	cases := []struct {
		name string
		data string
		want []authorshipRow
	}{
		{"empty input", "", []authorshipRow{}},
		{
			name: "single row",
			data: "Alice" + us + "alice@example.com\n",
			want: []authorshipRow{{Name: "Alice", Email: "alice@example.com"}},
		},
		{
			name: "three rows",
			data: "Alice" + us + "a@x\n" + "Bob" + us + "b@x\n" + "Alice" + us + "a@x\n",
			want: []authorshipRow{
				{Name: "Alice", Email: "a@x"},
				{Name: "Bob", Email: "b@x"},
				{Name: "Alice", Email: "a@x"},
			},
		},
		{
			name: "truncated rows are skipped",
			data: "bad-no-us-here\n" + "Alice" + us + "a@x\n",
			want: []authorshipRow{{Name: "Alice", Email: "a@x"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseAuthorshipLog([]byte(tc.data))
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestTopNFromMap(t *testing.T) {
	t.Parallel()

	t.Run("empty map", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, topNFromMap(map[string]int{}, 10))
	})

	t.Run("sorted descending with tie broken alphabetically", func(t *testing.T) {
		t.Parallel()
		got := topNFromMap(map[string]int{
			"alpha":   3,
			"bravo":   10,
			"charlie": 3,
			"delta":   5,
		}, 10)
		want := []topNEntry{
			{"bravo", 10},
			{"delta", 5},
			{"alpha", 3},
			{"charlie", 3},
		}
		assert.Equal(t, want, got)
	})

	t.Run("n bounds result size", func(t *testing.T) {
		t.Parallel()
		got := topNFromMap(map[string]int{
			"a": 1, "b": 2, "c": 3, "d": 4, "e": 5,
		}, 2)
		assert.Len(t, got, 2)
		assert.Equal(t, "e", got[0].Key)
		assert.Equal(t, "d", got[1].Key)
	})

	t.Run("n=0 returns all entries", func(t *testing.T) {
		t.Parallel()
		got := topNFromMap(map[string]int{"a": 1, "b": 2}, 0)
		assert.Len(t, got, 2)
	})
}

// ---- integration tests ----

func TestCollector_NoMailmap_RecordsAbsence(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "seed")

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "no-mailmap"})
	require.NoError(t, err)

	entry := indexByType(result)["identity_graph_depth"]
	require.NotNil(t, entry.Absence, "no .mailmap → absence")
	assert.Contains(t, entry.Absence.Reason, ".mailmap")
}

func TestCollector_MailmapPresent_EmitsSignal(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "seed")

	mailmap := `# Sample mailmap
Alice Smith <alice@example.com>
<bob@example.com> <bob.old@example.com>
Carol <carol@example.com> <carol@employer.com>
`
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".mailmap"), []byte(mailmap), 0o600))

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "has-mailmap"})
	require.NoError(t, err)

	sig := findSignal(t, result, "identity_graph_depth")
	v := unmarshalValue(t, sig)
	assert.Equal(t, float64(3), v["mailmap_entries"])
	assert.Equal(t, true, v["present"])
}

func TestCollector_DomainConsistency_TalliesFromLog(t *testing.T) {
	// Three commits by three authors across two domains:
	// two commits at example.com, one at other.example.
	// Expect unique_domains=2 and top_domain_share=2/3.
	t.Parallel()

	repo := initRepo(t)
	commitAs(t, repo, "Alice", "alice@example.com", "first")
	commitAs(t, repo, "Bob", "bob@example.com", "second")
	commitAs(t, repo, "Carol", "carol@other.example", "third")

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "domain-test"})
	require.NoError(t, err)

	sig := findSignal(t, result, "identity_domain_consistency")
	v := unmarshalValue(t, sig)
	assert.Equal(t, float64(3), v["total_commits"])
	assert.Equal(t, float64(2), v["unique_domains"])
	assert.InDelta(t, 2.0/3.0, v["top_domain_share"], 1e-9)
}

func TestCollector_MaintainerConcentration_Top1AndTopK(t *testing.T) {
	// One author dominates: 4 commits by Alice, 1 by Bob.
	// top_author_share = 4/5 = 0.8.
	// top_k (default 3) share = 5/5 = 1.0 since only 2 authors.
	t.Parallel()

	repo := initRepo(t)
	for i := 0; i < 4; i++ {
		commitAs(t, repo, "Alice", "alice@example.com", fmt.Sprintf("alice %d", i))
	}
	commitAs(t, repo, "Bob", "bob@example.com", "bob 1")

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "concentration"})
	require.NoError(t, err)

	sig := findSignal(t, result, "effective_maintainer_concentration")
	v := unmarshalValue(t, sig)
	assert.Equal(t, float64(5), v["total_commits"])
	assert.Equal(t, float64(2), v["unique_authors"])
	assert.InDelta(t, 4.0/5.0, v["top_author_share"], 1e-9)
	assert.InDelta(t, 1.0, v["top_k_share"], 1e-9)
	assert.Equal(t, float64(topAuthorsShareCutoff), v["top_k"])
}

func TestCollector_Authorship_EmptyRepo_RecordsAbsence(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "empty"})
	require.NoError(t, err)

	entry := indexByType(result)["identity_domain_consistency"]
	require.NotNil(t, entry.Absence, "empty repo → domain-consistency absence")
	entry = indexByType(result)["effective_maintainer_concentration"]
	require.NotNil(t, entry.Absence, "empty repo → maintainer-concentration absence")
}

// commitAs creates one empty commit with the given author name,
// email, and message. Each commit's author identity is overridden
// via env vars so tests can freely model multi-contributor repos
// without running `git config` between commits.
func commitAs(t *testing.T, repo, name, email, msg string) {
	t.Helper()
	// Use mustRunGitEnv for the identity override; repurposing
	// mustRunGit would require signature changes the other tests
	// rely on.
	full := []string{"-C", repo, "commit", "--allow-empty", "-m", msg}
	//nolint:gosec // G204: test helper; binary is "git" literal
	cmd := exec.Command("git", full...)
	// Start from gitenv.SafeEnv() — strip dangerous inherited
	// vars — then append the identity overrides. Inheriting from
	// cmd.Environ() would leak GIT_DIR / GIT_CONFIG_* and risk
	// writes against the shared worktree config.
	cmd.Env = append(gitenv.SafeEnv(),
		"GIT_AUTHOR_NAME="+name,
		"GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name,
		"GIT_COMMITTER_EMAIL="+email,
	)
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "commit %q: %s", msg, out)
}
