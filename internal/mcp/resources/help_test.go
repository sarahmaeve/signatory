package resources_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp/resources"
)

// URIPattern() is covered by the registration contract test in
// cmd/signatory (TestMCPRegistration_Contract) — the URI is verified
// as a live member of the resources/list output, which is a stronger
// contract than this isolated tautology was.

// TestHelpResource_HappyPath verifies that Read returns a successful
// response with a non-empty content string. We deliberately don't
// snapshot the whole text — help content will evolve — but we do
// check for markers that prove the response is the help text and not
// some stale or empty fallback.
func TestHelpResource_HappyPath(t *testing.T) {
	t.Parallel()
	r := &resources.HelpResource{}

	resp := r.Read(t.Context(), "signatory://help")
	require.Equal(t, "ok", resp.Status)
	require.Nil(t, resp.Error)

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Content string `json:"content"`
	}
	require.NoError(t, unmarshal(raw, &decoded))
	assert.NotEmpty(t, decoded.Content, "help content must not be empty")

	// Sanity anchors — phrases that the help text must contain for it
	// to be recognisably the orientation guide. If these assertions
	// fail because we legitimately renamed a concept, update the
	// anchors; don't just delete them.
	for _, anchor := range []string{
		"signatory",                  // the project name appears
		"supply-chain",               // scope statement
		"signatory_analyze",          // question→tool map includes analyze
		"signatory_show_conclusions", // and conclusions
		"signatory://posture",        // and posture resource
		"NotFound",                   // failure-mode explanation
	} {
		assert.True(t,
			strings.Contains(decoded.Content, anchor),
			"help text must contain anchor %q so the orientation role stays recognisable", anchor)
	}
}

// TestHelpResource_Description verifies the resources/list
// description begins with the "READ THIS FIRST" affordance. The
// phrasing matters: a scanning LLM sees the Description before the
// content, and the explicit affordance is what makes discoverability
// work. A description that drifts into pure reference phrasing
// silently regresses the dogfood finding that motivated this
// resource.
func TestHelpResource_Description(t *testing.T) {
	t.Parallel()
	r := &resources.HelpResource{}
	desc := r.Description()
	assert.Contains(t, desc, "READ THIS",
		"help description must carry an explicit affordance, not just be reference prose")
}

// TestHelpResource_URIIgnored verifies the URI argument has no effect on
// the response payload — HelpResource is static. Two Reads with
// deliberately different query strings must produce byte-identical Data.
// A regression that started routing content by query param would fail
// the equality check; the previous single-call form (status == "ok")
// would not have caught that.
func TestHelpResource_URIIgnored(t *testing.T) {
	t.Parallel()
	r := &resources.HelpResource{}

	respA := r.Read(t.Context(), "signatory://help")
	respB := r.Read(t.Context(), "signatory://help?anything=junk&other=value")

	require.Equal(t, "ok", respA.Status)
	require.Equal(t, "ok", respB.Status)

	// Byte-level comparison of the serialized Data payload. Comparing
	// the `any`-typed Data directly with reflect.DeepEqual would also
	// work, but marshalling makes the failure message human-readable
	// if the invariant breaks.
	assert.Equal(t, mustMarshal(t, respA.Data), mustMarshal(t, respB.Data),
		"HelpResource.Read must produce identical Data regardless of URI query params")
}
