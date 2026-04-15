package resources_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp/resources"
)

// TestHelpResource_URIPattern locks in the literal URI. Changing it
// breaks every client that has it in their workflow — treat this
// string like a schema commitment.
func TestHelpResource_URIPattern(t *testing.T) {
	t.Parallel()
	r := &resources.HelpResource{}
	assert.Equal(t, "signatory://help", r.URIPattern())
}

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
		"signatory",               // the project name appears
		"supply-chain",            // scope statement
		"signatory_analyze",       // question→tool map includes analyze
		"signatory_show_findings", // and findings
		"signatory://posture",     // and posture resource
		"NotFound",                // failure-mode explanation
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

// TestHelpResource_URIIgnored verifies the literal uri argument is
// ignored — HelpResource is a static resource, so the URI passed in
// should have no effect on the response.
func TestHelpResource_URIIgnored(t *testing.T) {
	t.Parallel()
	r := &resources.HelpResource{}
	resp := r.Read(t.Context(), "signatory://help?anything=junk")
	assert.Equal(t, "ok", resp.Status)
}
