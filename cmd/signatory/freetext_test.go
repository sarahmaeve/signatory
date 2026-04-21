package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadFreeText_InlineOnly covers the simple one-line case:
// --<name> set, --<name>-file unset, value flows through verbatim.
func TestReadFreeText_InlineOnly(t *testing.T) {
	t.Parallel()

	got, err := readFreeText("rationale", "audited by security team", "")
	require.NoError(t, err)
	assert.Equal(t, "audited by security team", got)
}

// TestReadFreeText_InlineWithNewline rejects a one-line flag value
// that contains a newline — the whole point of --<name>-file is that
// it exists for multi-line input, so an inline newline is a shell-
// quoting bug we should surface loudly.
func TestReadFreeText_InlineWithNewline(t *testing.T) {
	t.Parallel()

	_, err := readFreeText("rationale", "line1\nline2", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newline")
	assert.Contains(t, err.Error(), "--rationale-file")
}

// TestReadFreeText_FileRead covers the file form: --<name>-file
// points at a real file, contents come back minus the trailing
// newline a text editor typically leaves.
func TestReadFreeText_FileRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rationale.txt")
	require.NoError(t, os.WriteFile(path, []byte("multi\nline\nrationale\n"), 0o600))

	got, err := readFreeText("rationale", "", path)
	require.NoError(t, err)
	assert.Equal(t, "multi\nline\nrationale", got,
		"trailing newline should be stripped; interior newlines preserved")
}

// TestReadFreeText_FileRead_CRLF covers the CRLF trailing case
// for rationales edited on Windows or pasted from a web form —
// the final \r\n is stripped in one pass.
func TestReadFreeText_FileRead_CRLF(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rationale.txt")
	require.NoError(t, os.WriteFile(path, []byte("windows content\r\n"), 0o600))

	got, err := readFreeText("rationale", "", path)
	require.NoError(t, err)
	assert.Equal(t, "windows content", got)
}

// TestReadFreeText_FileRead_PreservesInteriorBlanks verifies that
// only ONE trailing newline is stripped — intentional blank lines
// before the final content survive.
func TestReadFreeText_FileRead_PreservesInteriorBlanks(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rationale.txt")
	require.NoError(t, os.WriteFile(path, []byte("header\n\nbody\n\n\n"), 0o600))

	got, err := readFreeText("rationale", "", path)
	require.NoError(t, err)
	assert.Equal(t, "header\n\nbody\n\n", got,
		"only the final single \\n is stripped; interior and preceding blank lines survive")
}

// TestReadFreeText_FileNotFound surfaces the OS error with the flag
// name in context, so the user sees which file they asked for.
func TestReadFreeText_FileNotFound(t *testing.T) {
	t.Parallel()

	_, err := readFreeText("rationale", "", "/does/not/exist/at/all.txt")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--rationale-file")
	assert.Contains(t, err.Error(), "/does/not/exist/at/all.txt")
}

// TestReadFreeText_Conflict covers the both-set case: exactly one of
// inline or file form must be used.
func TestReadFreeText_Conflict(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "rationale.txt")
	require.NoError(t, os.WriteFile(path, []byte("from file"), 0o600))

	_, err := readFreeText("rationale", "from flag", path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--rationale")
	assert.Contains(t, err.Error(), "--rationale-file")
	assert.Contains(t, err.Error(), "both set")
}

// TestReadFreeText_BothEmpty returns ("", nil). The helper doesn't
// decide whether empty is valid — that's the caller's policy (posture
// set requires non-empty; handoff --intake tolerates it).
func TestReadFreeText_BothEmpty(t *testing.T) {
	t.Parallel()

	got, err := readFreeText("rationale", "", "")
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestReadFreeText_NameInErrors verifies that every error message
// names the specific flag the caller used — "rationale" vs "reason"
// vs "intake" — so the user isn't hunting through unrelated flags
// for the mistake.
func TestReadFreeText_NameInErrors(t *testing.T) {
	t.Parallel()

	_, err := readFreeText("reason", "one\ntwo", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--reason")
	assert.NotContains(t, err.Error(), "rationale", "reason-labeled error must not mention rationale")
}
