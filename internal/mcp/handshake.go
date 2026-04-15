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
type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfoBody     `json:"serverInfo"`
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
	}, nil
}

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
