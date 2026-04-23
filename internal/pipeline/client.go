package pipeline

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/certs"
)

// DefaultURL is the production base URL of the pipeline message
// service. Matches the `signatory serve --port 21517` default and
// the 127.0.0.1-only bind in the server.
//
// The URL is HTTPS because agent traffic is TLS-only (Claude Code's
// WebFetch forces HTTPS and refuses self-signed certs). See
// design/tls-trust.md for the trust architecture this default
// participates in.
const DefaultURL = "https://127.0.0.1:21517"

// defaultTimeout bounds a single HTTP round-trip. Generous enough
// for the ~10 MB handoff-deposit upper bound the server enforces;
// tight enough that a wedged service doesn't hang a skill run.
// Matches the server's WriteTimeout so both sides agree when to
// give up.
const defaultTimeout = 60 * time.Second

// Client is a low-ceremony HTTP client for the pipeline message
// service. Callers construct one per process (or per test) via
// NewClient and call typed methods — CreateSession, DepositMessage,
// etc. — rather than hand-rolling net/http. The client owns its
// TLS trust config per design/tls-trust.md, applied at construction
// so every call uses the same anchor.
type Client struct {
	baseURL string
	httpC   *http.Client
}

// NewClient returns a Client targeting pipelineURL. Accepted schemes:
//
//   - https:// — production path. TLS trust is configured from the
//     canonical CA anchor (~/.signatory/certs/rootCA.pem) with a
//     fallback to the system root pool if the anchor file is absent.
//     This matches the per-client trust rule in design/tls-trust.md
//     §"Go CLI clients": the anchor is the authoritative source, the
//     system pool is the bridge for users who haven't run
//     `signatory certs init` but have run `mkcert -install`.
//   - http:// — test and debug path. TLS config is skipped entirely
//     (plain HTTP is selected by scheme, not by an InsecureSkipVerify
//     or --no-tls flag). This is the path httptest servers use.
//
// InsecureSkipVerify is never set, even for localhost. See
// design/tls-trust.md §"Architectural commitments" (4).
//
// Returns an error if pipelineURL is unparseable or uses an unsupported
// scheme.
func NewClient(pipelineURL string) (*Client, error) {
	u, err := url.Parse(pipelineURL)
	if err != nil {
		return nil, fmt.Errorf("parse pipeline URL %q: %w", pipelineURL, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("pipeline URL %q: unsupported scheme %q (want http or https)",
			pipelineURL, u.Scheme)
	}

	httpC := &http.Client{Timeout: defaultTimeout}
	if u.Scheme == "https" {
		tlsCfg, err := buildTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("build TLS config: %w", err)
		}
		httpC.Transport = &http.Transport{TLSClientConfig: tlsCfg}
	}

	return &Client{
		baseURL: strings.TrimRight(pipelineURL, "/"),
		httpC:   httpC,
	}, nil
}

// buildTLSConfig constructs the trust config signatory Go clients use
// to verify the pipeline service's TLS cert. Loads the canonical anchor
// if present; falls back to the system root pool otherwise. Never
// disables verification.
//
// "Anchor absent" is not a fatal error — a user who has run
// `mkcert -install` (which populates the system keychain) but not yet
// `signatory certs init` (which creates the anchor file) should still
// be able to use the CLI. The system-root fallback is the bridge.
func buildTLSConfig() (*tls.Config, error) {
	caPath, err := certs.DefaultCAPath()
	if err != nil {
		// Home-directory resolution failed — exceptional. Return
		// a config that uses the system pool so the caller isn't
		// wedged on a pathological env.
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil //nolint:gosec // G402: TLS 1.2+ baseline; server presents locally-trusted mkcert cert
	}

	data, err := os.ReadFile(caPath) //nolint:gosec // G304: path resolved from a package-owned constant, not user input
	if errors.Is(err, os.ErrNotExist) {
		// Anchor absent. Fall back to system roots — the tls package
		// uses them by default when RootCAs is nil.
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil //nolint:gosec // G402: TLS 1.2+ baseline; system pool bridges users on mkcert -install only
	}
	if err != nil {
		return nil, fmt.Errorf("read CA anchor %s: %w", caPath, err)
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// System pool unavailable (rare, but happens in stripped
		// container environments). A fresh empty pool with just the
		// anchor is still sufficient for our one server.
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("CA anchor %s contained no usable PEM certificates", caPath)
	}

	return &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// CreateSession starts a pipeline session for the given target and
// returns the server's populated Session record (with ID, Status,
// CreatedAt). target is required; metadata is optional (pass "" to
// omit).
//
// Maps server 400 (target required) and 503 (session cap exceeded)
// to readable Go errors carrying the server's {"error": "..."} body
// so the caller can surface it without additional decoding.
func (c *Client) CreateSession(ctx context.Context, target, metadata string) (*Session, error) {
	var sess Session
	err := c.postJSON(ctx, "/api/sessions", "create session",
		createSessionRequest{Target: target, Metadata: metadata}, &sess)
	if err != nil {
		return nil, err
	}
	return &sess, nil
}

// DepositMessage posts a message to the given session. role must be
// one of the server's accepted values (security, provenance,
// synthesist, orchestrator); msgType is typically "handoff" from the
// handoff --deposit-to caller. content is the rendered body bytes as
// a string; it is safe to contain literal newlines and control
// characters — the JSON encoding path handles escaping, and the
// rendered bytes never cross a shell boundary.
//
// Returns the server's populated Message (with ID, CreatedAt) on
// success, or an error carrying the server's {"error": "..."} body
// on validation failure.
func (c *Client) DepositMessage(ctx context.Context, sessionID, role, msgType, content, metadata string) (*Message, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("deposit message: session id is required")
	}
	var msg Message
	err := c.postJSON(ctx, "/api/sessions/"+sessionID+"/messages", "deposit message",
		depositMessageRequest{
			Role:     role,
			MsgType:  msgType,
			Content:  content,
			Metadata: metadata,
		},
		&msg,
	)
	if err != nil {
		return nil, err
	}
	return &msg, nil
}

// postJSON is the shared POST+JSON-round-trip primitive. All of
// Client's typed methods compose over this — the API surface
// (CreateSession, DepositMessage, etc.) provides typed request/
// response shapes, and this function handles the transport plumbing:
// marshal, request construction, Content-Type, Do, status check,
// drain-on-close, decode.
//
// verb is the human-readable operation name threaded into errors
// ("create session", "deposit message") so the caller's message
// names what was being attempted. path is relative to baseURL.
// body is the request payload (marshaled to JSON); out is the
// destination for the decoded response (any non-nil pointer whose
// shape the server will return on 2xx).
//
// Accepts both 200 and 201 as success since the pipeline server
// returns 201 Created for session / message creation and could
// plausibly return 200 for future idempotent verbs.
func (c *Client) postJSON(ctx context.Context, path, verb string, body, out any) error {
	reqBody, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal %s body: %w", verb, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+path, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build %s request: %w", verb, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpC.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", verb, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return decodeServerError(resp, verb)
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", verb, err)
	}
	return nil
}

// drainAndClose is the canonical "I'm done with this response body"
// sequence: copy any unread bytes to io.Discard, then close. Without
// the drain step, Go's HTTP transport cannot return the underlying TCP
// connection to the keep-alive pool — every subsequent request opens a
// fresh connection. For a CLI running one request per invocation the
// cost is negligible, but §2 introduces DepositMessage which the skill
// calls three times per /analyze run, and a deposit error path that
// leaves ~4 KB unread would force a new dial for the next deposit.
//
// io.Copy to io.Discard tolerates any body size and any partial prior
// read (json.Decoder stops at the end of one object, LimitReader caps
// error-body reads at 4 KB, etc.); whatever's unread gets consumed and
// thrown away. Close errors are suppressed — there's nothing actionable
// to do with them at this point in the request lifecycle.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

// decodeServerError reads a non-success response body, extracts the
// server's {"error": "..."} message when present, and returns a
// formatted error with status code. If the body isn't the expected
// shape, falls back to including a short raw excerpt so the caller
// still gets something actionable.
//
// Surfaces the body-read error when it indicates a real failure (not
// just "we stopped at the 4 KB cap"): a truncated read produces an
// empty raw slice and a non-nil error; hiding that behind "server
// returned 500 with empty body" blames the status code for what was
// actually a network-level problem.
func decodeServerError(resp *http.Response, verb string) error {
	const maxRead = 4 * 1024
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRead))

	var envelope struct {
		Error string `json:"error"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &envelope) == nil && envelope.Error != "" {
		return fmt.Errorf("%s: server returned %d: %s",
			verb, resp.StatusCode, envelope.Error)
	}

	excerpt := string(bytes.TrimSpace(raw))
	if excerpt == "" {
		if readErr != nil {
			return fmt.Errorf("%s: server returned %d, but response body read failed: %w",
				verb, resp.StatusCode, readErr)
		}
		return fmt.Errorf("%s: server returned %d with empty body",
			verb, resp.StatusCode)
	}
	return fmt.Errorf("%s: server returned %d: %s", verb, resp.StatusCode, excerpt)
}
