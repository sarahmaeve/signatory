package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schema helpers used across validation tests.
var (
	// strictSchema has additionalProperties:false and two declared fields.
	strictSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"target": {"type": "string"},
			"refresh": {"type": "boolean"}
		},
		"required": ["target"],
		"additionalProperties": false
	}`)

	// permissiveSchema has no additionalProperties constraint.
	permissiveSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"target": {"type": "string"}
		},
		"required": ["target"]
	}`)
)

// TestValidation_HappyPath verifies that valid input passes with no error.
func TestValidation_HappyPath(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	result := validateInput("signatory_analyze", s, json.RawMessage(`{"target":"github/foo/bar"}`))
	assert.Nil(t, result, "valid input should produce nil violation")
}

// TestValidation_UnknownField_StrictReject verifies that additionalProperties:false
// causes an unknown field to be rejected with the field named in the message.
// THIS IS THE CORE SECURITY INVARIANT: unknown fields must be rejected, not silently
// ignored, to catch typos and schema drift.
func TestValidation_UnknownField_StrictReject(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	result := validateInput("signatory_analyze", s,
		json.RawMessage(`{"target":"repo:github/foo/bar","hypothetical_flag":true}`))
	require.NotNil(t, result, "unknown field should produce a violation")
	assert.Equal(t, "error", result.Status)
	require.NotNil(t, result.Error)
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	// Message must name the offending field.
	assert.Contains(t, result.Error.Message, "hypothetical_flag")
	// Message must list valid fields.
	assert.Contains(t, result.Error.Message, "target")
}

// TestValidation_UnknownField_ValidFieldsListed verifies that the error
// details include the complete list of valid fields.
func TestValidation_UnknownField_ValidFieldsListed(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	result := validateInput("signatory_analyze", s,
		json.RawMessage(`{"target":"repo:x","bogus":1}`))
	require.NotNil(t, result)
	require.NotNil(t, result.Error)

	details, ok := result.Error.Details.(map[string]any)
	require.True(t, ok, "details should be a map")
	validFields, ok := details["valid_fields"].([]string)
	require.True(t, ok, "details.valid_fields should be a []string")
	assert.ElementsMatch(t, []string{"target", "refresh"}, validFields)
}

// TestValidation_MissingRequired verifies that a missing required field
// is rejected with a message naming the missing field.
func TestValidation_MissingRequired(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	result := validateInput("signatory_analyze", s, json.RawMessage(`{"refresh":true}`))
	require.NotNil(t, result)
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	assert.Contains(t, result.Error.Message, "target")
}

// TestValidation_TypeMismatch_String verifies that a field declared as
// "string" rejects a boolean value.
func TestValidation_TypeMismatch_String(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	// target should be string but we pass a number.
	result := validateInput("signatory_analyze", s, json.RawMessage(`{"target": 42}`))
	require.NotNil(t, result)
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	assert.Contains(t, result.Error.Message, "target")
}

// TestValidation_TypeMismatch_Boolean verifies that a field declared as
// "boolean" rejects a string value.
func TestValidation_TypeMismatch_Boolean(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	// refresh should be boolean but we pass a string.
	result := validateInput("signatory_analyze", s,
		json.RawMessage(`{"target":"repo:x","refresh":"yes"}`))
	require.NotNil(t, result)
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	assert.Contains(t, result.Error.Message, "refresh")
}

// TestValidation_PermissiveSchema_UnknownFieldAllowed verifies that when
// additionalProperties is not set to false, unknown fields pass through.
func TestValidation_PermissiveSchema_UnknownFieldAllowed(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(permissiveSchema)
	require.NoError(t, err)

	result := validateInput("my_tool", s,
		json.RawMessage(`{"target":"repo:x","extra_field":"ignored"}`))
	assert.Nil(t, result, "permissive schema should allow unknown fields")
}

// TestValidation_EmptyInput_WithRequired verifies that nil/empty input
// is treated as {} and triggers required-field validation.
func TestValidation_EmptyInput_WithRequired(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	result := validateInput("signatory_analyze", s, json.RawMessage(nil))
	require.NotNil(t, result)
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	assert.Contains(t, result.Error.Message, "target")
}

// TestValidation_NullInput_WithRequired verifies that JSON null is treated
// as {} and triggers required-field validation.
func TestValidation_NullInput_WithRequired(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	result := validateInput("signatory_analyze", s, json.RawMessage(`null`))
	require.NotNil(t, result)
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
}

// TestValidation_NotAnObject verifies that a non-object JSON value
// (e.g. an array) is rejected.
func TestValidation_NotAnObject(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	result := validateInput("signatory_analyze", s, json.RawMessage(`["array"]`))
	require.NotNil(t, result)
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
}

// TestValidation_ErrorMessageNamesToolAndField verifies that error messages
// reference both the tool name and the offending field — this is the
// "user-readable" requirement from mcp-server-architecture.md Q10.
func TestValidation_ErrorMessageNamesToolAndField(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(strictSchema)
	require.NoError(t, err)

	result := validateInput("signatory_analyze", s,
		json.RawMessage(`{"target":"x","misspelled_refresh":true}`))
	require.NotNil(t, result)
	msg := result.Error.Message
	assert.True(t, strings.Contains(msg, "signatory_analyze"), "message must name the tool")
	assert.True(t, strings.Contains(msg, "misspelled_refresh"), "message must name the field")
}

// TestParseInputSchema_MissingAdditionalProperties verifies that a schema
// without additionalProperties does not enable strictReject.
func TestParseInputSchema_MissingAdditionalProperties(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
	s, err := parseInputSchema(raw)
	require.NoError(t, err)
	assert.False(t, s.strictReject)
}

// TestParseInputSchema_AdditionalPropertiesFalse verifies that
// additionalProperties:false enables strictReject.
func TestParseInputSchema_AdditionalPropertiesFalse(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}},"additionalProperties":false}`)
	s, err := parseInputSchema(raw)
	require.NoError(t, err)
	assert.True(t, s.strictReject)
}

// TestParseInputSchema_EmptySchema verifies that an empty schema parses
// without error and enables no constraints.
func TestParseInputSchema_EmptySchema(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(json.RawMessage(nil))
	require.NoError(t, err)
	assert.False(t, s.strictReject)
	assert.Empty(t, s.required)
}

// -------------------------------------------------------------------
// H1: integer vs number discrimination and minimum/maximum enforcement.
//
// Before H1, `{"limit": 1.5}` passed a schema declaring {"type":
// "integer", "minimum": 0}. `{"limit": -1}` did too. These tests lock
// in the new behavior so a future refactor can't silently regress it.

// integerLimitSchema mirrors the production schemas used by
// show_analyses / show_findings / show_methodology: a single optional
// integer field with a minimum of 0.
var integerLimitSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"limit": {"type": "integer", "minimum": 0}
	},
	"additionalProperties": false
}`)

// numericRangeSchema exercises maximum enforcement (which no production
// schema declares yet) alongside minimum. Both bounds and both numeric
// types get coverage in one schema to keep the test surface tight.
var numericRangeSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"percent":  {"type": "number",  "minimum": 0, "maximum": 100},
		"attempts": {"type": "integer", "minimum": 1, "maximum": 5}
	},
	"additionalProperties": false
}`)

// TestValidation_Integer_RejectsFloat is the regression test for the
// reviewer's H1 finding: 1.5 against {"type":"integer"} used to pass.
func TestValidation_Integer_RejectsFloat(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(integerLimitSchema)
	require.NoError(t, err)

	result := validateInput("signatory_show_findings", s,
		json.RawMessage(`{"limit": 1.5}`))
	require.NotNil(t, result, "1.5 must not pass an integer schema")
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	assert.Contains(t, result.Error.Message, "integer")
	assert.Contains(t, result.Error.Message, "limit")
}

// TestValidation_Integer_AcceptsInteger proves the reject-float change
// didn't regress legitimate integer values.
func TestValidation_Integer_AcceptsInteger(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(integerLimitSchema)
	require.NoError(t, err)

	result := validateInput("signatory_show_findings", s,
		json.RawMessage(`{"limit": 42}`))
	assert.Nil(t, result, "an in-range integer must pass")
}

// TestValidation_Number_AcceptsFloat verifies that "number" type still
// accepts fractional values — the discrimination must be one-way
// (integer strict, number permissive), not both.
func TestValidation_Number_AcceptsFloat(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(numericRangeSchema)
	require.NoError(t, err)

	result := validateInput("tool", s, json.RawMessage(`{"percent": 12.5}`))
	assert.Nil(t, result, "a fractional value must pass a number schema")
}

// TestValidation_Number_AcceptsInteger verifies that integer values pass
// "number" schemas — number is strictly weaker than integer.
func TestValidation_Number_AcceptsInteger(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(numericRangeSchema)
	require.NoError(t, err)

	result := validateInput("tool", s, json.RawMessage(`{"percent": 12}`))
	assert.Nil(t, result, "an integer value must pass a number schema")
}

// TestValidation_Minimum_RejectsBelowBound is the second half of H1:
// minimum was parsed but unenforced. A negative limit against
// minimum:0 must now fail.
func TestValidation_Minimum_RejectsBelowBound(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(integerLimitSchema)
	require.NoError(t, err)

	result := validateInput("signatory_show_findings", s,
		json.RawMessage(`{"limit": -1}`))
	require.NotNil(t, result, "-1 must not pass a minimum:0 schema")
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	assert.Contains(t, result.Error.Message, "minimum")
	assert.Contains(t, result.Error.Message, "limit")
	// Bound formatting: integer bounds must not show "0.000000".
	assert.Contains(t, result.Error.Message, " 0")
	assert.NotContains(t, result.Error.Message, "0.0")
}

// TestValidation_Minimum_AcceptsAtBound verifies the bound is inclusive
// per JSON Schema draft 2020-12 semantics. minimum:0 must accept 0.
func TestValidation_Minimum_AcceptsAtBound(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(integerLimitSchema)
	require.NoError(t, err)

	result := validateInput("tool", s, json.RawMessage(`{"limit": 0}`))
	assert.Nil(t, result, "value equal to minimum must be accepted")
}

// TestValidation_Maximum_RejectsAboveBound covers the symmetric upper
// bound. No production schema declares maximum today, but the path
// is there — and if we don't exercise it, a future schema author
// declaring maximum gets silent permissiveness.
func TestValidation_Maximum_RejectsAboveBound(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(numericRangeSchema)
	require.NoError(t, err)

	result := validateInput("tool", s, json.RawMessage(`{"attempts": 10}`))
	require.NotNil(t, result, "10 must not pass a maximum:5 schema")
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	assert.Contains(t, result.Error.Message, "maximum")
	assert.Contains(t, result.Error.Message, " 5")
}

// TestValidation_Maximum_AcceptsAtBound — inclusive upper bound.
func TestValidation_Maximum_AcceptsAtBound(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(numericRangeSchema)
	require.NoError(t, err)

	result := validateInput("tool", s, json.RawMessage(`{"attempts": 5}`))
	assert.Nil(t, result, "value equal to maximum must be accepted")
}

// TestValidation_TypeErrorTakesPrecedenceOverBounds verifies the
// order-of-operations property: a value that's both the wrong type
// AND out of bounds reports the type error (the more fundamental
// problem), not the bounds error. This is a readability property,
// not a correctness one — but it matters for agent-facing messages.
func TestValidation_TypeErrorTakesPrecedenceOverBounds(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(integerLimitSchema)
	require.NoError(t, err)

	// -1.5 is both non-integer AND below minimum. The reported error
	// must be the integer violation.
	result := validateInput("tool", s, json.RawMessage(`{"limit": -1.5}`))
	require.NotNil(t, result)
	assert.Contains(t, result.Error.Message, "integer",
		"type error must take precedence over bounds error")
	assert.NotContains(t, result.Error.Message, "minimum",
		"bounds error must not be reported when the type is wrong")
}

// TestValidation_BoundsIgnoredOnNonNumericTypes verifies that a
// minimum declared on a string type (a schema bug, but possible)
// does not fire. Silently ignoring is consistent with the rest of
// parseInputSchema's permissiveness; a stricter parser belongs in M2.
func TestValidation_BoundsIgnoredOnNonNumericTypes(t *testing.T) {
	t.Parallel()
	stringWithMin := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "minimum": 10}
		},
		"additionalProperties": false
	}`)
	s, err := parseInputSchema(stringWithMin)
	require.NoError(t, err)

	// A single-character string would violate minimum:10 if we
	// (incorrectly) interpreted it as minLength. We should accept.
	result := validateInput("tool", s, json.RawMessage(`{"name": "x"}`))
	assert.Nil(t, result, "minimum on a string type must be silently ignored, not misapplied")
}

// TestValidation_MinimumZero_HasMinimumFlagDistinguished is the
// "declared vs. zero-default" distinction test: minimum:0 in the
// schema must be enforced, not mistaken for "no minimum declared."
// This catches a subtle bug where propertySchema.minimum==0 is
// ambiguous between "absent" and "present and equals zero"; the
// hasMinimum flag is the disambiguator.
func TestValidation_MinimumZero_HasMinimumFlagDistinguished(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(integerLimitSchema)
	require.NoError(t, err)

	// The real proof: -1 must be rejected. If hasMinimum were
	// accidentally false, -1 would pass.
	result := validateInput("tool", s, json.RawMessage(`{"limit": -1}`))
	require.NotNil(t, result, "minimum:0 must be distinguishable from 'no minimum'")
	assert.Contains(t, result.Error.Message, "minimum")
}

// TestValidation_Number_RejectsOverflow verifies that a number that
// overflows float64 (e.g. 1e400) is rejected when bounds are declared,
// rather than silently bypassing the bounds check. This exercises
// the "unverifiable value must not slip through" branch in
// checkBounds — without it, a buggy or malicious client could stuff
// a number we can't meaningfully compare to any bound and have it
// accepted as "within range."
func TestValidation_Number_RejectsOverflow(t *testing.T) {
	t.Parallel()
	s, err := parseInputSchema(numericRangeSchema)
	require.NoError(t, err)

	// 1e400 is far larger than max float64 (~1.8e308). strconv.ParseFloat
	// reports ErrRange. The bounds check must reject rather than accept.
	result := validateInput("tool", s,
		json.RawMessage(`{"percent": 1e400}`))
	require.NotNil(t, result, "unrepresentable number must be rejected when bounds are declared")
	assert.Equal(t, CodeSchemaViolation, result.Error.Code)
	assert.Contains(t, result.Error.Message, "representable")
}

// TestDescribeJSON_LabelCoverage locks in the M5 change: the default
// fallback is "invalid", not "number". A leading byte that isn't a
// legal JSON start (say '?' from truncated input) must not be
// misclassified, since the label goes into user-facing error messages.
//
// Each row is one leading byte and the label describeJSON should
// emit. The "number" row covers both sign and digit paths explicitly
// so the narrowing doesn't accidentally drop one.
func TestDescribeJSON_LabelCoverage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", "null"},
		{"string", `"hello"`, "string"},
		{"object", `{}`, "object"},
		{"array", `[]`, "array"},
		{"boolean true", `true`, "boolean"},
		{"boolean false", `false`, "boolean"},
		{"null literal", `null`, "null"},
		{"positive digit", `42`, "number"},
		{"leading zero", `0.5`, "number"},
		{"negative", `-1`, "number"},
		{"stray question mark", `?garbage`, "invalid"},
		{"stray comma", `,`, "invalid"},
		{"whitespace only", ` `, "invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := describeJSON(json.RawMessage(tc.raw))
			assert.Equal(t, tc.want, got,
				"describeJSON(%q) must label the leading byte honestly, not fall back to 'number'", tc.raw)
		})
	}
}
