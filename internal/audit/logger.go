// Package audit implements the append-only audit log for trust-
// modifying actions. It writes to two backends simultaneously:
//
//  1. The store's audit_log table (for in-database querying).
//  2. A JSON-lines file at ~/.signatory/audit.log (for grep/tail,
//     external tooling, and survival of database corruption).
//
// Dual writes mean every trust decision is recorded in two independent
// places. If the store is compromised or corrupted, the file log is
// still intact; if the file log is deleted, the store still has the
// history. Dual redundancy is cheap and catches failure modes that
// a single sink would hide.
//
// The file writer is best-effort: if it fails, the database write
// still succeeds and the file error is logged to stderr. This is a
// deliberate trade-off — losing the file log line is less bad than
// failing the underlying operation (posture set, burn, etc.) because
// of a full disk on ~.
package audit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Store is the subset of the persistence interface the audit logger
// needs. We take this as an interface rather than the full Store type
// to keep the audit package decoupled from internal/store (avoiding
// import cycles and making the audit package easier to test).
type Store interface {
	AppendAuditEntry(ctx context.Context, entry *profile.AuditEntry) error
}

// Logger writes audit entries to both a Store and a JSON-lines file.
// Safe for concurrent use — the file writer is serialized with a mutex
// because os.File writes are not atomic across line boundaries on
// every platform and we do not want interleaved lines.
type Logger struct {
	store    Store
	filePath string

	mu sync.Mutex
}

// New constructs a Logger backed by the given store and file path.
// The file is created on first write, not on New, so constructing a
// Logger is cheap and cannot fail. Callers typically pass filePath =
// filepath.Join(os.UserHomeDir(), ".signatory", "audit.log") via the
// DefaultFilePath helper.
func New(store Store, filePath string) *Logger {
	return &Logger{
		store:    store,
		filePath: filePath,
	}
}

// DefaultFilePath returns ~/.signatory/audit.log, or an empty string
// and error if $HOME is not resolvable. Callers can choose to pass
// the empty string to New to disable file logging entirely.
func DefaultFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".signatory", "audit.log"), nil
}

// Log writes a single audit entry. The ID and Timestamp fields are
// auto-populated if empty — callers typically only need to set Actor,
// Action, EntityID, and Detail.
//
// Returns an error only if the database write fails. A file write
// failure is reported to stderr but does not fail the call, because
// the in-database entry is the authoritative record and the file is
// a redundant secondary sink.
func (l *Logger) Log(ctx context.Context, entry *profile.AuditEntry) error {
	if entry == nil {
		return fmt.Errorf("audit: nil entry")
	}
	if entry.ID == "" {
		entry.ID = newID()
	}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}

	if err := l.store.AppendAuditEntry(ctx, entry); err != nil {
		return fmt.Errorf("audit: store write: %w", err)
	}

	if l.filePath != "" {
		if err := l.appendFile(entry); err != nil {
			// Best-effort — do not fail the call.
			fmt.Fprintf(os.Stderr, "audit: file write failed (continuing): %v\n", err)
		}
	}
	return nil
}

// LogAction is a convenience wrapper for the common case: build the
// entry from positional fields and call Log. Actor must be the team
// identity string (see internal/identity). Detail must be JSON.
func (l *Logger) LogAction(ctx context.Context, actor, action, entityID, detail string) error {
	return l.Log(ctx, &profile.AuditEntry{
		Actor:    actor,
		Action:   action,
		EntityID: entityID,
		Detail:   detail,
	})
}

// appendFile writes one JSON-lines record to the file, creating
// parent directories and the file itself if needed. Uses O_APPEND
// so concurrent processes can safely write to the same file (each
// line is a single write() syscall < PIPE_BUF on modern systems).
func (l *Logger) appendFile(entry *profile.AuditEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// fileLine is the on-disk projection of an audit entry: compact
	// JSON with the same field names as the Go struct so the file and
	// database stay consistent.
	line := fileLine{
		Timestamp: entry.Timestamp.UTC().Format(time.RFC3339),
		Actor:     entry.Actor,
		Action:    entry.Action,
		Entity:    entry.EntityID,
		Detail:    json.RawMessage(detailOrEmpty(entry.Detail)),
		ID:        entry.ID,
	}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(l.filePath), 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	// Defense in depth against symlink hijack (#81): an attacker who
	// plants a symlink at the audit log path could otherwise redirect
	// audit writes to an arbitrary file, turning the audit logger into
	// a write-anywhere primitive when combined with attacker-influenced
	// Actor/Action/Detail fields.
	//
	// Lstat check: rejects an existing symlink at the target path.
	// Cross-platform but has a TOCTOU window between Lstat and OpenFile.
	// Skipped if the path doesn't exist yet (first write creates it).
	//
	// O_NOFOLLOW flag: atomic, TOCTOU-free defense at open(2) time.
	// Real on Unix (syscall.O_NOFOLLOW), 0 on other platforms — see
	// nofollow_unix.go and nofollow_other.go.
	if info, err := os.Lstat(l.filePath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("audit log path is a symlink, refusing to follow: %s", l.filePath)
	}

	f, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY|nofollowFlag, 0600)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// fileLine is the JSON shape of one line in the audit file. It differs
// from profile.AuditEntry in that Detail is embedded as raw JSON (not
// quoted as a string) so the file is human-readable and jq-friendly.
type fileLine struct {
	Timestamp string          `json:"timestamp"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Entity    string          `json:"entity,omitempty"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	ID        string          `json:"id"`
}

// detailOrEmpty returns the detail string as-is if it's valid JSON,
// or "{}" otherwise. This keeps the on-disk file valid JSON lines
// even when a caller passes a non-JSON detail by accident.
func detailOrEmpty(detail string) string {
	if detail == "" {
		return "{}"
	}
	if !json.Valid([]byte(detail)) {
		return "{}"
	}
	return detail
}

// newID generates an opaque audit entry ID. 16 random bytes →
// 32-char hex — unique with astronomical probability and fixed-width
// for easy indexing.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read effectively never fails on supported platforms.
		// If it does, fall back to a time-based ID so Log() never
		// panics on something as fundamental as ID generation.
		return fmt.Sprintf("audit-fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
