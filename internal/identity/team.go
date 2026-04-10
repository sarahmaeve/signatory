// Package identity resolves the current team identity — the
// human-LLM signing pair that owns trust decisions made in this
// session. Actor identity is attached to every audit log entry,
// posture set_by field, burn burned_by field, and resolution
// resolved_by field, so every trust-modifying action can be traced
// back to the specific team that made it.
//
// Team identity format is `team:<human>+<llm>` — e.g.,
// `team:sarah+claude-opus-4.6`. This matches the design in
// design/entity-model-v2.md §Actor Identity. The v0.1 implementation
// stores team identity as a plain string without cryptographic
// backing; PGP/GPG-backed signing will arrive with the attestation
// utility layer in a later milestone.
package identity

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Current returns the team identity string for this session.
//
// Resolution order (first non-empty wins):
//
//  1. SIGNATORY_TEAM environment variable
//  2. Contents of ~/.signatory/team (first non-blank line, trimmed)
//  3. Fallback: team:<$USER or $USERNAME or "unknown">+unassisted
//
// The fallback deliberately tags the LLM component as "unassisted" so
// that un-configured sessions are visibly distinguishable in the audit
// log from properly-configured human+LLM team runs.
//
// Current never returns an error — resolution always produces a string.
// An unreadable config file or unset env fall through to the next step.
func Current() string {
	if v := strings.TrimSpace(os.Getenv("SIGNATORY_TEAM")); v != "" {
		return normalize(v)
	}

	if v, ok := readTeamFile(); ok {
		return normalize(v)
	}

	return fallback()
}

// readTeamFile returns the first non-blank line of ~/.signatory/team,
// or ("", false) if the file is missing, empty, or unreadable.
func readTeamFile() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	path := filepath.Join(home, ".signatory", "team")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line, true
		}
	}
	return "", false
}

// fallback builds an "unassisted" team identity from the current user.
func fallback() string {
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME") // Windows fallback.
	}
	if user == "" {
		user = "unknown"
	}
	return fmt.Sprintf("team:%s+unassisted", user)
}

// normalize ensures the team identity starts with the "team:" prefix.
// A user might write just "sarah+claude-opus-4.6" in their config file
// and expect it to work — we accept that and add the prefix on their
// behalf. Strings already prefixed are returned unchanged.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "team:") {
		return s
	}
	return "team:" + s
}
