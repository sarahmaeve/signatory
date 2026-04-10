package identity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecurity_ValidateIdentity_Accepts verifies that legitimate team
// identities — including the canonical "team:human+llm" form, the
// fallback "team:user+unassisted" form, and Unix-style usernames with
// underscores — pass validation. If this test breaks, the validator
// has gotten too strict and would reject real configurations.
func TestSecurity_ValidateIdentity_Accepts(t *testing.T) {
	tests := []string{
		"team:sarah+claude-opus-4.6",
		"team:james.park+gemini-2.5-flash",
		"team:testuser+unassisted",
		"team:unknown+unassisted",
		"team:Sarah+claude",                       // capital letter
		"team:john_doe+claude",                    // underscore in user (Unix usernames allow it)
		"team:user.name+claude-3.5-sonnet-latest", // multiple dots
	}
	for _, s := range tests {
		t.Run(s, func(t *testing.T) {
			assert.NoError(t, ValidateIdentity(s),
				"valid team identity should not be rejected")
		})
	}
}

// TestSecurity_ValidateIdentity_Rejects covers the attack vectors from
// issue #101. Each input represents a class of malformed/malicious
// data that must NOT be allowed to flow into the audit_log.actor /
// posture.set_by / burn.burned_by columns or the JSON audit file.
func TestSecurity_ValidateIdentity_Rejects(t *testing.T) {
	tests := []struct {
		name string
		s    string
	}{
		{"empty", ""},
		{"missing team prefix", "sarah+claude"},
		{"empty body after prefix", "team:"},
		{"control char NUL", "team:sarah\x00injected"},
		{"control char newline", "team:sarah\ninjected"},
		{"control char tab", "team:sarah\tinjected"},
		{"DEL char", "team:sarah\x7finjected"},
		{"ANSI escape clear screen", "team:sarah\x1b[2J"},
		{"non-ASCII Cyrillic lookalike", "team:lod\u0430sh+claude"},
		{"non-ASCII emoji", "team:sarah+\U0001f4a9"},
		{"shell metacharacter semicolon", "team:sarah;rm -rf /"},
		{"shell backtick", "team:sarah`whoami`"},
		{"shell dollar paren", "team:sarah$(whoami)"},
		{"forward slash", "team:sarah/path"},
		{"space in body", "team:sarah claude"},
		{"too long 1MB", "team:" + strings.Repeat("x", 1<<20)},
		{"too long just over cap", "team:" + strings.Repeat("x", 129)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIdentity(tc.s)
			require.Error(t, err, "malformed team identity must be rejected: %q", tc.s)
		})
	}
}

// TestSecurity_Current_RejectsMalformedSignatoryTeamEnv is the
// integration counterpart that verifies Current() actually plumbs
// ValidateIdentity into the env-var resolution path. Without this
// test, a future refactor that drops the validator call from Current()
// would silently re-introduce issue #101 and the unit test of
// ValidateIdentity would still pass.
//
// Note: NUL bytes are not exercised here because os.Setenv (and
// t.Setenv) refuse NUL bytes at the runtime layer — POSIX env vars
// cannot contain NUL bytes. The team-file resolution path (tested
// separately in TestSecurity_Current_RejectsMalformedTeamFile) can
// carry NUL bytes via os.WriteFile and is the relevant test for
// that vector.
func TestSecurity_Current_RejectsMalformedSignatoryTeamEnv(t *testing.T) {
	tests := []struct {
		name string
		env  string
	}{
		{"control char newline", "team:sarah\ninjected"},
		{"control char tab", "team:sarah\tinjected"},
		{"non-ASCII", "team:lod\u0430sh+claude"},
		{"shell injection", "team:sarah;rm -rf /"},
		{"backtick", "team:sarah`whoami`"},
		{"too long", "team:" + strings.Repeat("x", 200)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withHome(t)
			t.Setenv("SIGNATORY_TEAM", tc.env)

			_, err := Current()
			require.Error(t, err, "Current() must reject malformed SIGNATORY_TEAM env var")
			assert.Contains(t, err.Error(), "SIGNATORY_TEAM env var",
				"error must identify the SIGNATORY_TEAM source so the user knows where to fix it")
		})
	}
}

// TestSecurity_Current_RejectsMalformedTeamFile verifies the same
// validation runs on the ~/.signatory/team file resolution path.
func TestSecurity_Current_RejectsMalformedTeamFile(t *testing.T) {
	home := withHome(t)
	t.Setenv("SIGNATORY_TEAM", "")

	dir := filepath.Join(home, ".signatory")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "team"), []byte("team:sarah\x00injected\n"), 0600))

	_, err := Current()
	require.Error(t, err, "Current() must reject malformed ~/.signatory/team file")
	assert.Contains(t, err.Error(), ".signatory/team file",
		"error must identify the team file source so the user knows where to fix it")
}
