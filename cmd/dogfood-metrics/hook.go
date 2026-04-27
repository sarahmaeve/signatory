package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Maximum length for the `detail` field in a written hook event.
// Long Bash command lines or URLs get truncated to keep per-session
// reports skimmable; the full payload still lives in the OTEL trace
// when we need it.
const maxDetailLength = 200

// hookInputEnvelope models the JSON Claude Code passes on hook
// stdin. Round-3 verification confirmed these fields are always
// present: session_id, cwd, transcript_path, plus event-specific
// (tool_name + tool_input + tool_use_id for PreToolUse).
//
// We deliberately use json.RawMessage for tool_input rather than a
// concrete shape — the per-tool field set varies (Bash has command,
// Read has file_path, WebFetch has url, MCP tools have arbitrary
// shapes) and we extract per-tool fields downstream.
type hookInputEnvelope struct {
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	TranscriptPath string          `json:"transcript_path"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// hookEvent is the line we write to disk per hook invocation.
// JSONL format: one event per line.
type hookEvent struct {
	Timestamp      string `json:"ts"`
	Event          string `json:"event"`
	SessionID      string `json:"session_id,omitempty"`
	ToolUseID      string `json:"tool_use_id,omitempty"`
	ToolName       string `json:"tool_name,omitempty"`
	Classification string `json:"classification,omitempty"`
	Detail         string `json:"detail,omitempty"`
	CWD            string `json:"cwd,omitempty"`
}

// runHook is the testable core of the hook subcommand. Reads JSON
// from r, classifies the tool call, appends a JSONL line to the
// per-session file in outDir. Always returns nil — the hook MUST
// NOT exit non-zero (round-3: exit code 2 blocks the tool call,
// which is the opposite of what observe-only telemetry wants).
//
// On parse failure, writes a `malformed` event to a generic file
// (no session_id available) and returns nil. Drop-on-the-floor is
// worse than recording the malformed-input fact.
func runHook(r io.Reader, outDir, event string, now time.Time) error {
	body, err := io.ReadAll(io.LimitReader(r, 1024*1024))
	if err != nil {
		return writeMalformed(outDir, event, now, fmt.Sprintf("read stdin: %v", err))
	}

	var env hookInputEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return writeMalformed(outDir, event, now, fmt.Sprintf("parse stdin: %v", err))
	}

	classification, detail := classify(env.CWD, env.ToolName, env.ToolInput)

	ev := hookEvent{
		Timestamp:      now.UTC().Format(time.RFC3339),
		Event:          event,
		SessionID:      env.SessionID,
		ToolUseID:      env.ToolUseID,
		ToolName:       env.ToolName,
		Classification: classification,
		Detail:         truncate(detail, maxDetailLength),
		CWD:            env.CWD,
	}

	filename := "hooks-malformed.jsonl"
	if env.SessionID != "" {
		filename = "hooks-" + env.SessionID + ".jsonl"
	}
	return writeEvent(filepath.Join(outDir, filename), ev)
}

// writeMalformed records a hook invocation we couldn't parse, so we
// know the hook fired even when the input is unusable. Same return
// contract as runHook (always nil — never block the tool call).
func writeMalformed(outDir, event string, now time.Time, reason string) error {
	ev := hookEvent{
		Timestamp: now.UTC().Format(time.RFC3339),
		Event:     "malformed",
		Detail:    truncate(reason+" (original event: "+event+")", maxDetailLength),
	}
	_ = writeEvent(filepath.Join(outDir, "hooks-malformed.jsonl"), ev)
	return nil
}

// writeEvent appends a single JSON-encoded event line to path. The
// JSONL invariant (one JSON value per line) is preserved by
// json.Marshal's compact output plus an explicit newline.
func writeEvent(path string, ev hookEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: path constructed from receiver-controlled outDir + a sanitized session_id
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close() //nolint:errcheck // close after append; err already surfaced via Write
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	return nil
}

// classify maps (cwd, toolName, toolInput) to a (classification,
// detail) pair. The classification taxonomy is the dogfood-relevant
// "where did this call's data come from" axis, per design/agent-otel.md:
//
//   - local_db: signatory MCP server (data we already have)
//   - local_signatory_cli: invoking our own CLI/code locally
//   - local_other: any other local action (cat, ls, grep, etc.)
//   - external_web: WebFetch / WebSearch (always external)
//   - external_curl: Bash command shelling out to curl/wget/gh-api
//   - external_git: Bash git operation against a remote
//   - signatory_source: Read against signatory's own source tree
//     (the underspecification signal — analyst should never need
//     to read internal/, cmd/, or templates/handoffs/)
func classify(cwd, toolName string, toolInput json.RawMessage) (classification, detail string) {
	switch {
	case strings.HasPrefix(toolName, "mcp__signatory__"):
		return "local_db", toolName

	case toolName == "WebFetch":
		url := jsonField(toolInput, "url")
		return "external_web", url

	case toolName == "WebSearch":
		query := jsonField(toolInput, "query")
		return "external_web", query

	case toolName == "Bash":
		cmd := jsonField(toolInput, "command")
		return classifyBashCommand(cmd), cmd

	case toolName == "Read":
		path := jsonField(toolInput, "file_path")
		return classifyReadPath(cwd, path), path

	default:
		return "local_other", toolName
	}
}

// classifyBashCommand applies prefix/contains rules to the raw
// command string. The rules are deliberately narrow — we'd rather
// classify a borderline command as local_other and miss it than
// over-classify and produce false-positive economics signals.
func classifyBashCommand(cmd string) string {
	trimmed := strings.TrimSpace(cmd)
	if trimmed == "" {
		return "local_other"
	}
	switch {
	case trimmed == "signatory" || strings.HasPrefix(trimmed, "signatory "):
		return "local_signatory_cli"
	case strings.HasPrefix(trimmed, "go run ./cmd/signatory"):
		return "local_signatory_cli"
	case strings.HasPrefix(trimmed, "curl ") || strings.HasPrefix(trimmed, "wget "):
		return "external_curl"
	case strings.HasPrefix(trimmed, "gh "):
		// gh shells out to GitHub's API for most subcommands.
		// Conservative: treat all `gh` as external.
		return "external_curl"
	case strings.HasPrefix(trimmed, "git clone ") ||
		strings.HasPrefix(trimmed, "git fetch ") ||
		strings.HasPrefix(trimmed, "git fetch") && (len(trimmed) == len("git fetch")) ||
		strings.HasPrefix(trimmed, "git push ") ||
		strings.HasPrefix(trimmed, "git ls-remote ") ||
		strings.HasPrefix(trimmed, "git pull "):
		return "external_git"
	default:
		return "local_other"
	}
}

// classifyReadPath fires the underspecification signal when an
// analyst (or any other agent) reads signatory's own source. The
// scope is intentional:
//
//   - internal/: business logic the analyst should consume via MCP
//   - cmd/: CLI entry points the analyst should invoke, not read
//   - templates/handoffs/: handoff prompts the analyst already
//     received as part of the dispatch — re-reading them suggests
//     the prompt didn't surface what they needed
//
// design/, README, scripts/ etc. are NOT classified as source —
// reading those is normal documentation lookup.
func classifyReadPath(cwd, path string) string {
	if cwd == "" || path == "" {
		return "local_other"
	}
	for _, sub := range []string{"/internal/", "/cmd/", "/templates/handoffs/"} {
		if strings.HasPrefix(path, cwd+sub) {
			return "signatory_source"
		}
	}
	return "local_other"
}

// jsonField extracts a top-level string field from a json.RawMessage.
// Returns empty string on missing field, wrong type, or parse
// failure — callers use that as the "we don't have this datum"
// signal rather than failing the hook.
func jsonField(raw json.RawMessage, key string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	v, ok := m[key].(string)
	if !ok {
		return ""
	}
	return v
}

// truncate caps s at n bytes, appending an ellipsis when shortened.
// Used for the `detail` field so a 10 KiB curl command doesn't
// blow up a per-session report.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
