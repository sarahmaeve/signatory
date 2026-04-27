package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mcpForbiddenTokens lists substrings that internal store / driver
// errors commonly carry. None of them should ever appear in an MCP
// error response Message — the LLM caller has no use for them and
// they leak local DB paths, schema topology, and FK structure
// across the trust boundary.
//
// Mirrors the pipeline server's hardening list at
// internal/pipeline/hardening_test.go:120 — kept consistent so the
// two layers share one notion of "what counts as a leak."
var mcpForbiddenTokens = []string{
	"sqlite", "SQLITE",
	"FOREIGN KEY", "foreign key",
	"constraint",
	".db", ".sqlite",
	"goroutine", "runtime.",
	"/Users/", "/home/", "/tmp/",
	"sql:",
}

// assertNoMCPLeak fails the test if the message contains any of the
// forbidden tokens. Reports the offending token so the failure mode
// is obvious from the test output.
func assertNoMCPLeak(t *testing.T, message string) {
	t.Helper()
	lower := strings.ToLower(message)
	for _, tok := range mcpForbiddenTokens {
		// Case-insensitive on the lowercase variants; case-sensitive
		// for the all-caps and path-prefix tokens (a stray "/Users/"
		// in a message body is a leak no matter the case context).
		if tok == strings.ToLower(tok) {
			assert.NotContains(t, lower, tok,
				"MCP error message must not leak internal token %q; got %q", tok, message)
		} else {
			assert.NotContains(t, message, tok,
				"MCP error message must not leak internal token %q; got %q", tok, message)
		}
	}
}

// TestHardening_AnalyzeTool_StoreErrorDoesNotLeak induces a store
// failure (DB closed under the tool) and asserts the resulting MCP
// error response does not echo SQLite/driver internals. Pairs with
// the pipeline server's TestHardening_ErrorSanitization — the two
// MCP boundaries (signal pipeline + tool handlers) share the same
// no-leak contract.
func TestHardening_AnalyzeTool_StoreErrorDoesNotLeak(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	seedEntity(t, s, "repo:github/acme/myrepo", "acme/myrepo")

	// Close the underlying DB; the next store call from inside
	// Handle will fail with a database/sql driver error. Pre-fix,
	// that error string was concatenated into the response.
	require.NoError(t, s.Close())

	tool := &AnalyzeTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/myrepo"}`))

	require.Equal(t, "error", resp.Status,
		"closed-DB analyze must surface as error, not ok")
	require.NotNil(t, resp.Error)
	assertNoMCPLeak(t, resp.Error.Message)
}

// TestHardening_IngestAnalysisTool_StoreErrorDoesNotLeak is the
// matching contract for the mutating tool path. Same induction
// (DB closed); same no-leak assertion.
func TestHardening_IngestAnalysisTool_StoreErrorDoesNotLeak(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Close the DB before ingest. The validator and JSON decode
	// phases pass (no store touched yet); the failure surfaces
	// from IngestAnalystOutput at the bottom of Handle.
	require.NoError(t, s.Close())

	tool := &IngestAnalysisTool{Store: s}
	payload := wrapIngestPayload(t, minimalValidAnalystOutputJSON(t), "")
	resp := tool.Handle(context.Background(), payload)

	require.Equal(t, "error", resp.Status,
		"closed-DB ingest must surface as error, not ok")
	require.NotNil(t, resp.Error)
	assertNoMCPLeak(t, resp.Error.Message)
}
