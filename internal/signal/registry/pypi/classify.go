package pypi

import (
	"fmt"
	"strings"
)

// publisherClassification holds the result of classifying a single
// publisher login. Pure function output — no I/O, no entity-store
// dependencies, no network. Forgery resistance is medium-declining
// by design: an attacker can rename their account to bypass any
// pattern. The signal is heuristic risk-stratification, not a
// verdict.
//
// Class values:
//   - "human"           — default; no automation-naming pattern matched
//   - "bot"             — GitHub's canonical "[bot]" suffix
//   - "service-account" — automation-account naming patterns
//     (e.g., "-bot", "-ci", "-deploy", "-svc",
//     "-release", "-publisher", "-automation")
//   - "unknown"         — empty login (defensive)
//
// MatchedPattern is the specific substring that fired the rule,
// empty when class is "human" or "unknown". Reason is a
// human-readable explanation suitable for surfacing on the
// emitted signal.
type publisherClassification struct {
	Login          string
	Class          string
	MatchedPattern string
	Reason         string
}

// serviceAccountSuffixes are the hyphen-prefixed suffix patterns
// that classify a login as a service-account. v1 is conservative:
// requires a hyphen separator before the suffix to keep false-positive
// rate low. "deploybot" and "npmbot" fall through to "human" by
// design — accepted tradeoff. Future iterations may add no-separator
// detection or an explicit allowlist of known automation accounts.
//
// Sources: empirical patterns from the tj-actions case study
// (@tj-actions-bot), Mini-Shai-Hulud campaign reporting
// (publisher-side bot accounts), and conventional CI/CD naming.
var serviceAccountSuffixes = []string{
	"-bot",
	"-ci",
	"-deploy",
	"-svc",
	"-release",
	"-publisher",
	"-automation",
}

// classifyPublisherLogin applies name-pattern heuristics to a login
// string. Pure function: same input → same output.
//
// Rules in order (first match wins):
//  1. Empty login → "unknown" (defensive — extractPyPILogins should
//     already filter empties, but classifying the empty string as
//     anything else would be misleading)
//  2. "[bot]" suffix (GitHub bot convention) → "bot"
//  3. Hyphen-prefixed automation suffix → "service-account"
//  4. Otherwise → "human"
//
// Case-insensitive on pattern matching; preserves original case in
// the returned Login field.
func classifyPublisherLogin(login string) publisherClassification {
	if login == "" {
		return publisherClassification{
			Class:  "unknown",
			Reason: "empty login string",
		}
	}

	lowered := strings.ToLower(login)

	// Rule 2: GitHub's canonical "[bot]" suffix.
	if strings.HasSuffix(lowered, "[bot]") {
		return publisherClassification{
			Login:          login,
			Class:          "bot",
			MatchedPattern: "[bot]",
			Reason:         "login ends with GitHub bot suffix '[bot]'",
		}
	}

	// Rule 3: hyphen-prefixed automation-account suffixes.
	for _, suffix := range serviceAccountSuffixes {
		if strings.HasSuffix(lowered, suffix) {
			return publisherClassification{
				Login:          login,
				Class:          "service-account",
				MatchedPattern: suffix,
				Reason:         fmt.Sprintf("login ends with automation-account suffix %q", suffix),
			}
		}
	}

	// Rule 4: default to human.
	return publisherClassification{
		Login:  login,
		Class:  "human",
		Reason: "no automation-naming pattern matched",
	}
}
