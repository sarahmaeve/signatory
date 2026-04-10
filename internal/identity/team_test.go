package identity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withHome sets $HOME to a temp dir for the duration of the test and
// restores it afterwards. Returns the temp dir path so the caller can
// seed files under it.
func withHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	return tmp
}

func TestCurrent_EnvVarTakesPrecedence(t *testing.T) {
	withHome(t)
	t.Setenv("SIGNATORY_TEAM", "team:sarah+claude-opus-4.6")

	assert.Equal(t, "team:sarah+claude-opus-4.6", Current())
}

func TestCurrent_EnvVarNormalized(t *testing.T) {
	// User might forget the "team:" prefix — we add it.
	withHome(t)
	t.Setenv("SIGNATORY_TEAM", "sarah+claude-opus-4.6")

	assert.Equal(t, "team:sarah+claude-opus-4.6", Current())
}

func TestCurrent_ReadsTeamFile(t *testing.T) {
	home := withHome(t)
	t.Setenv("SIGNATORY_TEAM", "")

	dir := filepath.Join(home, ".signatory")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "team"),
		[]byte("team:sarah+claude-opus-4.6\n"),
		0600))

	assert.Equal(t, "team:sarah+claude-opus-4.6", Current())
}

func TestCurrent_TeamFileNormalizedAndCommentsIgnored(t *testing.T) {
	home := withHome(t)
	t.Setenv("SIGNATORY_TEAM", "")

	dir := filepath.Join(home, ".signatory")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, "team"),
		[]byte("# comment line\n\nsarah+claude-opus-4.6\n"),
		0600))

	assert.Equal(t, "team:sarah+claude-opus-4.6", Current())
}

func TestCurrent_FallsBackToUserUnassisted(t *testing.T) {
	withHome(t)
	t.Setenv("SIGNATORY_TEAM", "")
	t.Setenv("USER", "testuser")

	assert.Equal(t, "team:testuser+unassisted", Current())
}

func TestCurrent_FallbackUnknown(t *testing.T) {
	withHome(t)
	t.Setenv("SIGNATORY_TEAM", "")
	t.Setenv("USER", "")
	t.Setenv("USERNAME", "")

	assert.Equal(t, "team:unknown+unassisted", Current())
}

// TestCurrent_EmptyTeamFileFallsThrough verifies that an empty team
// file does not short-circuit to an empty string — it continues to
// the fallback.
func TestCurrent_EmptyTeamFileFallsThrough(t *testing.T) {
	home := withHome(t)
	t.Setenv("SIGNATORY_TEAM", "")
	t.Setenv("USER", "testuser")

	dir := filepath.Join(home, ".signatory")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "team"), []byte("\n  \n"), 0600))

	assert.Equal(t, "team:testuser+unassisted", Current())
}
