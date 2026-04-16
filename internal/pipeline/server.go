package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
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
// It binds to 127.0.0.1 only. The server shuts down gracefully
// when ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	// Graceful shutdown on context cancellation.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("server shutdown", "error", err)
		}
	}()

	s.logger.Info("pipeline server listening", "addr", addr)
	if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
	var req createSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if req.Target == "" {
		writeError(w, http.StatusBadRequest, "target is required")
		return
	}

	sess, err := s.store.CreateSession(r.Context(), req.Target, req.Metadata)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create session: %v", err)
		return
	}

	s.logger.Info("session created", "id", sess.ID, "target", sess.Target)
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list sessions: %v", err)
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
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "session %q not found", id)
			return
		}
		writeError(w, http.StatusInternalServerError, "get session: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "delete session: %v", err)
		return
	}
	s.logger.Info("session deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDepositMessage(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")

	var req depositMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: %v", err)
		return
	}
	if req.Role == "" || req.MsgType == "" {
		writeError(w, http.StatusBadRequest, "role and msg_type are required")
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
		writeError(w, http.StatusInternalServerError, "deposit message: %v", err)
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
		writeError(w, http.StatusInternalServerError, "get messages: %v", err)
		return
	}

	// Return content-only for single-message queries (typical agent use case).
	// Agents calling WebFetch want the handoff text, not a JSON envelope.
	if r.URL.Query().Get("format") == "raw" && len(msgs) == 1 {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, msgs[0].Content)
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
		if strings.Contains(err.Error(), "no rows") {
			writeError(w, http.StatusNotFound, "no matching message")
			return
		}
		writeError(w, http.StatusInternalServerError, "get latest: %v", err)
		return
	}

	// Raw format returns just the content as plain text.
	if r.URL.Query().Get("format") == "raw" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, msg.Content)
		return
	}

	writeJSON(w, http.StatusOK, msg)
}

// --- response helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck // best-effort response write
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck // best-effort
}
