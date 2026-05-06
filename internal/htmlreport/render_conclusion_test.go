package htmlreport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// fixtureConclusion returns a single Conclusion populated across every
// optional shape so the renderer's elision and rendering choices are
// all exercised by one fixture.
func fixtureConclusion() *exchange.Conclusion {
	signalType := "release-cadence"
	answersQ := "is the upstream actively maintained?"
	lineStart := 42
	lineEnd := 57
	commitSHA := "abc1234def5678"
	quoted := "func unsafePtrCast() <T> { ... }"

	return &exchange.Conclusion{
		ID:       "F003",
		Verdict:  "Release cadence has slowed materially since 2025-Q3",
		Severity: exchange.Severity{Default: exchange.SeverityHigh},
		Category: "vitality",
		Rationale: "The project published 14 releases in 2024 and 3 in 2025.\n" +
			"\n" +
			"This is a > 75% drop. <script>alert('x')</script> as text.",
		SignalType: &signalType,
		Citations: []exchange.Citation{
			{
				Path:      "CHANGELOG.md",
				LineStart: &lineStart,
				LineEnd:   &lineEnd,
				CommitSHA: &commitSHA,
				Quoted:    &quoted,
			},
			{
				Scope: &exchange.ScopeRef{
					Kind: "directory",
					Path: "internal/release/",
				},
			},
		},
		Prerequisites:    []string{"upstream maintainer absent", "no fork volunteered"},
		RemediationHints: []string{"pin to last 2024 release", "audit transitive on bump"},
		Supersedes: []exchange.Supersession{
			{PriorID: "F002", PriorRound: 1, Kind: exchange.SupersessionKindRefines},
		},
		AnswersQuestion:    &answersQ,
		RelatedConclusions: []string{"F001", "F999"}, // F999 is unresolved
	}
}

func fixtureConclusionPageInput() ConclusionPageInput {
	return ConclusionPageInput{
		Conclusion: fixtureConclusion(),
		OutputID:   "out-sec-0001abcdef",
		Analyst: exchange.AgentAttribution{
			AnalystID: "signatory-security-v1",
			Model:     "claude-opus-4-7",
			InvokedAt: "2026-05-06T12:00:00Z",
			Round:     2,
		},
		AnalystPagePath: "analysts/signatory-security-v1-r2.html",
		Plan: &LinkPlan{
			ConclusionPages: map[ConclusionKey]string{
				{OutputID: "out-sec-0001abcdef", LocalID: "F001"}: "conclusions/out-sec-0-F001.html",
				// F999 deliberately absent → unresolved, plain text.
			},
		},
		Page: PageContext{
			RootPrefix:  "../",
			GeneratedAt: "2026-05-06T13:00:00Z",
			Version:     "v0.1.0-test",
		},
	}
}

func TestRenderConclusionPage_HappyPath(t *testing.T) {
	in := fixtureConclusionPageInput()

	var buf bytes.Buffer
	require.NoError(t, RenderConclusionPage(&buf, in))
	out := buf.String()

	t.Run("HTML shell with stylesheet via root prefix", func(t *testing.T) {
		assert.Contains(t, out, "<!DOCTYPE html>")
		assert.Contains(t, out, `<link rel="stylesheet" href="../assets/style.css">`)
	})

	t.Run("verdict heading and severity badge", func(t *testing.T) {
		// Verdict is the h1; HTML-escaped on the way in.
		assert.Contains(t, out, "<h1>Release cadence has slowed materially since 2025-Q3</h1>")
		assert.Contains(t, out, "severity-high")
		assert.Contains(t, out, "high") // severity label visible
	})

	t.Run("category and signal type rendered", func(t *testing.T) {
		assert.Contains(t, out, "vitality")
		assert.Contains(t, out, "release-cadence")
	})

	t.Run("rationale paragraphs with HTML escaping", func(t *testing.T) {
		assert.Contains(t, out, "<p>The project published 14 releases in 2024 and 3 in 2025.</p>")
		assert.Contains(t, out, "&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;")
		assert.NotContains(t, out, "<script>alert")
	})

	t.Run("citations rendered as text blocks", func(t *testing.T) {
		// Line-based citation surfaces path + line range + commit + quoted.
		assert.Contains(t, out, "CHANGELOG.md")
		assert.Contains(t, out, "42")
		assert.Contains(t, out, "57")
		assert.Contains(t, out, "abc1234def5678")
		// Quoted snippet escaped (contains < and >).
		assert.Contains(t, out, "func unsafePtrCast() &lt;T&gt; { ... }")
		assert.NotContains(t, out, "<T>")
		// Scope-based citation surfaces kind + path.
		assert.Contains(t, out, "directory")
		assert.Contains(t, out, "internal/release/")
	})

	t.Run("prerequisites and remediation hints listed", func(t *testing.T) {
		assert.Contains(t, out, "upstream maintainer absent")
		assert.Contains(t, out, "no fork volunteered")
		assert.Contains(t, out, "pin to last 2024 release")
		assert.Contains(t, out, "audit transitive on bump")
	})

	t.Run("related conclusions: linked when planned, plain when not", func(t *testing.T) {
		// F001 has a page in the plan, written with the root prefix.
		assert.Contains(t, out, `href="../conclusions/out-sec-0-F001.html"`)
		// F999 is not in the plan; rendered as plain escaped text.
		idxF999 := strings.Index(out, "F999")
		require.GreaterOrEqual(t, idxF999, 0, "F999 should still appear as text")
		// And no anchor href ends in "F999.html".
		assert.NotContains(t, out, "F999.html")
	})

	t.Run("supersession chain visible", func(t *testing.T) {
		assert.Contains(t, out, "F002")
		// Supersession kind is rendered (refines).
		assert.Contains(t, out, "refines")
	})

	t.Run("answers-question surfaced", func(t *testing.T) {
		assert.Contains(t, out, "is the upstream actively maintained?")
	})

	t.Run("footer carries full IDs, attribution, and back-links", func(t *testing.T) {
		assert.Contains(t, out, "F003")               // local id
		assert.Contains(t, out, "out-sec-0001abcdef") // full output id
		assert.Contains(t, out, "signatory-security-v1")
		assert.Contains(t, out, "claude-opus-4-7")
		// Round 2 surfaced as part of attribution.
		assert.Contains(t, out, "round 2")
		// Back-links via root prefix.
		assert.Contains(t, out, `href="../index.html"`)
		assert.Contains(t, out, `href="../analysts/signatory-security-v1-r2.html"`)
		assert.Contains(t, out, "2026-05-06T13:00:00Z")
		assert.Contains(t, out, "v0.1.0-test")
	})
}

func TestRenderConclusionPage_RejectsNilConclusion(t *testing.T) {
	in := fixtureConclusionPageInput()
	in.Conclusion = nil

	var buf bytes.Buffer
	err := RenderConclusionPage(&buf, in)
	require.Error(t, err)
}
