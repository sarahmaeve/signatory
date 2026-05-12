package pypi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// classifyPublisherLogin is the pure-function classifier. Tests
// run against the four classes (human / bot / service-account /
// unknown) and the rule-precedence ordering. Forgery resistance is
// medium-declining by design: an attacker can rename their account
// to bypass any pattern. The signal is heuristic risk-stratification,
// not a verdict.

func TestClassifyPublisherLogin_Empty(t *testing.T) {
	t.Parallel()
	got := classifyPublisherLogin("")
	assert.Equal(t, "unknown", got.Class)
	assert.Empty(t, got.MatchedPattern, "empty input shouldn't claim a matched pattern")
	assert.Empty(t, got.Login)
}

func TestClassifyPublisherLogin_HumanName(t *testing.T) {
	t.Parallel()
	got := classifyPublisherLogin("alice")
	assert.Equal(t, "human", got.Class)
	assert.Equal(t, "alice", got.Login)
	assert.Empty(t, got.MatchedPattern,
		"human classification is the default — no pattern should be reported")
}

func TestClassifyPublisherLogin_GitHubBotSuffix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		login string
	}{
		{"dependabot[bot]"},
		{"github-actions[bot]"},
		{"renovate[bot]"},
		{"claude[bot]"},
	}
	for _, tc := range cases {
		t.Run(tc.login, func(t *testing.T) {
			got := classifyPublisherLogin(tc.login)
			assert.Equal(t, "bot", got.Class,
				"GitHub's [bot] suffix is the canonical bot-identity convention; must classify as bot, not service-account")
			assert.Equal(t, "[bot]", got.MatchedPattern)
		})
	}
}

func TestClassifyPublisherLogin_ServiceAccountSuffix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		login           string
		wantClass       string
		wantPattern     string
		wantDescription string
	}{
		{"tj-actions-bot", "service-account", "-bot",
			"the tj-actions case study — `@tj-actions-bot` PAT theft motivated this signal"},
		{"aws-publish-bot", "service-account", "-bot", ""},
		{"my-ci", "service-account", "-ci", ""},
		{"foo-deploy", "service-account", "-deploy", ""},
		{"app-svc", "service-account", "-svc", ""},
		{"microsoft-publisher", "service-account", "-publisher", ""},
		{"foo-release", "service-account", "-release", ""},
		{"bar-automation", "service-account", "-automation", ""},
	}
	for _, tc := range cases {
		t.Run(tc.login, func(t *testing.T) {
			got := classifyPublisherLogin(tc.login)
			assert.Equal(t, tc.wantClass, got.Class, tc.wantDescription)
			assert.Equal(t, tc.wantPattern, got.MatchedPattern)
		})
	}
}

func TestClassifyPublisherLogin_CaseInsensitive(t *testing.T) {
	t.Parallel()
	got := classifyPublisherLogin("MY-BOT")
	assert.Equal(t, "service-account", got.Class,
		"pattern matching is case-insensitive; UPPER-CASE-BOT should still classify")
	assert.Equal(t, "-bot", got.MatchedPattern)
	assert.Equal(t, "MY-BOT", got.Login,
		"login preserves original case in the signal value")
}

func TestClassifyPublisherLogin_NoSeparatorIsHuman(t *testing.T) {
	t.Parallel()
	// v1 is conservative: requires a hyphen separator before the
	// suffix. "deploybot" and "npmbot" fall through to human —
	// accepted tradeoff for low false-positive rate. False-negatives
	// here still produce useful data (the publisher entity is minted,
	// the maintainer_count emits); only the automation-flag is missed.
	cases := []string{
		"deploybot", // looks botty but no hyphen separator
		"npmbot",    // same
		"robot",     // human-named accounts that contain "bot"
		"botanist",  // ditto
		"ciara",     // contains "ci" but as part of a name
	}
	for _, login := range cases {
		t.Run(login, func(t *testing.T) {
			got := classifyPublisherLogin(login)
			assert.Equal(t, "human", got.Class,
				"v1 is conservative — no separator before suffix means no automation classification")
		})
	}
}

func TestClassifyPublisherLogin_GitHubBotSuffixWinsOverServiceAccount(t *testing.T) {
	t.Parallel()
	// "[bot]" suffix is the most-specific GitHub convention — when
	// it matches, classify as bot, not service-account. Pin
	// rule-precedence ordering.
	got := classifyPublisherLogin("ci-helper[bot]")
	assert.Equal(t, "bot", got.Class,
		"[bot] suffix takes precedence over service-account suffixes")
	assert.Equal(t, "[bot]", got.MatchedPattern)
}
