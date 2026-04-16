package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Request body size limits. The message limit is generous because
// handoff templates can be 500+ lines; the session limit is small
// because it's just a target URI and optional metadata.
const (
	maxSessionBodyBytes = 4 * 1024       // 4 KB
	maxMessageBodyBytes = 10 * 1024 * 1024 // 10 MB
)

// maxActiveSessions is the default cap on concurrent active sessions.
// Prevents unbounded growth from runaway loops. Configurable via
// NewServerWithOptions if we ever need it.
const maxActiveSessions = 100

// Valid role and msg_type values. Requests with values outside these
// sets are rejected with 400.
var (
	validRoles    = map[string]bool{"security": true, "provenance": true, "synthesist": true, "orchestrator": true}
	validMsgTypes = map[string]bool{"handoff": true, "output": true, "feedback": true, "template": true, "status": true}
)

// Server is the HTTP API for the pipeline message service.
// It binds to localhost only — this is a local development tool,
// not a network service.
type Server struct {
	store  *Store
	mux    *http.ServeMux
	server *http.Server
	logger *slog.Logger
}

// NewServer creates a pipeline server backed by the given store.
func NewServer(store *Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		store:  store,
		mux:    http.NewServeMux(),
		logger: logger,
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	s.mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	s.mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	s.mux.HandleFunc("POST /api/sessions/{id}/messages", s.handleDepositMessage)
	s.mux.HandleFunc("GET /api/sessions/{id}/messages", s.handleGetMessages)
	s.mux.HandleFunc("GET /api/sessions/{id}/messages/latest", s.handleGetLatestMessage)
	s.mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
}

// ListenAndServe starts the HTTP server on the given port.
// It binds to 127.0.0.1 only. If certFile and keyFile are both
// non-empty, TLS is enabled and the server listens on HTTPS.
// The server shuts down gracefully when ctx is cancelled.
//
// TLS is required for agent access via WebFetch (Claude Code's
// HTTP client forces HTTPS and rejects self-signed certs). Use
// mkcert or a similar tool to generate a locally-trusted cert
// for 127.0.0.1, and set NODE_EXTRA_CA_CERTS to the CA root so
// Claude Code's HTTP client trusts it.
func (s *Server) ListenAndServe(ctx context.Context, port int, certFile, keyFile string) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	// Graceful shutdown on context cancellation. The goroutine uses
	// context.Background() for the shutdown deadline deliberately:
	// when we reach the WithTimeout call below, the parent ctx has
	// already been cancelled (we waited for <-ctx.Done()). Deriving
	// the shutdown context from it would produce an immediately-dead
	// context and abort the graceful drain before any in-flight
	// request could finish. Background + a 5s timeout is correct.
	go func() { //nolint:gosec // G118: Background is deliberate; see comment above — parent ctx is already cancelled at the point of WithTimeout
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("server shutdown", "error", err)
		}
	}()

	tlsEnabled := certFile != "" && keyFile != ""
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	s.logger.Info("pipeline server listening", "addr", addr, "scheme", scheme)

	var err error
	if tlsEnabled {
		err = s.server.ListenAndServeTLS(certFile, keyFile)
	} else {
		err = s.server.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler returns the HTTP handler for testing with httptest.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// --- request/response types ---

type createSessionRequest struct {
	Target   string `json:"target"`
	Metadata string `json:"metadata,omitempty"`
}

type depositMessageRequest struct {
	Role     string `json:"role"`
	MsgType  string `json:"msg_type"`
	Content  string `json:"content"`
	Metadata string `json:"metadata,omitempty"`
}

// --- handlers ---

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSessionBodyBytes)

	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Target == "" {
		writeError(w, http.StatusBadRequest, "target is required")
		return
	}

	// Enforce session count limit.
	count, err := s.store.CountActiveSessions(r.Context())
	if err != nil {
		s.logger.Error("count sessions", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if count >= maxActiveSessions {
		writeError(w, http.StatusServiceUnavailable,
			"active session limit reached (%d); delete old sessions first", maxActiveSessions)
		return
	}

	sess, err := s.store.CreateSession(r.Context(), req.Target, req.Metadata)
	if err != nil {
		s.logger.Error("create session", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	s.logger.Info("session created", "id", sess.ID, "target", sess.Target)
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions(r.Context())
	if err != nil {
		s.logger.Error("list sessions", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if sessions == nil {
		sessions = []Session{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "session not found")
			return
		}
		s.logger.Error("get session", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		s.logger.Error("delete session", "id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.logger.Info("session deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDepositMessage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBodyBytes)
	sessionID := r.PathValue("id")

	var req depositMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Role == "" || req.MsgType == "" {
		writeError(w, http.StatusBadRequest, "role and msg_type are required")
		return
	}
	if !validRoles[req.Role] {
		writeError(w, http.StatusBadRequest, "invalid role %q", req.Role)
		return
	}
	if !validMsgTypes[req.MsgType] {
		writeError(w, http.StatusBadRequest, "invalid msg_type %q", req.MsgType)
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	msg := &Message{
		SessionID: sessionID,
		Role:      req.Role,
		MsgType:   req.MsgType,
		Content:   req.Content,
		Metadata:  req.Metadata,
	}
	msg, err := s.store.DepositMessage(r.Context(), msg)
	if err != nil {
		s.logger.Error("deposit message", "session", sessionID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	s.logger.Info("message deposited",
		"session", sessionID, "role", req.Role,
		"type", req.MsgType, "bytes", len(req.Content))
	writeJSON(w, http.StatusCreated, msg)
}

func (s *Server) handleGetMessages(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	filter := MessageFilter{
		SessionID: sessionID,
		Role:      r.URL.Query().Get("role"),
		MsgType:   r.URL.Query().Get("type"),
	}

	msgs, err := s.store.GetMessages(r.Context(), filter)
	if err != nil {
		s.logger.Error("get messages", "session", sessionID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Return content-only for single-message queries (typical agent use case).
	// Agents calling WebFetch want the handoff text, not a JSON envelope.
	if r.URL.Query().Get("format") == "raw" && len(msgs) == 1 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, msgs[0].Content) //nolint:errcheck // best-effort response write; client disconnect is not actionable
		return
	}

	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

func (s *Server) handleGetLatestMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	filter := MessageFilter{
		SessionID: sessionID,
		Role:      r.URL.Query().Get("role"),
		MsgType:   r.URL.Query().Get("type"),
	}

	msg, err := s.store.GetLatestMessage(r.Context(), filter)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "no matching message")
			return
		}
		s.logger.Error("get latest message", "session", sessionID, "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Raw format returns just the content as plain text.
	if r.URL.Query().Get("format") == "raw" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, msg.Content) //nolint:errcheck // best-effort response write; client disconnect is not actionable
		return
	}

	writeJSON(w, http.StatusOK, msg)
}

// --- response helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck,gosec // G104: best-effort response write; headers already committed, encode errors are not actionable
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck,gosec // G104: best-effort response write; headers already committed, encode errors are not actionable
}
