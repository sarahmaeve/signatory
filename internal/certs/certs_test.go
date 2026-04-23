package certs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Shared fixture: a minimal byte sequence that x509.ParsePEM accepts
// as "looks like a certificate." We don't need cryptographic
// validity — Check only verifies that the file looks like a PEM
// cert block. A real signature check would couple these tests to a
// specific test-only key, which adds no integrity value here.
func pemFixture(t *testing.T) []byte {
	t.Helper()
	return []byte(`-----BEGIN CERTIFICATE-----
MIIBkTCB+wIJAKH/EhuqJjtGMA0GCSqGSIb3DQEBCwUAMBoxGDAWBgNVBAMMD3Rl
c3Qtc2lnbmF0b3J5LWNhMB4XDTI2MDQyMjAwMDAwMFoXDTM2MDQyMjAwMDAwMFow
GjEYMBYGA1UEAwwPdGVzdC1zaWduYXRvcnktY2EwgZ8wDQYJKoZIhvcNAQEBBQAD
gY0AMIGJAoGBAM3fake3fake3fake3fake3fake3fake3fake3fake3fake3fake
-----END CERTIFICATE-----
`)
}

// --- Check: preflight ------------------------------------------------------
//
// The preflight is signatory's answer to NODE_EXTRA_CA_CERTS being an
// ambient env var that silently vanishes across shell restarts. Tests
// pin the exact failure taxonomy so the CLI's exit-code mapping and
// remediation-message quality don't drift.

func TestCheck_EnvUnset(t *testing.T) {
	r := CheckWithEnv(func(string) string { return "" })
	assert.False(t, r.OK, "empty env must fail preflight")
	assert.Equal(t, FailEnvUnset, r.Code)
	assert.Contains(t, r.Message, EnvVar,
		"failure message should name the env var so the user knows what to set")
	assert.NotEmpty(t, r.Fix,
		"every failure must include a remediation hint — the whole point of this preflight")
}

func TestCheck_PathMissing(t *testing.T) {
	r := CheckWithEnv(func(string) string { return "/definitely/not/real/rootCA.pem" })
	assert.False(t, r.OK)
	assert.Equal(t, FailPathMissing, r.Code)
	assert.Contains(t, r.Message, "/definitely/not/real/rootCA.pem",
		"failure should name the offending path")
}

func TestCheck_NotAPEM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootCA.pem")
	require.NoError(t, os.WriteFile(path, []byte("this is not a pem file"), 0o600))

	r := CheckWithEnv(func(string) string { return path })
	assert.False(t, r.OK)
	assert.Equal(t, FailPathInvalid, r.Code)
}

func TestCheck_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rootCA.pem")
	require.NoError(t, os.WriteFile(path, pemFixture(t), 0o600))

	r := CheckWithEnv(func(string) string { return path })
	assert.True(t, r.OK, "valid PEM at env-var path must pass; message=%q fix=%q", r.Message, r.Fix)
	assert.Equal(t, StatusOK, r.Code)
	assert.Empty(t, r.Fix, "OK result must not carry a remediation hint")
}

func TestCheck_ExpandsTilde(t *testing.T) {
	// Guards the case where a user (or a shell profile) writes
	// NODE_EXTRA_CA_CERTS=~/.signatory/certs/rootCA.pem — single-quoted,
	// or exported from a context that didn't expand `~`. Without this
	// expansion the path would be treated as literal "~/..." and fail.
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "rootCA.pem")
	require.NoError(t, os.WriteFile(path, pemFixture(t), 0o600))

	r := CheckWithEnv(func(string) string { return "~/rootCA.pem" })
	assert.True(t, r.OK, "tilde should expand via HOME; got: %s / %s", r.Message, r.Fix)
}

// --- Init: create + copy CA to a stable, signatory-owned path --------------

func TestInit_CopiesCAFromMkcert(t *testing.T) {
	certDir := t.TempDir()
	fixture := filepath.Join(t.TempDir(), "rootCA.pem")
	require.NoError(t, os.WriteFile(fixture, pemFixture(t), 0o600))

	restore := setMkcertCAForTest(func() (string, error) { return fixture, nil })
	t.Cleanup(restore)

	result, err := Init(InitOptions{CertDir: certDir, Stderr: io_discard()})
	require.NoError(t, err)

	assert.Equal(t, certDir, result.CertDir)
	assert.Equal(t, filepath.Join(certDir, CAFileName), result.CAPath)

	// The copy must actually exist at the destination and match the source.
	got, err := os.ReadFile(result.CAPath)
	require.NoError(t, err)
	want, err := os.ReadFile(fixture)
	require.NoError(t, err)
	assert.Equal(t, want, got, "copied CA should be byte-identical to source")
}

func TestInit_Idempotent(t *testing.T) {
	certDir := t.TempDir()
	fixture := filepath.Join(t.TempDir(), "rootCA.pem")
	require.NoError(t, os.WriteFile(fixture, pemFixture(t), 0o600))
	restore := setMkcertCAForTest(func() (string, error) { return fixture, nil })
	t.Cleanup(restore)

	_, err := Init(InitOptions{CertDir: certDir, Stderr: io_discard()})
	require.NoError(t, err, "first run must succeed")
	_, err = Init(InitOptions{CertDir: certDir, Stderr: io_discard()})
	require.NoError(t, err, "second run must not fail — init is idempotent")

	_, statErr := os.Stat(filepath.Join(certDir, CAFileName))
	assert.NoError(t, statErr, "CA must still exist after repeated init")
}

func TestInit_RefreshesStaleCAOnRerun(t *testing.T) {
	// If mkcert rotates its CA (reinstall), `certs init` should pick up
	// the new bytes on next run. Otherwise the signatory-owned copy
	// silently diverges from the system-trusted CA and TLS verification
	// fails mysteriously.
	certDir := t.TempDir()
	fixture := filepath.Join(t.TempDir(), "rootCA.pem")

	// First: seed with version-1 CA.
	require.NoError(t, os.WriteFile(fixture, pemFixture(t), 0o600))
	restore := setMkcertCAForTest(func() (string, error) { return fixture, nil })
	t.Cleanup(restore)

	_, err := Init(InitOptions{CertDir: certDir, Stderr: io_discard()})
	require.NoError(t, err)

	// Now mkcert "rotates" — write distinct bytes to the fixture.
	newBytes := append(pemFixture(t), []byte("# rotated\n")...)
	require.NoError(t, os.WriteFile(fixture, newBytes, 0o600))

	_, err = Init(InitOptions{CertDir: certDir, Stderr: io_discard()})
	require.NoError(t, err)

	got, _ := os.ReadFile(filepath.Join(certDir, CAFileName))
	assert.Equal(t, newBytes, got, "second init must refresh the managed copy")
}

func TestInit_MkcertNotFound(t *testing.T) {
	certDir := t.TempDir()
	restore := setMkcertCAForTest(func() (string, error) { return "", ErrMkcertNotFound })
	t.Cleanup(restore)

	_, err := Init(InitOptions{CertDir: certDir, Stderr: io_discard()})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMkcertNotFound),
		"caller needs to distinguish missing-mkcert from other failures to render a useful install hint")
}

func TestInit_CreatesCertDirWhenMissing(t *testing.T) {
	root := t.TempDir()
	certDir := filepath.Join(root, "nested", "does", "not", "exist")
	fixture := filepath.Join(t.TempDir(), "rootCA.pem")
	require.NoError(t, os.WriteFile(fixture, pemFixture(t), 0o600))
	restore := setMkcertCAForTest(func() (string, error) { return fixture, nil })
	t.Cleanup(restore)

	_, err := Init(InitOptions{CertDir: certDir, Stderr: io_discard()})
	require.NoError(t, err)

	info, err := os.Stat(certDir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// --- WriteProfile: idempotent shell profile patching -----------------------
//
// The goal: eliminate the "ambient env var" failure mode entirely for
// users who opt in. Re-running must be safe: no duplicate blocks, no
// lost user content, and changes to CAPath must be picked up cleanly
// without manual profile surgery.

func TestWriteProfile_AppendsWhenMissing(t *testing.T) {
	profile := filepath.Join(t.TempDir(), ".zshrc")
	original := "export PATH=/foo:$PATH\nalias ll='ls -la'\n"
	require.NoError(t, os.WriteFile(profile, []byte(original), 0o600))

	result, err := WriteProfile(WriteProfileOptions{
		ProfilePath: profile,
		CAPath:      "/Users/sarah/.signatory/certs/rootCA.pem",
	})
	require.NoError(t, err)
	assert.Equal(t, ProfileAppended, result.Action)

	got := readFile(t, profile)
	assert.Contains(t, got, original, "user's pre-existing content must survive verbatim")
	assert.Contains(t, got, ProfileMarkerBegin)
	assert.Contains(t, got, ProfileMarkerEnd)
	assert.Contains(t, got,
		`export NODE_EXTRA_CA_CERTS="/Users/sarah/.signatory/certs/rootCA.pem"`)
}

func TestWriteProfile_ReplacesExistingBlock(t *testing.T) {
	profile := filepath.Join(t.TempDir(), ".zshrc")
	initial := "line-before\n" +
		ProfileMarkerBegin + "\n" +
		`export NODE_EXTRA_CA_CERTS="/old/path/rootCA.pem"` + "\n" +
		ProfileMarkerEnd + "\n" +
		"line-after\n"
	require.NoError(t, os.WriteFile(profile, []byte(initial), 0o600))

	result, err := WriteProfile(WriteProfileOptions{
		ProfilePath: profile,
		CAPath:      "/new/path/rootCA.pem",
	})
	require.NoError(t, err)
	assert.Equal(t, ProfileReplaced, result.Action)

	got := readFile(t, profile)
	assert.Contains(t, got, "line-before")
	assert.Contains(t, got, "line-after")
	assert.Contains(t, got, "/new/path/rootCA.pem")
	assert.NotContains(t, got, "/old/path/rootCA.pem",
		"old value must be gone after replacement — no stale exports")
	assert.Equal(t, 1, strings.Count(got, ProfileMarkerBegin),
		"exactly one managed block; re-running must not duplicate")
	assert.Equal(t, 1, strings.Count(got, ProfileMarkerEnd))
}

func TestWriteProfile_UnchangedWhenIdentical(t *testing.T) {
	profile := filepath.Join(t.TempDir(), ".zshrc")

	_, err := WriteProfile(WriteProfileOptions{ProfilePath: profile, CAPath: "/p/rootCA.pem"})
	require.NoError(t, err)

	r2, err := WriteProfile(WriteProfileOptions{ProfilePath: profile, CAPath: "/p/rootCA.pem"})
	require.NoError(t, err)
	assert.Equal(t, ProfileUnchanged, r2.Action,
		"second run with identical CAPath must report Unchanged — lets CLI skip the log line")
}

func TestWriteProfile_CreatesFileWhenMissing(t *testing.T) {
	profile := filepath.Join(t.TempDir(), ".zshrc")
	// Deliberately do not create the file — some users have no existing
	// zshrc, and `signatory certs init --write-profile` should work on a
	// fresh machine without a manual `touch ~/.zshrc` step.

	result, err := WriteProfile(WriteProfileOptions{
		ProfilePath: profile,
		CAPath:      "/p/rootCA.pem",
	})
	require.NoError(t, err)
	assert.Equal(t, ProfileCreated, result.Action)

	got := readFile(t, profile)
	assert.Contains(t, got, ProfileMarkerBegin)
	assert.Contains(t, got, "/p/rootCA.pem")
}

// --- test helpers ----------------------------------------------------------

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// io_discard returns an io.Writer that drops all output. We use a
// local helper rather than io.Discard directly so tests stay
// self-documenting about intent and future hooks can switch to a
// capture buffer without churning every call site.
func io_discard() *discardWriter { return &discardWriter{} }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
