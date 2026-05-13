// Package mcp: initialize/initialized lifecycle.
//
// The MCP lifecycle requires:
//  1. Client sends initialize (request with id).
//  2. Server responds with its capabilities and serverInfo.
//  3. Client sends notifications/initialized (notification, no id).
//  4. Server is now in operational state.
//
// Any requests (other than initialize or ping) received before the
// initialized notification is seen are rejected per spec.
//
// clientInfo (name + version) from the initialize request is recorded
// for the audit trail: every tool invocation stamps
// "source: mcp/<name>/<version>".
package mcp

import (
	"encoding/json"
	"fmt"
	"sync"
)

// protocolVersion is the MCP spec version this server implements.
const protocolVersion = "2025-11-25"

// initializeParams mirrors the params of the initialize request.
// We only decode the fields we need; extra capabilities are ignored.
type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      clientInfo         `json:"clientInfo"`
}

// clientCapabilities holds the subset of client capability flags we
// care about. Extended with fields as needed in future versions.
type clientCapabilities struct {
	Tools     *struct{} `json:"tools,omitempty"`
	Resources *struct{} `json:"resources,omitempty"`
}

// clientInfo carries the name and version from initialize.clientInfo.
// Stored by the handshake and exposed for audit logging.
type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Source returns the audit-trail source tag "mcp/<name>/<version>"
// for this client. Returns "mcp/unknown/unknown" when called before
// the handshake completes.
func (c clientInfo) Source() string {
	name := c.Name
	if name == "" {
		name = "unknown"
	}
	ver := c.Version
	if ver == "" {
		ver = "unknown"
	}
	return fmt.Sprintf("mcp/%s/%s", name, ver)
}

// initializeResult is the result body of the server's initialize response.
// Instructions carries server-level orientation that the client may
// surface to the model — per MCP 2025-11-25 §Lifecycle/Initialize it
// is the designated channel for "what this server is for and when to
// reach for it." Empty is legal; non-empty is strongly recommended
// because it's pushed to the model on every session start, whereas
// resources are only read when the model chooses to fetch them.
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfoBody     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// serverCapabilities declares what the server supports. v0.1 declares
// tools and resources; no subscriptions or listChanged for now.
type serverCapabilities struct {
	Tools     *struct{} `json:"tools,omitempty"`
	Resources *struct{} `json:"resources,omitempty"`
}

// serverInfoBody is the serverInfo block in the initialize response.
type serverInfoBody struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// lifecycleState tracks where we are in the MCP lifecycle.
type lifecycleState int

const (
	statePreInit     lifecycleState = iota // before any initialize request
	stateInitialized                       // after initialize response sent, before notifications/initialized
	stateOperational                       // after notifications/initialized received
)

// handshake manages the MCP initialize/initialized lifecycle. It is
// owned by the Server; its mutating methods (handleInitialize,
// handleInitializedNotification) run in the Serve read loop, while
// its observers (isOperational, ClientInfo) run in spawned handler
// goroutines per server.go:217. The mu guard closes the resulting
// race on state + client — see handshake_test.go and
// server_test.go's TestServer_HandshakeRace_* for the red-before-
// green evidence.
//
// version is set once at construction (NewServer) and never mutated
// thereafter, so it is outside the lock's coverage.
type handshake struct {
	mu     sync.RWMutex
	state  lifecycleState
	client clientInfo
	// version is the server version announced in the initialize
	// response's serverInfo.version. Threaded from the owning Server's
	// injected value so tests and production stamp distinct strings
	// without racing on a package-level var. Immutable after
	// construction — does not require lock coverage.
	version string
}

// handleInitialize processes an initialize request. Returns the result
// to send back. May only be called once; second call returns an error
// response code.
//
// Holds the write lock for the full method body. Callers are the
// Serve read loop (synchronous dispatch of the initialize frame),
// so there is no reentrancy risk, and the lock span includes the
// JSON unmarshal (which operates on a stack-local, not the handshake
// struct) for simplicity — releasing mid-method would require
// splitting the state check from the state mutation.
func (h *handshake) handleInitialize(params json.RawMessage) (*initializeResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.state != statePreInit {
		return nil, fmt.Errorf("initialize already called")
	}

	var p initializeParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid initialize params: %w", err)
		}
	}

	// Record clientInfo for audit trail regardless of version match.
	h.client = p.ClientInfo

	// Version negotiation: if client asks for a version we don't speak,
	// we respond with our version. Per spec the client SHOULD disconnect
	// if it doesn't support our version — that's the client's decision.
	// We always respond with our protocolVersion.

	h.state = stateInitialized

	empty := struct{}{}
	return &initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities: serverCapabilities{
			Tools:     &empty,
			Resources: &empty,
		},
		ServerInfo: serverInfoBody{
			Name:    "signatory",
			Version: h.version,
		},
		Instructions: serverInstructions,
	}, nil
}

// serverInstructions is the routing nudge the server emits on every
// initialize. Kept deliberately short — this text rides in every
// session's context window, so brevity matters.
//
// Goals (in priority order):
//  1. Tell the model what kind of tool signatory is (supply-chain
//     trust analysis), so the model can decide if a user question
//     is in-scope.
//  2. Steer it toward signatory_* tools over generic grep/file
//     searches for in-scope questions — this addresses the
//     dogfood finding where a freshly-connected session reached
//     for file tools despite the MCP surface being available.
//  3. Point at signatory://help for fuller guidance, so we don't
//     have to inline the whole question→tool map here.
//
// Maintenance: when adding a new tool or resource, update the
// "typical entry points" section of signatory://help FIRST (that's
// the deep content) and update this text only if the top-level
// framing changes.
const serverInstructions = `signatory is a supply-chain trust analysis tool. Its MCP surface provides read-only access to a local store of trust analyses, conclusions, postures, and burns.

When a user asks about dependency safety, supply-chain risk, whether a package is trustworthy, assessment conclusions, or posture decisions, prefer the signatory_* tools (signatory_analyze, signatory_show_analyses, signatory_show_conclusions, signatory_show_methodology, signatory_signals, signatory_detail, signatory_deltas, signatory_survey) and signatory:// resources over grep, file search, or web lookups. The tools query a structured store built from prior analyst runs.

Routing priority for "is X safe?" questions:
1. FIRST check signatory_analyze — if the target is in the store, answer from it. This is a cache lookup, not a live scan.
2. If signatory_analyze returns NotFound, the target hasn't been assessed. THEN offer to run the /analyze skill — it dispatches specialist analyst agents using signatory handoff prompts, ingests their v1-schema JSON output, and synthesizes a combined assessment. The /analyze skill IS the automated pipeline.
3. Do NOT skip step 1 and go straight to /analyze or /vet-dependency. The store may already have the answer, and re-collecting is expensive.
4. /vet-dependency is the manual human-readable fallback — use it only when the user explicitly requests a narrative document, not as the default pipeline.

Key distinctions:
- signatory_analyze returns a single target's cached trust summary; signatory_signals returns its raw evidence records.
- signatory_show_analyses lists what has been assessed; signatory_show_conclusions searches individual concerns across analyses.
- signatory_deltas is the time-series companion: it answers "what changed for X recently?" or "show transitions since <time>" with per-field before/after diffs. signatory_signals shows the current snapshot; signatory_deltas shows movement across snapshots. Requires an explicit time scope (since/last/range_start+range_end); caps total transitions at 200 with a truncated flag.
- "Conclusions" are Layer-2 reasoned interpretations produced by analysts (human or AI), not Layer-1 mechanical observations. The word choice is deliberate — these are discernments, not discoveries.
- Analyses are ingested, not live-scanned: NotFound means "not in the store," not "failed to analyze." NotFound is the signal to escalate to collection, not to retry.

Read signatory://help for the full tool-selection guide and concept map.`

// handleInitializedNotification processes the notifications/initialized
// notification and transitions state to operational.
//
// The MCP lifecycle (2025-11-25) says clients send this notification
// exactly once after a successful initialize response. If it arrives
// outside that window (e.g. before initialize, or a duplicate after
// already-operational), the spec is explicit: the server ignores it.
// No error return — there is nothing the caller could do that the spec
// doesn't already prescribe, and silently ignoring matches the spec's
// "strict outputs, liberal inputs" posture for notifications.
func (h *handshake) handleInitializedNotification() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != stateInitialized {
		return
	}
	h.state = stateOperational
}

// isOperational reports whether the lifecycle is past the handshake
// and ready for tool/resource calls. Called from spawned handler
// goroutines (server.go:217), hence the read-lock — paired with
// handleInitialize / handleInitializedNotification's write-lock to
// close the race surfaced by TestHandshake_StateAccessIsRaceFree.
func (h *handshake) isOperational() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.state == stateOperational
}

// ClientInfo returns the clientInfo captured during the initialize
// handshake. Safe to call at any time; returns zero value before
// initialize is processed.
//
// Read-locked on the same mutex that guards state — the two fields
// are written together by handleInitialize, and the doc comment on
// h.client says it is "exposed for audit logging," which means
// handler goroutines will call this once audit wiring is stamping
// source tags per request. Closing the race here is forward-
// looking coverage; see TestHandshake_ClientInfoIsRaceFree.
func (h *handshake) ClientInfo() clientInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}
