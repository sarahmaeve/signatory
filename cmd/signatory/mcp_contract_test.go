package main

// Contract test for the MCP server's registered tool/resource surface.
//
// What this replaces: nine per-handler stub tests (TestXxxTool_Name and
// TestXxxResource_URIPattern) that each asserted a single string
// literal against itself. Those tests were tautological — the test
// body duplicated the implementation body, so a rename touched both in
// lockstep and the test could never fail for any bug an engineer might
// actually introduce.
//
// What this replaces them with: one test that wires the production
// registerMCPHandlers (the same code path Run() uses in production),
// then asserts the registry observed through mcp.Server's accessors
// matches a pinned expected set. Adding or removing a handler requires
// updating the expected list here — that is the contract.
//
// Why it's load-bearing:
//   - mcp.Server.Register() panics on an InputSchema that isn't valid
//     JSON or doesn't set additionalProperties:false. Reaching the
//     assertion at all means every registered tool's schema is both
//     well-formed JSON AND strict-reject — which subsumes the two
//     TestXxxTool_InputSchemaValid checks that were also deleted.
//   - Every URI pattern and tool name is verified as a live member of
//     the production registry, not as an isolated literal.
//   - New handler added without doc update → test fails.
//   - Handler silently removed from production wiring → test fails.
//
// Note on staleness risk: yes, the expected slices below must be
// updated when the registered surface changes. That is the intended
// load-bearing property — the update forces a conscious edit in the
// same commit as the capability change, which is exactly what a
// capability-surface guard should do.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// expectedMCPToolNames is the pinned set of tool names the Phase 1 MCP
// surface exposes. Adding a tool here AND in registerMCPHandlers is the
// only way to pass this test — a mismatch in either direction fails.
var expectedMCPToolNames = []string{
	"signatory_analyze",
	"signatory_detail",
	"signatory_ingest_analysis",
	"signatory_show_analyses",
	"signatory_show_conclusions",
	"signatory_show_methodology",
	"signatory_show_synthesis",
	"signatory_signals",
	"signatory_summary",
	"signatory_survey",
}

// expectedMCPResourcePatterns is the pinned set of URI patterns.
// Static URIs are literal; templated URIs carry their RFC 6570
// fragment (e.g. "{?target}") so the pattern is exactly what
// Register sees and exactly what resources/list returns.
var expectedMCPResourcePatterns = []string{
	"signatory://analyses",
	"signatory://burns",
	"signatory://config",
	"signatory://help",
	"signatory://posture",
	"signatory://signal-types",
	"signatory://unexamined",
}

// TestMCPRegistration_Contract wires the production registerMCPHandlers
// onto a real mcp.Server and confirms the tool name set and resource
// URI set match expectedMCPToolNames / expectedMCPResourcePatterns.
//
// The Server's Register method panics on a malformed InputSchema, so
// reaching the assertions proves every tool's schema is valid JSON
// with additionalProperties:false — the InputSchema-validity check
// the old per-tool tests performed is subsumed here.
func TestMCPRegistration_Contract(t *testing.T) {
	t.Parallel()

	// A real store is required because several handlers (PostureResource,
	// UnexaminedResource, AnalysesResource, the storeful tools) need a
	// non-nil *store.SQLite threaded through registerMCPHandlers. The
	// registry accessors don't exercise handler Read/Handle paths —
	// those are covered by the per-handler test files — so an empty
	// on-disk SQLite is sufficient.
	dbPath := t.TempDir() + "/contract.db"
	s, err := store.OpenSQLite(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	srv := mcp.NewServer("0.0.0-contract-test")
	// registerMCPHandlers panics if any tool's InputSchema is invalid,
	// so reaching the line after is itself a schema-validity check.
	registerMCPHandlers(srv, s, dbPath, "0.0.0-contract-test")

	assert.Equal(t, expectedMCPToolNames, srv.RegisteredToolNames(),
		"MCP tool surface drifted. Update registerMCPHandlers in mcp.go "+
			"AND expectedMCPToolNames in this file. The mismatch exists "+
			"to force a conscious edit when the capability surface changes.")

	assert.Equal(t, expectedMCPResourcePatterns, srv.RegisteredResourcePatterns(),
		"MCP resource surface drifted. Update registerMCPHandlers in mcp.go "+
			"AND expectedMCPResourcePatterns in this file.")
}

// TestMCPRegistration_NoDuplicates would be redundant: mcp.Server's
// tools and resources maps are keyed by Name()/URIPattern() respectively,
// so a duplicate registration silently replaces the earlier entry
// rather than producing two map keys. The expected-slice comparison
// above catches a duplicate indirectly: if registration dropped a
// handler by collision, the observed slice would be shorter than the
// expected slice and the test fails with a clear diff.
