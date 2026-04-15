# Signatory: MCP Protocol Envelopes

## Status

Draft. Companion to
[`design/mcp-server-architecture.md`](mcp-server-architecture.md).
Where that document decides, this one specifies the wire format.

Scope: every tool's input schema, output schema, error shape, and the
shared envelope conventions. Intended as the canonical reference when
implementing `internal/mcp/tools/*.go` handlers and when writing
client-side integration tests.

## Framing

All communication is JSON-RPC 2.0 over stdio per the MCP specification
at <https://modelcontextprotocol.io/specification/2025-11-25>.
Requests have `id`, `method`, `params`; responses have `id` plus either
`result` or `error`. Notifications (server-to-client, no response
expected) use no `id`. signatory-mcp does not emit notifications in
v0.1 (deferred: progress notifications for long-running refresh
operations).

**Spec-version alignment caveat:** the example envelopes below were
drafted against the 2025-11-25 spec but are illustrations of our
intended semantics. The Phase 1 implementer should cross-check each
shape against the current published spec before committing to it —
specifically the handshake `protocolVersion` negotiation, the
`content` array shape in tool responses, and any changes to the
resource URI/query-parameter handling. Differences should be
reconciled in code, not in this doc.

## Handshake

Client initiates:

```json
{
  "jsonrpc": "2.0", "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2025-11-25",
    "capabilities": {"tools": {}, "resources": {}},
    "clientInfo": {"name": "claude-code", "version": "1.x"}
  }
}
```

Server responds:

```json
{
  "jsonrpc": "2.0", "id": 1,
  "result": {
    "protocolVersion": "2025-11-25",
    "capabilities": {"tools": {}, "resources": {}},
    "serverInfo": {"name": "signatory", "version": "0.1.0"}
  }
}
```

`clientInfo.name` and `clientInfo.version` are persisted with every
audit entry as `source: mcp/<name>/<version>`.

## Tool-response envelope (universal)

Every `tools/call` response wraps the tool-specific payload in:

```json
{
  "content": [{"type": "text", "text": "<JSON-serialized envelope>"}],
  "isError": false
}
```

Where `<envelope>` is:

```json
{
  "status": "ok",
  "data": { /* tool-specific */ },
  "metadata": {
    "server_version": "0.1.0",
    "elapsed_ms": 42,
    "cache_hit": true,
    "requires_confirm": false
  }
}
```

On error, `isError: true` and:

```json
{
  "status": "error",
  "error": {
    "code": "schema_violation",
    "message": "Human-readable explanation.",
    "details": { /* code-specific */ }
  },
  "metadata": { "server_version": "0.1.0", "elapsed_ms": 3 }
}
```

### Error codes (tool-level)

| Code | Meaning |
|---|---|
| `schema_violation` | Input rejected; `details.field` names the problem field; `details.valid_fields` lists accepted names |
| `not_found` | Target entity not in store; `details.target` echoes the query |
| `cache_miss_requires_refresh` | Cached data absent; client should suggest `signatory_refresh` |
| `unsafe_operation_needs_confirm` | Write tool invoked without `confirm_token` |
| `invalid_confirm_token` | Token expired, tampered, or already used; `details.reason` specifies |
| `dispatch_requested` | Not a true error (status is `ok`); indicates the `data` is a dispatch-subagent directive — see separate shape |
| `validation_failed` | `signatory_ingest` schema check failed; `details.errors` is the validator output |
| `direct_api_not_activated` | Direct-API mode requested but v0.1 config refuses |
| `internal_error` | Unexpected server-side failure; `details` may include a stack-free summary |

## Input-schema discipline

**Strict-reject:** every tool's `inputSchema` in `tools/list` declares
`additionalProperties: false`. Unknown fields produce
`schema_violation` with the field name in `details.field`. Error
messages read like:

> Unknown field `hypotethical_flag` in tool input for `signatory_analyze`. Valid fields: `target`, `refresh`.

(Note the deliberate misspelling in the hypothetical — typos are the
common case this defense catches.)

## Per-tool schemas

### `signatory_analyze`

Retrieve the cached profile for a target, with optional refresh.

**Input:**
```json
{
  "target": "string — canonical URI, URL, or owner/repo shorthand",
  "depth": "provenance | signals (default: provenance)",
  "refresh": "bool (default: false)"
}
```

**Behavior:** normalizes `target` via `profile.NormalizeGitHubRepoInput`
or equivalent. Returns cached data; on cache miss with `refresh: false`,
returns `cache_miss_requires_refresh` error. With `refresh: true`,
invokes `signatory_refresh` semantics inline (same confirmation flow).

**Output (`status: ok`):**
```json
{
  "entity": {
    "canonical_uri": "repo:github/owner/name",
    "short_name": "owner/name",
    "entity_type": "project",
    "temporal_era": "modern_ai",
    "posture": {"tier": "trusted_for_now", "set_at": "2026-04-14T…"}
  },
  "signals_summary": {
    "vitality": {"last_commit": "…", "commits_30d": 14},
    "governance": {"maintainer_count": 3, "commit_signing_pct": 40},
    "criticality": {"stars": 12847, "adoption": "high"}
  },
  "anomalies": [],
  "forgery_resistance": "medium-declining"
}
```

### `signatory_signals`

Layer 1 raw signal output for a target. No LLM, no synthesis.

**Input:** `{target: "..."}`
**Output:** array of signal records matching the `signals` table shape
plus `details` JSON. Full fidelity; no summarization.

### `signatory_detail`

Drill into one signal group for an entity.

**Input:**
```json
{
  "target": "...",
  "signal_group": "vitality | governance | publication | hygiene | criticality | history"
}
```

**Output:** full signal records for the requested group including
source, timestamp, `forgery_resistance` rating, and the `details` JSON.

### `signatory_survey`

Dependency-tree posture overview.

**Input:**
```json
{
  "manifest_path": "string (default: cwd)",
  "refresh": "bool (default: false)"
}
```

**Output:**
```json
{
  "direct_count": 47,
  "transitive_count": 312,
  "posture_breakdown": {
    "vetted_frozen": 8,
    "trusted_for_now": 91,
    "unexamined": 258,
    "unknown_provenance": 2
  },
  "top_concerns": [
    {"target": "…", "signal": "fallow_status", "severity": "high"}
  ],
  "candidates_for_review": [
    {"target": "…", "criticality": "high", "examined": false}
  ]
}
```

### `signatory_refresh`

Collect signals from network sources. Subject to user confirmation.

**Input:**
```json
{
  "target": "... | all",
  "signals": ["array of signal type names, optional"],
  "confirm_token": "string, required on commit call"
}
```

**Flow:**
1. First call without `confirm_token`: returns preview.
```json
{
  "status": "ok",
  "data": {
    "preview": {
      "target": "repo:github/foo/bar",
      "api_calls_estimated": 14,
      "rate_limit_cost": {"github": 14, "github_remaining": 4986},
      "signals_to_collect": ["stars", "contributors", "commits", "..."]
    },
    "confirm_token": "ct_7a3f…",
    "expires_at": "…"
  },
  "metadata": {"requires_confirm": true, ...}
}
```
2. Client shows preview, user confirms.
3. Client re-invokes with `confirm_token`. Server executes, returns
   the refreshed signals.

### `signatory_show_analyses`

List ingested analyst outputs. Mirrors the CLI `signatory show-analyses`.

**Input:**
```json
{
  "target": "string, optional",
  "analyst_id": "string, optional",
  "since": "RFC3339 timestamp, optional",
  "limit": "int (default: 20)"
}
```

**Output:** array of `AnalystOutputSummary` records from the store.

### `signatory_show_findings`

Query findings across ingested outputs. Mirrors CLI.

**Input:**
```json
{
  "target": "string, optional",
  "analyst_id": "string, optional",
  "signal_type": "string, optional",
  "severity": ["positive", "informational", "low", "medium", "high", "critical"],
  "design_intent": "bool, optional",
  "limit": "int"
}
```

**Output:** array of `FindingSummary` from `store.ListFindings`.

### `signatory_show_methodology`

Query methodology patterns across ingested outputs. Mirrors CLI.

**Input:**
```json
{
  "target": "string, optional",
  "analyst_id": "string, optional",
  "signal_group": "string, optional",
  "hit_on_target": "hit | miss, optional",
  "limit": "int"
}
```

**Output:** array of `MethodologyPatternSummary`.

### `signatory_analyze_provenance`

Dispatch-subagent tool. Returns instructions for the client to
dispatch a provenance analyst subagent.

**Input:**
```json
{
  "target": "string",
  "intake_question": "string, optional",
  "refresh": "bool (default: false)"
}
```

**Output (`status: ok`, but `data.kind == \"dispatch_subagent\"`):**

```json
{
  "kind": "dispatch_subagent",
  "role": "provenance",
  "system_prompt": "...full rendered prompt from templates/handoffs/provenance-review-v1.md...",
  "user_prompt": "Signal bundle + intake question + target context",
  "recommended_model": "claude-sonnet-4-6",
  "allowed_tools": ["Read", "Grep", "Glob"],
  "output_path": "/tmp/signatory-dispatch-<uuid>.json",
  "output_schema": "v1.analyst-output",
  "on_complete": {
    "tool": "signatory_ingest",
    "params": {"file": "/tmp/signatory-dispatch-<uuid>.json"}
  }
}
```

`output_path` is a server-generated unique path to avoid collisions
across concurrent dispatches in the same session.

The client is expected to:
1. Dispatch a subagent with exactly `system_prompt`, `user_prompt`,
   `allowed_tools` (client enforces); model = `recommended_model`.
2. Wait for the subagent to produce JSON at `output_path`.
3. Invoke `signatory_ingest(file=output_path)` to persist.

If the client ignores `on_complete`, the user can always invoke
`signatory_ingest` manually.

### `signatory_analyze_security`

Same shape as `signatory_analyze_provenance`. Differences:

- `role: "security"`
- `recommended_model: "claude-opus-4-6"` (per the dual-analyst doc's
  novel-target routing rationale)
- `system_prompt` uses the security-review template
- `allowed_tools` is still `["Read", "Grep", "Glob"]` plus a
  scoped-path `Write` for the output file — no `Bash`, no network-
  capable tools. This is the v0.1 "sandbox by capability restriction."

### `signatory_analyze_full`

Composes provenance + security + (optional) synthesis. Returns a
sequence of dispatch-subagent directives.

**Input:**
```json
{
  "target": "string",
  "intake_question": "string, optional",
  "llm_synthesis": "bool (default: false)"
}
```

**Output:** a dispatch-chain directive — same shape as other dispatch
tools but with an `on_complete` that references the next leg:

```json
{
  "kind": "dispatch_chain",
  "steps": [
    {"role": "provenance", "..." },
    {"role": "security", "..." }
  ],
  "on_all_complete": {
    "tool": "signatory_synthesize",
    "params": {
      "target": "...",
      "llm_synthesis": true  // only when caller opted in
    }
  }
}
```

Clients that don't implement `dispatch_chain` can iterate: pick off
`steps[0]`, dispatch, ingest, then re-invoke `signatory_analyze_full`
to advance. The server is stateless across the chain (the next step is
derivable from what's already ingested).

### `signatory_ingest`

Accept a v1-schema analyst output file, validate, persist.

**Input:**
```json
{
  "file": "string — path to a JSON or markdown analyst output"
}
```

**Output (success):**
```json
{
  "output_id": "uuid",
  "entity_id": "uuid",
  "idempotent": false,
  "finding_count": 7,
  "positive_absence_count": 4,
  "observation_count": 1,
  "methodology_pattern_count": 14
}
```

**Output (validation failure):**
```json
{
  "status": "error",
  "error": {
    "code": "validation_failed",
    "message": "File <path> did not pass v1 schema validation.",
    "details": {
      "violations": [
        {"path": "findings[2].severity.default", "message": "must be one of ..."},
        {"path": "findings[2].citations", "message": "required"}
      ]
    }
  }
}
```

Per Q7/Q10: **don't-trust-and-verify.** Schema violation is a hard
error, not a warning. No partial ingest.

### `signatory_set_posture`

Record a posture decision. Two-call confirmation pattern.

**Input (preview call):**
```json
{
  "target": "string",
  "tier": "vetted_frozen | trusted_for_now | unexamined | unknown_provenance",
  "rationale": "string",
  "version": "string, optional"
}
```

**Output (preview):**
```json
{
  "status": "ok",
  "data": {
    "preview": {
      "entity": {"canonical_uri": "..."},
      "current_posture": {"tier": "unexamined", "set_at": "..."},
      "new_posture": {"tier": "trusted_for_now", "rationale": "..."},
      "version_scope": "all versions" | "specific: 4.18.2"
    },
    "confirm_token": "ct_…",
    "expires_at": "..."
  },
  "metadata": {"requires_confirm": true, ...}
}
```

**Input (commit call):**
```json
{
  "target": "...",
  "tier": "...",
  "rationale": "...",
  "version": "...",
  "confirm_token": "ct_…"
}
```

**Output (commit):**
```json
{
  "status": "ok",
  "data": {
    "entity_id": "...",
    "posture_id": "...",
    "previous_tier": "unexamined",
    "recorded_at": "..."
  }
}
```

Server validates `confirm_token`:
- Signed (HMAC) with a server-session key
- Bound to the exact preview content (hash of normalized preview JSON)
- Single-use (server tracks used tokens for TTL window)
- Not expired (default 5-minute TTL)

Any mismatch → `invalid_confirm_token` with `details.reason`.

### `signatory_burn`

Record a burn. Same two-call pattern as `signatory_set_posture`.

**Preview output includes blast radius:**
```json
{
  "preview": {
    "entity": {"canonical_uri": "..."},
    "blast_radius": [
      {"canonical_uri": "pkg:npm/foo", "relationship": "depends_on", "degree": 1},
      {"canonical_uri": "pkg:npm/bar", "relationship": "transitive_via:foo", "degree": 2}
    ],
    "reason": "..."
  },
  "confirm_token": "..."
}
```

Same commit-call shape as `signatory_set_posture`.

### `signatory_synthesize`

Produce a briefing from ingested analyst outputs.

**Input:**
```json
{
  "target": "string",
  "llm_synthesis": "bool (default: false)"
}
```

**Output — default (`llm_synthesis: false`):**

Deterministic Go-templating. Returns:
```json
{
  "kind": "briefing",
  "target": "...",
  "analyst_outputs_used": ["<output_id>", "<output_id>"],
  "markdown": "... rendered briefing, matching the design/analysis/<target>.md style ..."
}
```

**Output — opt-in (`llm_synthesis: true`):**

Dispatch-subagent directive (explicit higher-resource opt-in):
```json
{
  "kind": "dispatch_subagent",
  "role": "synthesist",
  "system_prompt": "...",
  "user_prompt": "... packaged analyst outputs ...",
  "recommended_model": "claude-opus-4-6",
  "allowed_tools": ["Read", "Write"],
  "output_path": "/tmp/signatory-dispatch-<uuid>.json",
  "output_schema": "v1.analyst-output",
  "on_complete": {
    "tool": "signatory_ingest",
    "params": {"file": "..."}
  }
}
```

The synthesist's output is itself a v1-schema analyst output with
`analyst_id: signatory-synthesist`, so it flows through the standard
ingest path.

## Resources

Resources use `resources/list` and `resources/read` per MCP spec. Each
resource URI returns a JSON document with the same envelope shape as
tool calls (sans `requires_confirm`).

### `signatory://posture`

```json
{
  "status": "ok",
  "data": {
    "total": 347,
    "by_tier": {"vetted_frozen": 8, "trusted_for_now": 91, "unexamined": 248},
    "oldest_posture": {"entity": "...", "set_at": "..."},
    "newest_posture": {"entity": "...", "set_at": "..."}
  }
}
```

### `signatory://burns`

Array of active burn records with source (local vs. inherited).

### `signatory://unexamined`

Dependencies without posture, sorted by criticality (highest first).
Intended for the "what haven't we looked at?" LLM prompt.

### `signatory://signal-types`

The signal-type registry per
`design/signal-storage-evolution.md`. Returns the full catalog.

### `signatory://analyses`

Query-parameter-driven: `signatory://analyses?target=<uri>` returns
analyst outputs for a target. Without params, returns a listing keyed
by entity.

### `signatory://config`

Effective server config (excluding secrets like any future API keys).
Useful for agents to understand the server's current capabilities:

```json
{
  "mcp_version": "0.1.0",
  "transport": "stdio",
  "direct_api_activated": false,
  "llm_synthesis_available": true,
  "db_path": "~/.signatory/signatory.db"
}
```

## Confirm-token shape

Tokens are opaque from the client's perspective but have a defined
server-side structure:

```
ct_<base64url(hmac_sha256(session_key, preview_json) + ttl_unix)>
```

Properties enforced by the server:
- Single-use: a bloom filter or map of used tokens within the TTL window
- TTL: default 5 minutes from issue
- Bound to preview: re-serializing preview must match the token's
  embedded hash
- Bound to tool: server checks the token was issued for the exact tool
  name the commit call uses (prevents replay across tools)

Tokens never leave the signatory-mcp process — the client treats them
as opaque strings.

## What's not specified in v0.1

- **Progress notifications.** MCP supports `notifications/progress` for
  long-running tool calls. v0.1 tools are fast enough that this isn't
  needed; v0.2+ may add them to `signatory_refresh` and the dispatch-
  chain tools.
- **Streaming tool responses.** v0.1 is request/response only. Same
  reasoning.
- **Content verification of ingested outputs.** Schema-only is the v0.1
  ceiling; citation liveness / commit-SHA consistency is v0.2.
- **Custom MCP capabilities.** The `initialize` handshake declares
  `tools` and `resources`. No custom capabilities.

## Cross-references

- [`design/mcp-server-architecture.md`](mcp-server-architecture.md) —
  the decisions this document implements
- [`design/mcp-interface.md`](mcp-interface.md) — the v0 tool philosophy
  and example agent conversation flow
- [`internal/exchange/`](../internal/exchange/) — the v1 analyst-output
  schema that `signatory_ingest` validates and dispatch-subagent outputs
  produce
- [`internal/store/store.go`](../internal/store/store.go) — the Store
  interface that backs the read-only tools
- [`internal/audit/`](../internal/audit/) — the audit sink every tool
  invocation records to
