---
name: analyze
description: >
  Run signatory's trust analysis pipeline for a target. Checks the
  signatory store first; if empty, dispatches specialist analyst agents
  using signatory handoff prompts (security + provenance), ingests their
  structured v1-schema JSON output, and synthesizes a combined assessment.
  This is the v0.1 automated pipeline that populates the signatory store
  and produces LLM-synthesized trust assessments. For manual human-readable
  analysis, use /vet-dependency instead.
allowed-tools: Bash Read Write Glob Grep Agent WebFetch
---

# Analyze — signatory trust analysis pipeline

This skill is a thin host adapter around `signatory pipeline run`,
the deterministic orchestrator that lives in Go. The Go state
machine drives every transition; this skill's job is to read the
JSON events the orchestrator emits, dispatch the agents it requests
via `Agent()`, and exec the `next_command` it returns when the
agents have ingested their output.

```
check store → pipeline run "$TARGET" → dispatch analyst agents →
pipeline run --resume <sid> → dispatch synthesist →
pipeline close <sid> → present proposal → pipeline close <sid> --yes
```

The target is specified as $ARGUMENTS — a GitHub/GitLab URL, package
coordinate (Go module, npm, PyPI, crates.io), or owner/repo shorthand.

## Step 0 — Check the signatory store

Before doing anything expensive, check whether signatory already has
data for this target.

```bash
signatory show-analyses "$TARGET" 2>&1
echo "exit: $?"
```

**Check the exit code before interpreting the output:**

- **Exit 0 + output shows analyses**: data exists. Query conclusions:
  ```bash
  signatory show-conclusions --target "$TARGET" 2>&1
  ```
  Present the existing analysis to the user. Ask whether they want a
  fresh run (re-collect) or are satisfied. If satisfied, stop.
  If re-collecting, proceed to Step 1.

- **Exit 0 + "BURNED"**: entity is burned. Tell the user that the
  entity is burned, show the reason, and stop.

- **Exit 0 + "No entity matches"**: no data in the store. Tell the
  user "no existing analysis" and proceed to Step 1.

- **Any non-zero exit code**: something is broken — a CLI syntax
  error, missing database, permissions problem, etc. **STOP.** Report
  the exact error output to the user. Do NOT proceed to Step 1.
  Common causes: wrong flag syntax (show-analyses uses a positional
  argument, not `--target`), missing `~/.signatory/signatory.db`
  (run `signatory init` first), or a corrupted database file.

## Step 1 — Start the pipeline

Make sure the pipeline service is running, then ask the orchestrator
to start a new analysis. The Go side composes everything that
doesn't require LLM judgment — certs preflight, session creation,
handoff render+deposit, signal refresh, dispatch-prompt rendering —
and emits one JSON event describing what the host (this skill) needs
to do next.

```bash
signatory serve status >/dev/null 2>&1 || signatory serve start

START=$(signatory pipeline run "$TARGET")
echo "$START"
```

The result is a JSON object. Parse the `phase` field:

- **`"analysts_dispatch_required"`** — proceed to Step 2.
- Anything else (or a non-zero exit) — read the error output, report
  to the user, stop. The orchestrator's stderr carries step-by-step
  diagnostics; common causes are TLS misconfiguration (run
  `signatory certs init --write-profile`) and an unresolvable target.

The relevant fields in the start-phase event:

| Field                 | Use                                          |
|-----------------------|----------------------------------------------|
| `phase`               | Always `"analysts_dispatch_required"` here    |
| `analysis_session_id` | Threaded into `next_command` for resume       |
| `dispatches[]`        | Two entries: `security` and `provenance`      |
| `next_command`        | Argv to run after both analysts have ingested |
| `instructions`        | Human-readable guidance (echo to user if useful) |

## Step 2 — Dispatch analyst agents IN PARALLEL

Each entry in `dispatches[]` carries the three fields `Agent()`
needs. Iterate the array and dispatch all entries in **one message**
so they run concurrently:

For each `d` in `dispatches[]`, call `Agent()` with:
- `description`: `d.description`
- `prompt`: `d.prompt` (already fully substituted by the orchestrator)
- `subagent_type`: `general-purpose`
- `allowed_tools`: split `d.allowed_tools` on whitespace

Wait for ALL agents in this batch to complete before proceeding.

## Step 3 — Resume the pipeline

Run the orchestrator's `next_command` from Step 1. It verifies
analyst landing, renders + deposits the synthesis handoff, and
emits the synthesist dispatch prompt — all deterministically.

```bash
RESUME=$(signatory pipeline run --resume "$ANALYSIS_SID")
echo "$RESUME"
```

Parse the `phase` field:

- **`"missing_analysts"`** — one or more analysts didn't ingest. The
  `missing` array names which roles. Re-dispatch the named role(s)
  with explicit guidance to call `signatory_ingest_analysis` (with
  `source`, `collected_from`, and `analysis_session_id` all set),
  then re-run `signatory pipeline run --resume "$ANALYSIS_SID"`.

- **`"synthesist_dispatch_required"`** — both analysts landed; the
  synthesis handoff has been deposited. Proceed to Step 4. The
  `dispatches[]` array contains a single entry for the synthesist;
  `next_command` points at `pipeline close`.

## Step 4 — Dispatch the synthesist

Same pattern as Step 2, but with the single synthesist entry from
`dispatches[]`. Wait for it to complete (it calls
`signatory_ingest_analysis` to land its synthesis output) before
proceeding.

## Step 5 — Close the pipeline

Run the orchestrator's `next_command` from Step 3 to retrieve the
synthesis proposal:

```bash
CLOSE=$(signatory pipeline close "$ANALYSIS_SID")
echo "$CLOSE"
```

Parse the JSON:

- `status: "proposal"` — synthesis landed. Present the `proposed_tier`
  to the user and ask for confirmation.
- Error — no synthesis output found in the session. The synthesist
  either failed silently or didn't call `signatory_ingest_analysis`.

When the user approves:

```bash
signatory pipeline close "$ANALYSIS_SID" --yes
```

Returns `status: "closed"` and `posture_accepted: true`. This
accepts the proposed posture and closes the analysis session
atomically.

Alternative terminal statuses when things went sideways:

```bash
# Analyst never ingested and couldn't be re-dispatched
signatory pipeline close "$ANALYSIS_SID" --status failed --yes

# Partial result accepted by user
signatory pipeline close "$ANALYSIS_SID" --status partial --yes
```

"Analysis only — no posture recorded" is a valid terminal state for
non-dependency targets. Use `signatory analysis end "$ANALYSIS_SID"
--status completed` to close without posture.

## Important constraints

- **Do not skip Step 0.** The store may already have the answer.
  Re-collecting signals from GitHub's API is slow, rate-limited, and
  redundant if the data exists.

- **Do not invent your own orchestration.** `signatory pipeline run`
  is the orchestrator. This skill's job is to dispatch agents and
  exec the `next_command` the orchestrator returns. If you find
  yourself reaching for `signatory handoff`, `signatory pipeline
  prepare`, `signatory pipeline dispatch-prompts`, or `signatory
  pipeline verify` directly, stop — those are the building blocks
  `pipeline run` already composes.

- **The dispatches array IS the agent parameters.** Don't hardcode
  prompts, descriptions, or allowed-tools. Read them from the JSON
  event verbatim. Drift between this skill and the templates is
  exactly what the orchestrator is designed to prevent.

- **Do not merge the two analysts into one agent.** The orchestrator
  emits two dispatches because security and provenance require
  different focus areas, methodologies, and blind spots. Merging
  them produces a generalist that's weaker at both.

- **Trust the ingest-tool validator.** `signatory_ingest_analysis`
  runs the v1 schema validator before writing; an invalid payload
  returns `CodeSchemaViolation` naming the offending field. Agents
  should fix the JSON and retry in the same turn rather than
  dropping fields or writing markdown to a file.

- **Do not skip synthesis.** Raw conclusions without synthesis are
  data without interpretation. The synthesist is what makes the
  pipeline useful — it's the step where an LLM adds the value that
  a database query cannot.
