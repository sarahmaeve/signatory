package git

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollector_FirstCommitDate_EmptyRepo_Absence(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "empty"})
	require.NoError(t, err)

	entry := indexByType(result)["first_commit_date"]
	require.NotNil(t, entry.Absence, "empty repo → first_commit_date absence")
	assert.Contains(t, entry.Absence.Reason, "no commits")
}

func TestCollector_FirstCommitDate_SingleCommit(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "seed")

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "one-commit"})
	require.NoError(t, err)

	sig := findSignal(t, result, "first_commit_date")
	v := unmarshalValue(t, sig)
	require.NotEmpty(t, v["date"])
	require.NotEmpty(t, v["era"])

	// Date should be parseable as RFC3339 and recent (≤ one minute old).
	parsed, err := time.Parse(time.RFC3339, v["date"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().UTC(), parsed, time.Minute)

	// days_ago is non-negative for a just-made commit; exact 0 or 1
	// depending on clock skew and UTC-day boundary handling.
	daysAgo, ok := v["days_ago"].(float64)
	require.True(t, ok)
	assert.GreaterOrEqual(t, int(daysAgo), 0)
}

func TestCollector_FirstCommitDate_MultipleCommits_ReturnsFirst(t *testing.T) {
	// Three commits, each with an explicit backdated author date.
	// The collector must return the EARLIEST date, not the latest.
	t.Parallel()

	repo := initRepo(t)

	for _, ct := range []struct{ date, msg string }{
		{"2020-01-15T00:00:00Z", "oldest"},
		{"2022-06-01T00:00:00Z", "middle"},
		{"2024-03-10T00:00:00Z", "newest"},
	} {
		//nolint:gosec // G204: test helper
		cmd := exec.Command("git", "-C", repo, "commit", "--allow-empty", "-m", ct.msg)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_DATE="+ct.date,
			"GIT_COMMITTER_DATE="+ct.date,
		)
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "commit %q: %s", ct.msg, out)
	}

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "multi"})
	require.NoError(t, err)

	sig := findSignal(t, result, "first_commit_date")
	v := unmarshalValue(t, sig)

	parsed, err := time.Parse(time.RFC3339, v["date"].(string))
	require.NoError(t, err)

	expected, _ := time.Parse(time.RFC3339, "2020-01-15T00:00:00Z")
	assert.Equal(t, expected.Unix(), parsed.Unix(),
		"first_commit_date should be the earliest commit, not the latest")

	// Era classification should reflect the 2020 date (Era 1 or 2
	// depending on the trust-model thresholds; we don't hardcode
	// which, just that the field is populated with a known era).
	era := v["era"].(string)
	assert.NotEmpty(t, era, "era should be populated")
}
