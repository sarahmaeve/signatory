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
// owned by the Server and its methods are called from the dispatch loop.
type handshake struct {
	state  lifecycleState
	client clientInfo
	// version is the server version announced in the initialize
	// response's serverInfo.version. Threaded from the owning Server's
	// injected value so tests and production stamp distinct strings
	// without racing on a package-level var.
	version string
}

// handleInitialize processes an initialize request. Returns the result
// to send back. May only be called once; second call returns an error
// response code.
func (h *handshake) handleInitialize(params json.RawMessage) (*initializeResult, error) {
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

When a user asks about dependency safety, supply-chain risk, whether a package is trustworthy, assessment conclusions, or posture decisions, prefer the signatory_* tools (signatory_analyze, signatory_show_analyses, signatory_show_conclusions, signatory_show_methodology, signatory_signals, signatory_detail, signatory_survey) and signatory:// resources over grep, file search, or web lookups. The tools query a structured store built from prior analyst runs.

Key distinctions:
- signatory_analyze returns a single target's cached trust summary; signatory_signals returns its raw evidence records.
- signatory_show_analyses lists what has been assessed; signatory_show_conclusions searches individual concerns across analyses.
- "Conclusions" are Layer-2 reasoned interpretations produced by analysts (human or AI), not Layer-1 mechanical observations. The word choice is deliberate — these are discernments, not discoveries.
- Analyses are ingested, not live-scanned: NotFound means "not in the store," not "failed to analyze."

Read signatory://help for the full tool-selection guide and concept map.`

// handleInitializedNotification processes the notifications/initialized
// notification. Transitions state to operational. Safe to call only
// when in stateInitialized.
func (h *handshake) handleInitializedNotification() error {
	if h.state != stateInitialized {
		// Spec says ignore notifications before/after the expected
		// window, but log it for observability. Not a fatal error.
		return nil
	}
	h.state = stateOperational
	return nil
}

// isOperational reports whether the lifecycle is past the handshake
// and ready for tool/resource calls.
func (h *handshake) isOperational() bool {
	return h.state == stateOperational
}

// ClientInfo returns the clientInfo captured during the initialize
// handshake. Safe to call at any time; returns zero value before
// initialize is processed.
func (h *handshake) ClientInfo() clientInfo {
	return h.client
}
