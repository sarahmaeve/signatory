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
	"regexp"
	"strings"
)

// MaxTeamIdentityLength is the hard upper bound on a normalized team
// identity, including the "team:" prefix. Real-world identities are
// well under 50 characters; 133 (5 for "team:" + 128 body) is generous
// slack and a hard cap to prevent log/display blowup, expensive
// rendering, and length-based DoS from attacker-controlled input.
const MaxTeamIdentityLength = 133

// validIdentityBody restricts the post-"team:" portion of an identity
// to a printable, ASCII-only character set: alphanumerics plus the
// separators "._+-" used by the human+llm format from
// design/entity-model-v2.md §Actor Identity. Underscore is included
// because Unix usernames can contain it (the fallback uses $USER
// directly).
//
// ASCII-only is the deliberate v0.1 choice — same reasoning as #78's
// ValidateCanonicalURI: lookalike fragmentation via Cyrillic/Greek
// homoglyphs would split audit ownership across visually-identical
// near-duplicates, defeating the purpose of identifying who took an
// action. If a future need for non-ASCII names emerges, the right
// answer is Unicode NFC normalization at the boundary.
var validIdentityBody = regexp.MustCompile(`^[A-Za-z0-9_.+-]{1,128}$`)

// ValidateIdentity checks that s is safe to persist as an actor field
// in audit_log, posture.set_by, burn.burned_by, and signal_resolutions.
// resolved_by. It is the input-boundary defense for issue #101 — every
// path that produces a team identity (env var, ~/.signatory/team file,
// $USER fallback) routes through this validator before the result is
// returned from Current().
//
// Rules, in order:
//
//  1. Length 1..MaxTeamIdentityLength bytes.
//  2. Starts with the literal "team:" prefix (Current normalizes
//     missing prefixes; this validator runs after that step).
//  3. The post-"team:" body matches validIdentityBody.
//
// Rule 3 implicitly catches control characters (NUL, newline, tab,
// escape sequences), non-ASCII bytes (lookalike fragmentation), and
// shell-metacharacter injection.
func ValidateIdentity(s string) error {
	if s == "" {
		return fmt.Errorf("team identity is empty")
	}
	if len(s) > MaxTeamIdentityLength {
		return fmt.Errorf("team identity exceeds maximum length of %d bytes (got %d)",
			MaxTeamIdentityLength, len(s))
	}
	if !strings.HasPrefix(s, "team:") {
		return fmt.Errorf("team identity %q does not start with required \"team:\" prefix", s)
	}
	body := strings.TrimPrefix(s, "team:")
	if !validIdentityBody.MatchString(body) {
		return fmt.Errorf("team identity %q has invalid body — must match [A-Za-z0-9_.+-]{1,128}", s)
	}
	return nil
}

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
// The resolved identity is validated via ValidateIdentity before being
// returned (#101 — env var and team file contents must not flow into
// audit_log.actor / posture.set_by / etc. unsanitized). A validation
// failure surfaces immediately at command startup so the user notices
// and can fix their configuration, rather than corrupting the audit
// trail silently.
func Current() (string, error) {
	if v := strings.TrimSpace(os.Getenv("SIGNATORY_TEAM")); v != "" {
		s := normalize(v)
		if err := ValidateIdentity(s); err != nil {
			return "", fmt.Errorf("SIGNATORY_TEAM env var: %w", err)
		}
		return s, nil
	}

	if v, ok := readTeamFile(); ok {
		s := normalize(v)
		if err := ValidateIdentity(s); err != nil {
			return "", fmt.Errorf("~/.signatory/team file: %w", err)
		}
		return s, nil
	}

	s := fallback()
	if err := ValidateIdentity(s); err != nil {
		return "", fmt.Errorf("fallback team identity from $USER: %w", err)
	}
	return s, nil
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
