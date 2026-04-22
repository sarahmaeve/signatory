// Package mcp: strict JSON-Schema validation for tool inputs.
//
// We implement a narrow subset of JSON Schema 2020-12 sufficient for
// all signatory tool schemas: type:object, properties (with type),
// required, additionalProperties:false. No $ref, no allOf, no
// recursive schemas — signatory tools don't need them and a full
// implementation would add unjustified complexity.
//
// Design rationale: the spec (mcp-server-architecture.md Q10) mandates
// strict-reject of unknown fields with a message that names the
// offending field and lists valid fields. This is the entire motivation
// for the validator — not general-purpose schema validation.
package mcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// inputSchema is the parsed form of a tool's JSON Schema. We only
// parse what we need to validate tool inputs; extra schema keywords
// are ignored.
type inputSchema struct {
	// properties maps field name → property constraints. A nil entry
	// means "property is declared but no constraints we parse" — we
	// still treat it as a known field for additionalProperties.
	properties map[string]*propertySchema
	required   map[string]bool
	// strictReject is true when additionalProperties:false was set.
	strictReject bool
}

// propertySchema carries the subset of JSON Schema constraints we
// actually enforce for a single property: type, plus optional numeric
// bounds. Expanding this struct is the expected path for future schema
// features (minLength, pattern, enum) — add the parsed form here and
// a matching branch in checkType/checkBounds.
type propertySchema struct {
	// typ is the JSON Schema type: "string", "boolean", "number",
	// "integer", "object", "array", or "" if unspecified.
	typ string
	// minimum / maximum are the bounds for numeric types. They are
	// silently ignored on non-numeric types (consistent with the rest
	// of this parser's permissiveness); a schema that declares
	// minimum on a string is a schema bug we don't currently catch.
	// The hasX flags distinguish "not declared" from "declared as 0."
	minimum    float64
	hasMinimum bool
	maximum    float64
	hasMaximum bool
}

// parseInputSchema parses the JSON Schema raw bytes into our narrow
// schema representation. Unknown top-level schema keywords are silently
// ignored; the caller validates the full schema at registration time by
// checking that the tool author set additionalProperties:false.
func parseInputSchema(raw json.RawMessage) (*inputSchema, error) {
	if len(raw) == 0 {
		return &inputSchema{}, nil
	}

	// Decode to a generic map so we can inspect any key.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("inputSchema must be a JSON object: %w", err)
	}

	schema := &inputSchema{
		properties: make(map[string]*propertySchema),
		required:   make(map[string]bool),
	}

	// additionalProperties: false
	if ap, ok := top["additionalProperties"]; ok {
		var v bool
		if err := json.Unmarshal(ap, &v); err == nil {
			schema.strictReject = !v // false → reject additionals
		}
		// additionalProperties: {} (schema) is ignored for now; we
		// treat any non-false value as "allow additional", which is
		// safe-default for forward compat.
	}

	// properties: { fieldName: { "type": "string", "minimum": 0, … }, … }
	if props, ok := top["properties"]; ok {
		var propMap map[string]json.RawMessage
		if err := json.Unmarshal(props, &propMap); err != nil {
			return nil, fmt.Errorf("properties must be an object: %w", err)
		}
		for name, def := range propMap {
			schema.properties[name] = parseProperty(def)
		}
	}

	// required: ["field1", "field2"]
	if req, ok := top["required"]; ok {
		var reqList []string
		if err := json.Unmarshal(req, &reqList); err != nil {
			return nil, fmt.Errorf("required must be an array: %w", err)
		}
		for _, f := range reqList {
			schema.required[f] = true
		}
	}

	return schema, nil
}

// parseProperty extracts the constraints we recognize from one
// property definition. It never returns nil — an unparseable or
// feature-thin property still round-trips as a zero-valued
// *propertySchema so additionalProperties checks know the field is
// declared. Constraints we don't recognize are silently ignored.
func parseProperty(def json.RawMessage) *propertySchema {
	var raw struct {
		Type    string       `json:"type"`
		Minimum *json.Number `json:"minimum"`
		Maximum *json.Number `json:"maximum"`
	}
	// If the def is not a JSON object (e.g., the author wrote `true`
	// instead of `{"type":"string"}`) we return an empty spec rather
	// than fail — the type check will no-op and additionalProperties
	// still works. A stricter parseInputSchema would reject this; see
	// the M2 pending fix.
	if err := json.Unmarshal(def, &raw); err != nil {
		return &propertySchema{}
	}
	p := &propertySchema{typ: raw.Type}
	if raw.Minimum != nil {
		if f, err := raw.Minimum.Float64(); err == nil {
			p.minimum = f
			p.hasMinimum = true
		}
	}
	if raw.Maximum != nil {
		if f, err := raw.Maximum.Float64(); err == nil {
			p.maximum = f
			p.hasMaximum = true
		}
	}
	return p
}

// validFields returns a sorted slice of property names for use in
// error messages. Sorted for deterministic output.
func (s *inputSchema) validFields() []string {
	names := make([]string, 0, len(s.properties))
	for name := range s.properties {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// validateInput checks that input conforms to the schema, returning a
// descriptive *Response with CodeSchemaViolation if not.
// Returns nil if input is valid.
//
// Validation rules:
//  1. Input must be a JSON object (or null/absent → treated as {}).
//  2. If additionalProperties:false, any field not in properties is rejected
//     with a message naming the exact field and listing valid fields.
//  3. Every required field must be present.
//  4. Each present field's value must match the declared type (string,
//     bool, number/integer, object, array). Type mismatch is an error.
func validateInput(toolName string, schema *inputSchema, raw json.RawMessage) *Response {
	// Null or missing input → treat as empty object.
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}

	// Decode to map for field-level inspection.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Err(CodeSchemaViolation,
			fmt.Sprintf("tool input for %s must be a JSON object: %s", toolName, err.Error()),
			nil)
	}

	validFieldsList := schema.validFields()

	// 1. Check for additional (unknown) properties.
	if schema.strictReject {
		for name := range fields {
			if _, ok := schema.properties[name]; !ok {
				return Err(CodeSchemaViolation,
					fmt.Sprintf("unknown field %q in tool input for %s. Valid fields: [%s]",
						name, toolName, strings.Join(validFieldsList, ", ")),
					map[string]any{
						"field":        name,
						"valid_fields": validFieldsList,
					})
			}
		}
	}

	// 2. Check required fields are present.
	for field := range schema.required {
		if _, ok := fields[field]; !ok {
			return Err(CodeSchemaViolation,
				fmt.Sprintf("required field %q missing in tool input for %s", field, toolName),
				map[string]any{
					"field":        field,
					"valid_fields": validFieldsList,
				})
		}
	}

	// 3. Type-check and bounds-check present fields with a declared
	//    type. Type is checked first: "limit":1.5 against an integer
	//    schema reports "not an integer" rather than a minimum/maximum
	//    violation — the type error is the more fundamental one.
	for name, raw := range fields {
		prop, ok := schema.properties[name]
		if !ok || prop == nil || prop.typ == "" {
			continue // unknown type or not in schema — skip both checks
		}
		if err := checkType(prop.typ, raw); err != nil {
			return Err(CodeSchemaViolation,
				fmt.Sprintf("field %q in tool input for %s: %s", name, toolName, err.Error()),
				map[string]any{
					"field": name,
					"type":  prop.typ,
				})
		}
		if err := checkBounds(prop, raw); err != nil {
			return Err(CodeSchemaViolation,
				fmt.Sprintf("field %q in tool input for %s: %s", name, toolName, err.Error()),
				map[string]any{
					"field": name,
					"type":  prop.typ,
				})
		}
	}

	return nil // valid
}

// checkType validates that raw JSON matches the expected JSON Schema
// type. The narrowed subset covers: string, boolean, number, integer,
// object, array. "integer" is strictly stricter than "number" —
// 1.5 passes "number" but fails "integer", which is the spec's
// behavior and what our schemas depend on.
func checkType(declaredType string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	switch declaredType {
	case "string":
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("expected string, got %s", describeJSON(raw))
		}
	case "boolean":
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("expected boolean, got %s", describeJSON(raw))
		}
	case "number":
		var v json.Number
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("expected number, got %s", describeJSON(raw))
		}
	case "integer":
		// json.Number accepts both integers and floats — we have to
		// round-trip through Int64 to reject 1.5 against an integer
		// schema. This is the H1 regression path; without it, our
		// three "limit" fields silently accept fractional values and
		// the handler gets a confused decode error later.
		var v json.Number
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("expected integer, got %s", describeJSON(raw))
		}
		if _, err := v.Int64(); err != nil {
			return fmt.Errorf("expected integer, got %s (%s)", v.String(), describeJSON(raw))
		}
	case "object":
		var v map[string]json.RawMessage
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("expected object, got %s", describeJSON(raw))
		}
	case "array":
		var v []json.RawMessage
		if err := json.Unmarshal(raw, &v); err != nil {
			return fmt.Errorf("expected array, got %s", describeJSON(raw))
		}
	}
	return nil
}

// checkBounds enforces minimum/maximum on numeric types after the
// type check has passed. Silently no-ops for non-numeric types and
// for properties that declared no bounds — "declared but unenforced"
// was the bug H1 fixed, so the branch matters even when it looks
// empty. Any numeric value that can't be represented as float64
// (e.g., 1e400 overflowing to infinity) is rejected rather than
// silently accepted: an unverifiable value must not bypass bounds.
func checkBounds(prop *propertySchema, raw json.RawMessage) error {
	if !prop.hasMinimum && !prop.hasMaximum {
		return nil
	}
	if prop.typ != "number" && prop.typ != "integer" {
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		// checkType already ran and passed, so getting here means a
		// caller reordered the pipeline and skipped the type check.
		// Surface the inconsistency rather than silently accept.
		return fmt.Errorf("internal error: bounds check on non-numeric JSON value %s", describeJSON(raw))
	}
	v, err := n.Float64()
	if err != nil {
		// strconv.ErrRange or similar — the number overflows float64.
		// We can't compare it to a bound, so we must reject.
		return fmt.Errorf("value %s is outside the representable numeric range", n.String())
	}
	if prop.hasMinimum && v < prop.minimum {
		return fmt.Errorf("value %s is below minimum %s",
			n.String(), formatBound(prop.minimum))
	}
	if prop.hasMaximum && v > prop.maximum {
		return fmt.Errorf("value %s is above maximum %s",
			n.String(), formatBound(prop.maximum))
	}
	return nil
}

// formatBound renders a numeric bound without trailing ".0" when the
// bound is a whole number — so minimum:0 on an integer field reports
// "below minimum 0" rather than "below minimum 0.000000". Small UX
// detail; the error message is what the LLM client sees.
func formatBound(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%g", f)
}

// describeJSON returns a human-readable type label for the leading byte
// of a raw JSON value, for use in error messages. The label is a
// first-byte approximation — "looks like a number" means "starts with
// '-' or a digit," not "parses as a valid JSON number." Good enough
// for error messages the LLM client reads; not suitable for
// validation decisions (those use json.Unmarshal on the full value).
func describeJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}
	switch raw[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return "number"
	default:
		// Unknown leading byte — malformed or truncated input that
		// the caller presumably detected separately. Returning
		// "invalid" rather than misclassifying it as a number keeps
		// the message honest.
		return "invalid"
	}
}
