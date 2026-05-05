# Moving orchestration from prose to deterministic code

Dogfood session `cef3c5ab` (classnames) surfaced two bugs where an
orchestrating LLM misinterpreted prose instructions in SKILL.md.
The immediate fixes added more prose. This document asks the structural
question: **what mechanical work should move to Go code so prose quality
stops being load-bearing?**

## The current cost structure

The /analyze SKILL.md is ~530 lines of orchestration instructions.
Roughly 80% of the orchestrator LLM's work is mechanical shell
scripting (capture stdout → thread variables → substitute into
prompts → check exit codes → parse prose). The remaining 20% is
actual intelligence: error diagnosis, user interaction, presenting
results. Every token spent on the 80% is a reliability risk — the
LLM can misthread a variable, misparse prose, or choose WebFetch
over Read — and none of it requires judgment.

## Candidates for deterministic code

### 1. `signatory pipeline prepare` — collapse Steps 0a–1b into one command

**Current state:** The orchestrator runs 5–7 sequential bash commands
across Steps 0a (certs check), 1a (serve start), 1b (session create),
1c (analysis begin), 1d (handoff security + clone), 1e (handoff
provenance), and 1b-refresh (analyze --refresh). Each produces stdout
that must be captured into a shell variable and threaded to later
commands. The LLM must derive `TARGET_NAME` via `basename`, pass
`$ANALYSIS_SID` to `--analysis-session-id`, and so on.

**Proposed:** A single deterministic command that accepts `$TARGET` and
returns a JSON manifest with every downstream variable pre-computed:

```bash
signatory pipeline prepare "$TARGET" \
  --expected-analyst signatory-security-v1 \
  --expected-analyst signatory-provenance-v1 \
  --expected-analyst signatory-synthesis-v1 \
  --clone-dir filestore/clones/
```

Internally it would:

1. Run `certs check` (fail fast on TLS misconfiguration)
2. Ensure pipeline service is running (`serve status || serve start`)
3. Create pipeline session → `session_id`
4. Create analysis session → `analysis_session_id`
5. Render + deposit security handoff (clone, precheck, deposit)
6. Render + deposit provenance handoff (reuse clone, deposit)
7. Run `analyze --refresh --path` (populate Layer-1 signals)
8. Return structured JSON:

```json
{
  "session_id": "...",
  "analysis_session_id": "...",
  "target": "pkg:npm/classnames",
  "target_name": "classnames",
  "target_url": "https://github.com/JedWatson/classnames",
  "clone_path": "filestore/clones/classnames",
  "handoffs_deposited": ["security", "provenance"],
  "signals_refreshed": true,
  "status": "ready"
}
```

**What this eliminates:**
- 5 stdout-capture-into-variable steps
- `TARGET_NAME` derivation in bash
- Session ID threading between commands
- Exit code branching at each sub-step (prepare fails atomically)
- Certs/serve preflight as separate orchestrator concerns

**What it doesn't eliminate:** The orchestrator still needs to read the
JSON and substitute values into dispatch prompts. But it substitutes
from one structured source instead of threading 5 shell variables.

**Estimated complexity:** Medium. Each sub-step already exists as a
standalone Go function. `prepare` composes them in sequence with
early-exit on error. The main new work is the JSON output format and
integration-testing the composed flow.

---

### 2. `signatory pipeline dispatch-prompts` — render Agent() prompts deterministically

**Current state:** SKILL.md contains two pseudo-code `Agent()` blocks
with `{SESSION_ID}`, `{TARGET}`, `{TARGET_NAME}`, and
`{ANALYSIS_SID}` placeholders. The orchestrator LLM must substitute
all four into each prompt before dispatching. This is the substitution
that the classnames synthesist got wrong (confused pipeline session ID
with analysis session ID).

**Proposed:** After `prepare` succeeds, a second command renders the
exact dispatch prompts:

```bash
signatory pipeline dispatch-prompts \
  --session-id "$SESSION_ID" \
  --analysis-session-id "$ANALYSIS_SID" \
  --target "$TARGET" \
  --target-name "$TARGET_NAME" \
  --clone-path "filestore/clones/$TARGET_NAME"
```

Returns JSON:

```json
{
  "security": {
    "description": "Security analyst for classnames",
    "prompt": "You are a security analyst...\n\nA full clone of the target is at: filestore/clones/classnames\n...",
    "subagent_type": "general-purpose"
  },
  "provenance": {
    "description": "Provenance analyst for classnames",
    "prompt": "You are a provenance analyst...\n...",
    "subagent_type": "general-purpose"
  },
  "synthesist": {
    "description": "Synthesist for classnames",
    "prompt": "You are the synthesist...\n\nIMPORTANT — two different session IDs...\n...",
    "subagent_type": "general-purpose"
  }
}
```

**What this eliminates:**
- All prompt-level variable substitution by the LLM
- The `{SESSION_ID}` vs `{ANALYSIS_SID}` confusion surface entirely
- The need for SKILL.md to carry inline prompt templates at all
- Prompt drift between SKILL.md and the actual dispatch

**What it doesn't eliminate:** The LLM still calls `Agent()` with the
rendered prompt. But it's copy-paste from JSON, not string
interpolation in its head.

**Variant:** If `prepare` already returns the full manifest, this
could be `signatory pipeline dispatch-prompts --from-manifest manifest.json`
which reads the prepare output and renders all three prompts. Zero
variables for the orchestrator.

**Estimated complexity:** Low-medium. The prompt templates are currently
inline in SKILL.md. Moving them to Go templates (or to
`templates/dispatch/`) is straightforward. The substitution logic is
identical to what `config.RenderTemplate` already does for handoffs.

---

### 3. `signatory pipeline verify` — replace Step 3 prose parsing

**Current state:** After analysts complete, the orchestrator runs
`signatory show-analyses "$TARGET"` and `signatory analysis show
"$ANALYSIS_SID"`, then parses prose stdout to determine whether both
analysts landed. If one is missing, the LLM must decide to
re-dispatch.

**Proposed:** A verification command that returns structured status:

```bash
signatory pipeline verify "$ANALYSIS_SID"
```

Returns JSON:

```json
{
  "status": "ready_for_synthesis",
  "expected": ["signatory-security-v1", "signatory-provenance-v1"],
  "landed": ["signatory-security-v1", "signatory-provenance-v1"],
  "missing": [],
  "output_ids": {
    "signatory-security-v1": "uuid-1",
    "signatory-provenance-v1": "uuid-2"
  }
}
```

Or on partial landing:

```json
{
  "status": "missing_analysts",
  "missing": ["signatory-provenance-v1"],
  "landed": ["signatory-security-v1"]
}
```

**What this eliminates:**
- Prose parsing of `show-analyses` and `analysis show` output
- The LLM's role in counting rows and matching analyst IDs
- Ambiguity about what "ready" means

**Estimated complexity:** Low. `analysis show --json` already computes
the expected/landed/missing rollup. `verify` is a thin wrapper that
reads the rollup and maps it to a status enum.

---

### 4. `signatory pipeline close` — replace Steps 5–6 variable threading

**Current state:** After synthesis, the orchestrator must:
1. Extract `output_id` from the synthesist agent's transcript (fragile)
2. Run `signatory posture accept "$OUTPUT_ID" --yes` (user confirmed)
3. Run `signatory analysis end "$ANALYSIS_SID" --status completed --synthesis-output-id "$OUTPUT_ID"`

The output_id extraction is the most fragile step — it depends on the
synthesist agent reporting it in its final message. If the agent
forgets, Step 5 stalls.

**Proposed:** Instead of extracting the output_id from the agent
transcript, query the store:

```bash
signatory pipeline close "$ANALYSIS_SID" --status completed
```

Internally:
1. Query `analyst_outputs WHERE analysis_session_id = $ANALYSIS_SID AND analyst_id LIKE 'signatory-synthesis%'`
2. Extract the synthesis output_id from the store (deterministic)
3. Present the proposed posture to stdout for user confirmation
4. On `--yes`: accept posture + close session atomically

**What this eliminates:**
- Output ID extraction from agent transcript entirely
- The requirement that the synthesist report its output_id
- Two separate commands (posture accept + analysis end) that must share the same output_id

**What it doesn't eliminate:** User confirmation is still interactive.
The `--yes` flag is for scripted use; interactive mode would present
the proposal and wait.

**Estimated complexity:** Low. The store query is one SQL join. The
posture accept and session close logic already exist.

---

### 5. Move dispatch prompt templates out of SKILL.md into `templates/dispatch/`

**Current state:** The agent dispatch prompts live inline in SKILL.md
as pseudo-code. They're part of the prose the orchestrator LLM reads.
Changes to the prompts require editing SKILL.md, and the prompts
compete with the orchestration instructions for the LLM's attention.

**Proposed:** Move dispatch prompts to `templates/dispatch/`:
- `templates/dispatch/security-dispatch-v1.md`
- `templates/dispatch/provenance-dispatch-v1.md`
- `templates/dispatch/synthesist-dispatch-v1.md`

`signatory pipeline dispatch-prompts` (proposal #2) loads these
templates and substitutes variables. SKILL.md references them by name
but never contains the prompt text. The handoff templates and dispatch
templates are then both managed the same way: versioned markdown files
with `{PLACEHOLDER}` substitution.

**What this eliminates:**
- Inline prompt templates in SKILL.md (~60 lines)
- Prompt/orchestration instruction interleaving
- The risk that editing a prompt accidentally breaks an orchestration instruction

**Estimated complexity:** Low. Same template mechanics as handoffs.

---

### 6. `--json` on every CLI command the orchestrator calls

**Current state:** Several commands emit prose-only output. The
orchestrator must parse sentences like "No entity matches TARGET" or
count table rows.

**Proposed:** Add `--json` to every command the SKILL.md orchestrator
calls:
- `signatory show-analyses --json` (may already exist)
- `signatory certs check --json` → `{"status": "ok"}` or `{"status": "error", "diagnostic": "..."}`
- `signatory serve status --json` → `{"running": true, "pid": 1234, "port": 21517}`
- `signatory analyze --refresh --json` → `{"signals_collected": 47, "errors": []}`
- `signatory posture accept --json` → `{"posture_id": "...", "tier": "...", "accepted": true}`

**What this eliminates:**
- All prose parsing by the orchestrator LLM
- Exit code interpretation (JSON carries status field)
- Ambiguity when CLI output format changes

**Estimated complexity:** Medium. Each command needs a JSON output path.
Some already have `--json`; others need it added.

---

## Sequencing

Rough dependency order and value per unit of effort:

| Priority | Proposal | Effort | Impact |
|----------|----------|--------|--------|
| 1 | `pipeline prepare` | Medium | Eliminates ~7 bash variable-capture steps and all session ID threading |
| 2 | `pipeline dispatch-prompts` | Low-med | Eliminates prompt substitution (the direct cause of the synthesis bug) |
| 3 | `pipeline verify` | Low | Eliminates prose parsing for analyst landing verification |
| 4 | `pipeline close` | Low | Eliminates fragile output_id extraction from agent transcript |
| 5 | `--json` everywhere | Medium | Eliminates all remaining prose parsing |
| 6 | `templates/dispatch/` | Low | Separates prompt content from orchestration logic |

Proposals 1+2 together would reduce SKILL.md to roughly:

```
Step 0: signatory pipeline prepare "$TARGET" → JSON manifest
Step 1: Read manifest, dispatch security + provenance agents (prompts from manifest)
Step 2: signatory pipeline verify "$ANALYSIS_SID"
Step 3: Render + deposit synthesis handoff, dispatch synthesist (prompt from manifest)
Step 4: signatory pipeline close "$ANALYSIS_SID" → present posture, get user confirmation
```

That's ~5 orchestrator decisions instead of ~20 bash commands with
variable threading. The LLM's job becomes "read JSON, dispatch agents,
present results" — which is what it's good at.

## What stays in the LLM's domain

- **Error diagnosis.** When `prepare` fails, the LLM reads the
  structured error and decides what to tell the user. A deterministic
  command can't diagnose "your GitHub token expired" from context.

- **User interaction.** "Re-collect or accept existing?" and "Accept
  this posture?" are genuine decisions that require the user's input
  and the LLM's presentation skills.

- **Re-dispatch decisions.** When an analyst fails, deciding whether
  to retry with amended instructions, skip, or abort is judgment work.

- **Agent dispatch itself.** Claude Code's Agent tool requires an LLM
  to call it. The dispatch prompt content can be deterministic; the
  act of dispatching cannot (today).

## Constraint: no LLM-client dependencies in the binary

Per v0.1 Invariant 1, the `signatory` binary does not import an LLM
client or depend on any AI SDK. The boundary is: Go code handles data,
transport, validation, and composition. The LLM handles dispatch,
interaction, and judgment. The proposals above respect this boundary —
`prepare` composes existing Go functions; `dispatch-prompts` renders
templates; neither calls an LLM.
