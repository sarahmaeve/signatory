package exchange

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarkdown_RoundtripFromFixture(t *testing.T) {
	// Decode the JSON fixture, marshal to markdown, parse markdown,
	// confirm we get an equivalent struct back. The JSON-format
	// fixture is the canonical reference — markdown is the
	// human-readable persistence format derived from it.
	first := decodeFixture(t)

	md, err := first.MarshalMarkdown()
	require.NoError(t, err, "marshal markdown")

	// Sanity checks on the output shape.
	assert.True(t, strings.HasPrefix(string(md), "---\n"),
		"output must start with frontmatter delimiter")
	assert.Contains(t, string(md), "\n---\n",
		"output must have closing frontmatter delimiter on its own line")

	second, err := UnmarshalMarkdown(md)
	require.NoError(t, err, "unmarshal markdown")

	assert.True(t, reflect.DeepEqual(*first, *second),
		"round-trip must be DeepEqual")
}

func TestMarkdown_RoundtripIdempotent(t *testing.T) {
	// Two passes through marshal/unmarshal should produce identical
	// bytes — the format's canonical form should be a fixed point.
	first := decodeFixture(t)

	md1, err := first.MarshalMarkdown()
	require.NoError(t, err)

	out, err := UnmarshalMarkdown(md1)
	require.NoError(t, err)

	md2, err := out.MarshalMarkdown()
	require.NoError(t, err)

	assert.Equal(t, string(md1), string(md2),
		"second marshal should produce identical bytes to first")
}

func TestMarkdown_BodyIsRoundNotes(t *testing.T) {
	// Verify the body of the markdown contains the RoundNotes
	// string, so a human reading the file on GitHub sees prose
	// commentary rendered as markdown.
	first := decodeFixture(t)
	require.NotEmpty(t, first.RoundNotes,
		"fixture should have round_notes set; if it doesn't, this test needs adjusting")

	md, err := first.MarshalMarkdown()
	require.NoError(t, err)

	// The body should contain a recognizable substring of round_notes.
	// We pick a phrase that's distinctive enough to identify and short
	// enough not to fail on minor whitespace edits.
	snippet := first.RoundNotes[:50]
	assert.Contains(t, string(md), snippet,
		"body must contain round_notes prose")

	// And the frontmatter should NOT contain the round_notes content.
	// (Split at the closing delimiter and check.)
	parts := strings.SplitN(string(md), "\n---\n", 2)
	require.Len(t, parts, 2, "should have closing frontmatter delim")
	frontmatter := parts[0]
	assert.NotContains(t, frontmatter, snippet,
		"frontmatter must NOT contain round_notes content")
}

func TestMarkdown_NoRoundNotes_OmitsBody(t *testing.T) {
	// If RoundNotes is empty, the marshalled output should still
	// be valid (frontmatter + closing delim) but the body section
	// should be absent.
	o := &AnalystOutput{
		Attribution: AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-01-01T00:00:00Z",
		},
		Target:   "pkg:test/x",
		Findings: []Finding{},
	}

	md, err := o.MarshalMarkdown()
	require.NoError(t, err)

	// Should end with closing delimiter + newline, no body.
	assert.True(t, strings.HasSuffix(string(md), "\n---\n"),
		"output without round_notes should end at closing delim; got: %q",
		string(md[len(md)-min(20, len(md)):]))

	// Re-parse should give back an output with empty RoundNotes.
	parsed, err := UnmarshalMarkdown(md)
	require.NoError(t, err)
	assert.Empty(t, parsed.RoundNotes)
}

func TestMarkdown_MultilineRoundNotes(t *testing.T) {
	// Multi-paragraph markdown body with code fences, headings, etc.
	// Should round-trip exactly (modulo the trailing-newline
	// normalization in MarshalMarkdown).
	body := `# Top heading

A paragraph with **bold** and *italic*.

- A list item
- Another item

` + "```rust\nfn example() -> i32 { 42 }\n```\n" + `

A trailing paragraph.`

	o := &AnalystOutput{
		Attribution: AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-01-01T00:00:00Z",
		},
		Target:     "pkg:test/x",
		Findings:   []Finding{},
		RoundNotes: body,
	}

	md, err := o.MarshalMarkdown()
	require.NoError(t, err)

	parsed, err := UnmarshalMarkdown(md)
	require.NoError(t, err)
	assert.Equal(t, body, parsed.RoundNotes)
}

func TestMarkdown_MissingOpenDelimiter(t *testing.T) {
	_, err := UnmarshalMarkdown([]byte("no frontmatter here\n\nsome body\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frontmatter delimiter")
}

func TestMarkdown_MissingCloseDelimiter(t *testing.T) {
	input := []byte("---\ntarget: pkg:test/x\n\nbody but no close\n")
	_, err := UnmarshalMarkdown(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing closing")
}

func TestMarkdown_MalformedYAML(t *testing.T) {
	input := []byte("---\nthis is: not: valid: yaml: at all\n  - mixed\n---\n")
	_, err := UnmarshalMarkdown(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frontmatter")
}

func TestMarkdown_RoundNotesInFrontmatter_Rejected(t *testing.T) {
	// Defensive check: if someone hand-edits a file and puts
	// round_notes in the frontmatter (where it doesn't belong), the
	// parser should reject the document rather than silently
	// concatenating with the body.
	input := []byte(`---
attribution:
  analyst_id: x
  model: y
  invoked_at: "2026-01-01T00:00:00Z"
target: "pkg:test/x"
findings: []
round_notes: |
  this should not be here
---

actual body content
`)
	_, err := UnmarshalMarkdown(input)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "round_notes appears in frontmatter")
}

func TestMarkdown_BOMHandling(t *testing.T) {
	// Files edited on Windows sometimes have a UTF-8 BOM. The
	// parser should tolerate it.
	input := []byte{0xEF, 0xBB, 0xBF}
	input = append(input, []byte("---\n"+
		"attribution:\n  analyst_id: x\n  model: y\n  invoked_at: \"2026-01-01T00:00:00Z\"\n"+
		"target: \"pkg:test/x\"\nfindings: []\n"+
		"---\n")...)
	out, err := UnmarshalMarkdown(input)
	require.NoError(t, err)
	assert.Equal(t, "pkg:test/x", out.Target)
}

func TestMarkdown_CRLFHandling(t *testing.T) {
	// CRLF line endings should parse cleanly. yaml.v3 handles them
	// natively but our delimiter detection normalizes first to be
	// sure.
	input := []byte("---\r\n" +
		"attribution:\r\n  analyst_id: x\r\n  model: y\r\n  invoked_at: \"2026-01-01T00:00:00Z\"\r\n" +
		"target: \"pkg:test/x\"\r\nfindings: []\r\n" +
		"---\r\n" +
		"\r\nbody line\r\n")
	out, err := UnmarshalMarkdown(input)
	require.NoError(t, err)
	assert.Equal(t, "pkg:test/x", out.Target)
	assert.Equal(t, "body line", out.RoundNotes)
}

func TestMarkdown_NilOutput_Errors(t *testing.T) {
	var o *AnalystOutput
	_, err := o.MarshalMarkdown()
	require.Error(t, err)
}

// TestMarkdown_TagsConsistent verifies the JSON and YAML tags on
// every struct field match. This is a guardrail: if someone adds a
// new field with `json:"foo"` but forgets `yaml:"foo"`, the field
// would silently get a different name in YAML serialization, breaking
// round-trip with JSON-emitted fixtures.
//
// The reflection walk covers every exported field on every struct
// type used in AnalystOutput.
func TestMarkdown_TagsConsistent(t *testing.T) {
	types := []any{
		AnalystOutput{},
		AgentAttribution{},
		Finding{},
		Severity{},
		ContextualSeverity{},
		ContextSpec{},
		Citation{},
		ScopeRef{},
		PositiveAbsence{},
		Observation{},
		MethodologyCatalog{},
		MethodologyPattern{},
		CollectorHint{},
		Supersession{},
	}
	for _, sample := range types {
		typ := reflect.TypeOf(sample)
		t.Run(typ.Name(), func(t *testing.T) {
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				jsonTag := field.Tag.Get("json")
				yamlTag := field.Tag.Get("yaml")
				assert.NotEmpty(t, jsonTag,
					"%s.%s: missing json tag", typ.Name(), field.Name)
				assert.NotEmpty(t, yamlTag,
					"%s.%s: missing yaml tag", typ.Name(), field.Name)
				assert.Equal(t, jsonTag, yamlTag,
					"%s.%s: json and yaml tags differ (json=%q, yaml=%q)",
					typ.Name(), field.Name, jsonTag, yamlTag)
			}
		})
	}
}
