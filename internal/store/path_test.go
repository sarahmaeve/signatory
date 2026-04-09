package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultDBPath(t *testing.T) {
	path, err := DefaultDBPath()
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	expected := filepath.Join(home, ".signatory", "signatory.db")
	assert.Equal(t, expected, path)
}

func TestDefaultDBPath_NoTilde(t *testing.T) {
	path, err := DefaultDBPath()
	require.NoError(t, err)
	assert.False(t, strings.HasPrefix(path, "~"), "path should not start with ~")
}

func TestResolvePath_TildePrefix(t *testing.T) {
	path, err := ResolvePath("~/mydir/my.db")
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, "mydir", "my.db"), path)
}

func TestResolvePath_TildeOnly(t *testing.T) {
	path, err := ResolvePath("~")
	require.NoError(t, err)

	// ~ alone should resolve to the default path.
	defaultPath, err := DefaultDBPath()
	require.NoError(t, err)
	assert.Equal(t, defaultPath, path)
}

func TestResolvePath_EmptyString(t *testing.T) {
	path, err := ResolvePath("")
	require.NoError(t, err)

	defaultPath, err := DefaultDBPath()
	require.NoError(t, err)
	assert.Equal(t, defaultPath, path)
}

func TestResolvePath_AbsolutePath(t *testing.T) {
	path, err := ResolvePath("/var/data/signatory.db")
	require.NoError(t, err)
	assert.Equal(t, "/var/data/signatory.db", path)
}

func TestResolvePath_RelativePath(t *testing.T) {
	path, err := ResolvePath("data/signatory.db")
	require.NoError(t, err)
	assert.Equal(t, "data/signatory.db", path)
	assert.False(t, strings.HasPrefix(path, "~"))
}

func TestResolvePath_TrailingSlash(t *testing.T) {
	path, err := ResolvePath("~/mydir/")
	require.NoError(t, err)

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	// filepath.Join cleans trailing slashes.
	assert.Equal(t, filepath.Join(home, "mydir"), path)
}

func TestResolvePath_DotDot(t *testing.T) {
	path, err := ResolvePath("/var/data/../signatory.db")
	require.NoError(t, err)
	// filepath.Clean resolves .. segments.
	assert.Equal(t, "/var/signatory.db", path)
}

func TestResolvePath_TildeInMiddle(t *testing.T) {
	// ~ in the middle of a path should NOT be expanded — only ~/prefix.
	path, err := ResolvePath("/var/~user/data.db")
	require.NoError(t, err)
	assert.Equal(t, "/var/~user/data.db", path)
}
