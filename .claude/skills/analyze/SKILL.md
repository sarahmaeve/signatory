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

This skill orchestrates signatory's full analysis pipeline:

```
check store → prepare (sessions + handoffs + signals) →
dispatch analysts → verify landing → synthesist handoff →
dispatch synthesist → close pipeline
```

The target is specified as $ARGUMENTS — a GitHub/GitLab URL, package
coordinate (Go module, npm, PyPI, crates.io), or owner/repo shorthand.

This skill is NOT a monolithic analyst. It is an **orchestrator** that
dispatches specialist agents and synthesizes their output. The
specialists do the collection work; this skill manages the pipeline.

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

## Step 1 — Prepare the pipeline

A single command replaces the multi-step setup (certs preflight,
serve start, session create, analysis begin, handoff render + deposit,
git clone, and Layer-1 signal refresh). It returns a JSON manifest
with every variable the downstream steps need — the orchestrating
LLM never threads shell variables or parses stdout.

```bash
signatory serve status >/dev/null 2>&1 || signatory serve start

MANIFEST=$(signatory pipeline prepare "$TARGET" \
  --expected-analyst signatory-security-v1 \
  --expected-analyst signatory-provenance-v1 \
  --expected-analyst signatory-synthesis-v1)
echo "$MANIFEST"
```

The `prepare` command:
1. Preflight checks TLS certs (mkcert CA via NODE_EXTRA_CA_CERTS)
2. Resolves the target (URL → canonical URI, short name, clone URL)
3. Creates a pipeline session (transport layer)
4. Creates an analysis session (audit identity in the store)
5. Renders + deposits security and provenance handoffs
6. Clones the target repo
7. Refreshes Layer-1 signals against the clone

All outputs are in the JSON manifest:

| Field                 | Use                                          |
|-----------------------|----------------------------------------------|
| `session_id`          | Pipeline transport session (for WebFetch URLs)|
| `analysis_session_id` | Analysis audit identity (for ingest + verify) |
| `target`              | Original target URI (unresolved)              |
| `target_name`         | Short name (basename) of the target           |
| `clone_path`          | Path to the local clone                       |
| `handoffs_deposited`  | Which roles' handoffs were deposited          |
| `signals_refreshed`   | Whether Layer-1 signals were refreshed        |
| `status`              | `"ready"` if everything succeeded             |

If `prepare` fails, its stderr carries step-by-step diagnostics.
**Do not proceed if status is not `"ready"`.**

Extract the variables you need from the manifest. Every `$VARIABLE`
reference in subsequent steps comes from this JSON — no manual
derivation, no `basename`, no second Bash call to parse output.

## Step 2 — Dispatch analyst agents IN PARALLEL

Render the dispatch prompts deterministically:

```bash
DISPATCH=$(signatory pipeline dispatch-prompts \
  --session-id "$SESSION_ID" \
  --analysis-session-id "$ANALYSIS_SID" \
  --target "$TARGET" \
  --target-name "$TARGET_NAME" \
  --clone-path "$CLONE_PATH")
echo "$DISPATCH"
```

The result is a JSON object with a `prompts` map. Each entry
(`security`, `provenance`, `synthesist`) contains:
- `description`: one-line agent description
- `prompt`: the fully-rendered prompt body (all placeholders substituted)
- `allowed_tools`: space-separated tool names for the agent

Dispatch the `security` and `provenance` agents in ONE message so
they run concurrently. For each, call Agent() with the `description`,
`prompt`, and `allowed_tools` from the JSON.

**Save the `synthesist` entry for Step 4.** Do not dispatch it yet —
the synthesist's handoff (which it retrieves via WebFetch) can only
be generated after both analysts have landed their output.

Wait for BOTH agents to complete before proceeding.

## Step 3 — Verify both analysts landed their output

```bash
VERIFY=$(signatory pipeline verify "$ANALYSIS_SID")
echo "$VERIFY"
```

Parse the JSON result:

- `status: "ready_for_synthesis"` — both analysts landed. Proceed to
  Step 4.
- `status: "missing_analysts"` — the `missing` array names which
  analyst(s) didn't land output. Re-dispatch the missing role with
  explicit guidance to call signatory_ingest_analysis with `source`,
  `collected_from`, and `analysis_session_id` all set.

The `output_ids` map provides the output UUID for each landed analyst,
useful for diagnostics if needed.

## Step 4 — Synthesist handoff + dispatch

Generate the synthesis handoff and deposit it. The handoff body
carries the full structured evidence — every analyst conclusion,
positive absence, and observation — so the synthesist's entire input
arrives via WebFetch.

```bash
signatory handoff synthesist "$TARGET" \
  --analysis-session-id "$ANALYSIS_SID" \
  --deposit-to "$SESSION_ID"
```

If the handoff fails with "no entity matches" or "no non-synthesis
analyses to synthesize," the analysts from Step 2 didn't land output
in the store. Check Step 3's verify result.

Dispatch the synthesist using the `synthesist` entry from Step 2's
dispatch-prompts JSON. Same Agent() pattern: use the `description`,
`prompt`, and `allowed_tools` from the JSON.

## Step 5 — Close the pipeline

First, retrieve the synthesis proposal (dry run):

```bash
CLOSE=$(signatory pipeline close "$ANALYSIS_SID")
echo "$CLOSE"
```

Parse the JSON:

- `status: "proposal"` — synthesis landed. Present the `proposed_tier`
  to the user and ask for confirmation.
- Error — no synthesis output found in the session. The synthesist
  either failed silently or didn't call signatory_ingest_analysis.

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

- **Do not merge the two analysts into one agent.** The dual-analyst
  pattern exists because security and provenance require different
  focus areas, different methodologies, and different blind spots.
  Merging them produces a generalist that's weaker at both.

- **Trust the ingest-tool validator.** signatory_ingest_analysis
  runs the v1 schema validator before writing; an invalid payload
  returns CodeSchemaViolation naming the offending field. Agents
  should fix the JSON and retry in the same turn rather than
  dropping fields or writing markdown to a file.

- **Do not skip synthesis.** Raw conclusions without synthesis are
  data without interpretation. The synthesis is what makes the
  pipeline useful — it's the step where an LLM adds the value that
  a database query cannot.

- **The handoff IS the instructions.** Do not invent your own
  analyst instructions. `signatory handoff` generates role-specific
  prompts from templates that encode signatory's trust model. Those
  templates are the single source of truth for what each analyst
  should do.

- **The dispatch prompts ARE the agent parameters.** Do not hardcode
  agent prompts, descriptions, or allowed-tools in this skill.
  `signatory pipeline dispatch-prompts` renders them from versioned
  templates. Use the JSON output verbatim.
