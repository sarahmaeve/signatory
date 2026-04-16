// Package pipeline provides an ephemeral message-passing service for
// signatory's analyst pipeline. It enables orchestrators and analyst
// agents to exchange handoffs, output, and feedback through a local
// HTTP API backed by SQLite, eliminating /tmp files and context-window
// pressure.
//
// Core concepts:
//
//   - Session: one pipeline run (e.g., "analyze alacritty"). Scoped by
//     a target URI and a unique ID. Multiple sessions can run concurrently.
//   - Message: typed, role-scoped content within a session. Types include
//     handoff (orchestrator → agent), output (agent → orchestrator),
//     feedback (service → agent), and template fragments.
//
// The store uses the same SQLite database as the main signatory store,
// with its own namespaced tables (pipeline_sessions, pipeline_messages).
// Data is ephemeral — sessions can be cleaned up without affecting the
// trust analysis store.
package pipeline

import "time"

// Session represents one pipeline run.
type Session struct {
	ID        string    `json:"id"`
	Target    string    `json:"target"`
	Status    string    `json:"status"` // active, complete, failed
	CreatedAt time.Time `json:"created_at"`
	Metadata  string    `json:"metadata,omitempty"` // JSON blob
}

// Message is a typed, role-scoped piece of content within a session.
type Message struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`     // security, provenance, synthesist, orchestrator
	MsgType   string    `json:"msg_type"` // handoff, output, feedback, template, status
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	Metadata  string    `json:"metadata,omitempty"` // JSON blob
}

// MessageFilter specifies which messages to retrieve.
type MessageFilter struct {
	SessionID string
	Role      string // optional; empty = all roles
	MsgType   string // optional; empty = all types
}
