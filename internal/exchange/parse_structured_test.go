package exchange_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

const sampleStructuredText = `# Analysis: test-target
Analyst: test-analyst
Model: claude-test
Round: 1
Target-commit: abc123

## Conclusion: F001
Severity: medium
Category: test_category
Verdict: Something is concerning here
Citation: src/main.py:47-52 "the important line"
Citation: src/helper.py:10
This is the rationale text that explains
why this conclusion matters across multiple lines.

## Conclusion: F002
Severity: positive
Category: good_thing
Design-intent: true
Verdict: A defense that's tighter than expected
No citations needed for this one. The whole codebase
was reviewed and no issues found.

## Absence: eval/exec usage
Confidence: exhaustive
Citation: src/
Grepped all files, zero hits.

## Observation: O001
Title: Interesting architectural note
Category: architecture
The codebase uses an unusual but effective pattern
for handling untrusted input.

## Round-notes
First round of analysis covering security surface.
`

func TestParseStructuredOutput_HappyPath(t *testing.T) {
	t.Parallel()
	r := strings.NewReader(sampleStructuredText)
	out, err := exchange.ParseStructuredOutput(r, "pkg:test/target")
	require.NoError(t, err)

	// Attribution.
	assert.Equal(t, "test-analyst", out.Attribution.AnalystID)
	assert.Equal(t, "claude-test", out.Attribution.Model)
	assert.Equal(t, 1, out.Attribution.Round)
	assert.Equal(t, "abc123", out.TargetCommit)
	assert.Equal(t, "pkg:test/target", out.Target)

	// Conclusions.
	require.Len(t, out.Conclusions, 2)

	f1 := out.Conclusions[0]
	assert.Equal(t, "F001", f1.ID)
	assert.Equal(t, exchange.SeverityValue("medium"), f1.Severity.Default)
	assert.Equal(t, "test_category", f1.Category)
	assert.Contains(t, f1.Verdict, "Something is concerning")
	assert.Contains(t, f1.Rationale, "rationale text")
	require.Len(t, f1.Citations, 2)

	// First citation: path:line-line "quoted"
	c1 := f1.Citations[0]
	assert.Equal(t, "src/main.py", c1.Path)
	require.NotNil(t, c1.LineStart)
	assert.Equal(t, 47, *c1.LineStart)
	require.NotNil(t, c1.LineEnd)
	assert.Equal(t, 52, *c1.LineEnd)
	require.NotNil(t, c1.Quoted)
	assert.Equal(t, "the important line", *c1.Quoted)

	// Second citation: path:line (no end, no quote)
	c2 := f1.Citations[1]
	assert.Equal(t, "src/helper.py", c2.Path)
	require.NotNil(t, c2.LineStart)
	assert.Equal(t, 10, *c2.LineStart)
	assert.Nil(t, c2.LineEnd)
	assert.Nil(t, c2.Quoted)

	// Second conclusion: positive, design-intent.
	f2 := out.Conclusions[1]
	assert.Equal(t, "F002", f2.ID)
	assert.Equal(t, exchange.SeverityValue("positive"), f2.Severity.Default)
	assert.True(t, f2.DesignIntent)
	assert.Empty(t, f2.Citations)

	// Positive absence.
	require.Len(t, out.PositiveAbsences, 1)
	a := out.PositiveAbsences[0]
	assert.Equal(t, "eval/exec usage", a.PatternChecked)
	assert.Equal(t, exchange.Confidence("exhaustive"), a.Confidence)
	assert.Contains(t, a.Description, "zero hits")
	// Citation is scope-based (directory, no line number).
	require.Len(t, a.Citations, 1)
	require.NotNil(t, a.Citations[0].Scope)
	assert.Equal(t, "file", a.Citations[0].Scope.Kind)

	// Observation.
	require.Len(t, out.Observations, 1)
	o := out.Observations[0]
	assert.Equal(t, "O001", o.ID)
	assert.Equal(t, "Interesting architectural note", o.Title)
	assert.Equal(t, "architecture", o.Category)
	assert.Contains(t, o.Body, "unusual but effective")

	// Round notes.
	assert.Contains(t, out.RoundNotes, "First round")
}

func TestParseStructuredOutput_MissingSeverity(t *testing.T) {
	t.Parallel()
	input := `## Conclusion: F001
Category: test
Verdict: something
No severity line — should error.
`
	_, err := exchange.ParseStructuredOutput(strings.NewReader(input), "pkg:test/x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Severity")
	assert.Contains(t, err.Error(), "F001")
}

func TestParseStructuredOutput_InvalidSeverity(t *testing.T) {
	t.Parallel()
	input := `## Conclusion: F001
Severity: extreme
Category: test
Verdict: something
`
	_, err := exchange.ParseStructuredOutput(strings.NewReader(input), "pkg:test/x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid severity")
}

func TestParseStructuredOutput_MissingVerdict(t *testing.T) {
	t.Parallel()
	input := `## Conclusion: F001
Severity: medium
Category: test
No verdict line.
`
	_, err := exchange.ParseStructuredOutput(strings.NewReader(input), "pkg:test/x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Verdict")
}

func TestParseStructuredOutput_TargetFromCLI(t *testing.T) {
	t.Parallel()
	// Even if the text says Target: something-else, the CLI-provided
	// target wins — agents shouldn't have to know canonical URI format.
	input := `# Analysis
Target: wrong-target

## Conclusion: F001
Severity: low
Category: test
Verdict: test verdict
`
	out, err := exchange.ParseStructuredOutput(strings.NewReader(input), "pkg:correct/target")
	require.NoError(t, err)
	assert.Equal(t, "pkg:correct/target", out.Target)
}

func TestParseStructuredOutput_EmptyInput(t *testing.T) {
	t.Parallel()
	out, err := exchange.ParseStructuredOutput(strings.NewReader(""), "pkg:test/x")
	require.NoError(t, err)
	assert.Equal(t, "pkg:test/x", out.Target)
	assert.Empty(t, out.Conclusions)
}

// TestParseStructuredOutput_Validates confirms that the output of
// ParseStructuredOutput passes the exchange.Validate method —
// proving that the parser constructs schema-valid objects, which
// is the whole point of the "code for structure" principle.
func TestParseStructuredOutput_Validates(t *testing.T) {
	t.Parallel()
	r := strings.NewReader(sampleStructuredText)
	out, err := exchange.ParseStructuredOutput(r, "pkg:test/target")
	require.NoError(t, err)

	err = out.Validate()
	assert.NoError(t, err, "parsed output must pass Validate — if this fails, the parser is constructing invalid objects")
}

// TestParseStructuredOutput_DoubleHeaderAbsence verifies that an
// agent emitting an ID line followed by a description line for
// absences is handled gracefully. The ID-only section (no fields,
// no body, no citations) is discarded; the real content lands in
// the second section.
func TestParseStructuredOutput_DoubleHeaderAbsence(t *testing.T) {
	t.Parallel()
	input := `Analyst: test-analyst
Model: test-model
Round: 1

## Absence: A001
## Absence: network-outbound connections or telemetry
Confidence: exhaustive
Citation: Cargo.toml
Searched all dependency declarations for HTTP client crates — none found.

## Absence: A002
## Absence: embedded secrets
Confidence: thoroughly_reviewed
Citation: src/
No keys, tokens, or credentials in embedded files.
`
	out, err := exchange.ParseStructuredOutput(strings.NewReader(input), "pkg:test/x")
	require.NoError(t, err)

	// Two real absences, not four.
	require.Len(t, out.PositiveAbsences, 2)
	assert.Equal(t, "network-outbound connections or telemetry", out.PositiveAbsences[0].PatternChecked)
	assert.Contains(t, out.PositiveAbsences[0].Description, "HTTP client crates")
	assert.Equal(t, "embedded secrets", out.PositiveAbsences[1].PatternChecked)
	assert.Contains(t, out.PositiveAbsences[1].Description, "keys, tokens")

	// Must also pass full validation.
	require.NoError(t, out.Validate())
}

// TestParseStructuredOutput_DoubleHeaderConclusion verifies the same
// pattern for conclusions — an agent might emit:
//
//	## Conclusion: F001
//	## Conclusion: IPC socket permissions
//	Severity: medium
//	...
func TestParseStructuredOutput_DoubleHeaderConclusion(t *testing.T) {
	t.Parallel()
	input := `Analyst: test-analyst
Model: test-model
Round: 1

## Conclusion: F001
## Conclusion: IPC socket permissions
Severity: medium
Category: ipc_auth
Verdict: Socket created with no explicit permission restriction
The IPC socket is world-connectable on multi-user systems.
`
	out, err := exchange.ParseStructuredOutput(strings.NewReader(input), "pkg:test/x")
	require.NoError(t, err)

	// One real conclusion, not two.
	require.Len(t, out.Conclusions, 1)
	assert.Equal(t, "IPC socket permissions", out.Conclusions[0].ID)
	assert.Equal(t, "Socket created with no explicit permission restriction", out.Conclusions[0].Verdict)
	require.NoError(t, out.Validate())
}

// TestParseStructuredOutput_DoubleHeaderObservation covers the
// observation variant of the empty-section pattern.
func TestParseStructuredOutput_DoubleHeaderObservation(t *testing.T) {
	t.Parallel()
	input := `Analyst: test-analyst
Model: test-model
Round: 1

## Observation: O001
## Observation: Threat model is single-user desktop
Title: Threat model is single-user desktop
Category: trust_boundary
The application runs as a terminal emulator with local access only.
`
	out, err := exchange.ParseStructuredOutput(strings.NewReader(input), "pkg:test/x")
	require.NoError(t, err)

	require.Len(t, out.Observations, 1)
	assert.Equal(t, "Threat model is single-user desktop", out.Observations[0].Title)
	require.NoError(t, out.Validate())
}

// TestParseStructuredOutput_SingleHeaderStillWorks confirms that the
// normal single-header format (no ID prefix) is unaffected by the
// empty-section guard.
func TestParseStructuredOutput_SingleHeaderStillWorks(t *testing.T) {
	t.Parallel()
	input := `Analyst: test-analyst
Model: test-model
Round: 1

## Absence: eval/exec usage
Confidence: exhaustive
Citation: src/
Grepped all files, zero hits.
`
	out, err := exchange.ParseStructuredOutput(strings.NewReader(input), "pkg:test/x")
	require.NoError(t, err)
	require.Len(t, out.PositiveAbsences, 1)
	assert.Equal(t, "eval/exec usage", out.PositiveAbsences[0].PatternChecked)
	assert.Contains(t, out.PositiveAbsences[0].Description, "zero hits")
	require.NoError(t, out.Validate())
}
