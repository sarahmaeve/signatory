package resources

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/mcp"
)

// ConfigResource serves signatory://config — the effective MCP server
// configuration. Consumed by agents to understand what capabilities are
// active in this server instance.
//
// Security invariant: this resource MUST NEVER include secrets (API keys,
// tokens, credentials of any kind). The db_path field is deliberately
// included because agents benefit from knowing where the database lives,
// but the value is the path string only — not the database contents.
//
// Threat model note: the config resource is read by LLM agents. Leaking
// an API key here means it appears verbatim in the model's context window,
// which may be logged, persisted, or inadvertently included in a tool call
// to an external service. The "never include secrets" rule is structural,
// not advisory.
type ConfigResource struct {
	// DBPath is the resolved file path of the signatory SQLite database.
	// Populated by the server at startup from the --db flag or default
	// location. Never empty in production; may be empty in tests that
	// don't need path display.
	DBPath string

	// Version is the server version string surfaced in the config's
	// mcp_version field. Injected at construction alongside the other
	// server-identity values so this resource has no dependency on
	// package-level state; the same pattern as the Server struct's
	// version field (see internal/mcp/server.go). Production wires the
	// real version here from cmd/signatory/mcp.go; tests inject a
	// fixture string.
	Version string
}

// URIPattern returns the literal URI for this static resource.
func (r *ConfigResource) URIPattern() string {
	return "signatory://config"
}

// Description summarises the resource for resources/list.
func (r *ConfigResource) Description() string {
	return "READ THIS on session start (or when the user asks about signatory's setup) to confirm server version, transport, enabled capabilities, and the DB path. Contains no secrets. For orientation on what signatory does, read signatory://help instead."
}

// configData is the JSON shape returned by signatory://config.
//
// Fields are deliberately sparse in v0.1. Adding a field here expands
// the information surface available to agents; any new field must be
// reviewed for secret leakage before landing.
type configData struct {
	// MCPVersion is the running server's declared version string,
	// threaded in from ConfigResource.Version at construction.
	MCPVersion string `json:"mcp_version"`
	// Transport is always "stdio" in v0.1. HTTP/SSE is a v0.2+ feature.
	Transport string `json:"transport"`
	// DirectAPIActivated is false in v0.1. See design/mcp-server-
	// architecture.md §"Analyst-invocation mode" for the v0.2 activation
	// path. When false, agents that want LLM analysis must use the
	// dispatch-subagent pattern.
	DirectAPIActivated bool `json:"direct_api_activated"`
	// LLMSynthesisAvailable indicates whether the signatory_synthesize
	// tool supports llm_synthesis:true. True in v0.1 because the tool
	// returns a dispatch-subagent response (no local API key needed).
	LLMSynthesisAvailable bool `json:"llm_synthesis_available"`
	// DBPath is the filesystem path of the SQLite database in use.
	// Useful for agents that want to report "which database was queried."
	// This is a path string only — never database contents.
	//
	// security: this is a path, not credentials. Safe to surface.
	DBPath string `json:"db_path"`
}

// Read returns the static config. No store access needed.
func (r *ConfigResource) Read(_ context.Context, _ string) *mcp.Response {
	return mcp.OK(configData{
		MCPVersion:            r.Version,
		Transport:             "stdio",
		DirectAPIActivated:    false,
		LLMSynthesisAvailable: true,
		DBPath:                r.DBPath,
	})
}
