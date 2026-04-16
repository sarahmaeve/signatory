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
check store → generate handoff prompts → dispatch analyst agents →
validate + ingest structured output → synthesize combined assessment
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
signatory show-analyses --target "$TARGET" 2>&1
```

If analyses exist, query the conclusions:

```bash
signatory show-conclusions --target "$TARGET" 2>&1
```

**If data exists**: present the existing analysis to the user. Ask
whether they want a fresh run (re-collect) or are satisfied with the
existing assessment. If satisfied, synthesize from existing data
(skip to Step 5). If re-collecting, proceed to Step 1.

**If no data**: tell the user "no existing analysis in the store"
and proceed to Step 1.

## Step 1 — Resolve the target

Parse $ARGUMENTS to determine:
- The canonical URI (e.g. `repo:github/komoot/photon`)
- The source forge (GitHub, GitLab, etc.)
- The ecosystem if applicable (Go, Rust, npm, Python)

For GitHub targets, resolve the default branch and HEAD commit:
```bash
gh api repos/{owner}/{repo} --jq '{default_branch, pushed_at}'
gh api repos/{owner}/{repo}/commits --jq '.[0].sha' | head -c 12
```

## Step 2 — Detect ecosystem + generate handoff prompts

### 2a. Detect language and ecosystem

The provenance handoff REQUIRES `--ecosystem`. Detect it:

```bash
gh api repos/{owner}/{repo} --jq '.language'
```

Map the result:
- Python → `--ecosystem pypi --language python`
- Go → `--ecosystem go --language go`
- Rust → `--ecosystem crates`
- JavaScript/TypeScript → `--ecosystem npm`
- Java/Kotlin → no ecosystem flag (Maven/Gradle not yet supported);
  pass `--ecosystem go` as a placeholder and note the gap

### 2b. Generate the handoff prompts

Always use `--force` to overwrite stale files from prior runs.

```bash
signatory handoff security "$TARGET" \
  --language "$LANGUAGE" --ecosystem "$ECOSYSTEM" \
  --force -o /tmp/signatory-handoff-security.md

signatory handoff provenance "$TARGET" \
  --ecosystem "$ECOSYSTEM" \
  --force -o /tmp/signatory-handoff-provenance.md
```

### 2c. Check for unfilled placeholders

The handoff command prints unfilled placeholders to stderr.
**These are acceptable and expected:**

- `INTAKE_QUESTION` — no intake question for automated runs. The
  analyst will operate without one; this is fine.
- `TARGET_PATH` — no local clone. The analysts can work from
  GitHub API + web fetches. If a local clone is needed, the
  analyst agent can clone it themselves.

**Do NOT stop to manually fill these.** Proceed to Step 3. The
analysts' handoff instructions handle the absent-placeholder case.

## Step 3 — Dispatch analyst agents IN PARALLEL

Spawn two agents in a single message (parallel dispatch). Each agent
receives the handoff prompt as its instructions and produces v1-schema
JSON as its output.

**IMPORTANT**: send BOTH Agent calls in ONE message so they run
concurrently. Do NOT wait for one to finish before starting the other.

```
Agent(security-analyst):
  prompt: |
    You are a security analyst. Follow the instructions in
    /tmp/signatory-handoff-security.md exactly.
    
    Your output is a v1-schema JSON file. Before writing it, read
    filestore/analysis/signatory-security-v1.json as a schema
    reference — your JSON must match this structure exactly.
    
    Write your output to:
      filestore/analysis/{target-name}-security-v1.json
    
    After writing, validate:
      signatory format-check filestore/analysis/{target-name}-security-v1.json
    
    If format-check fails, fix the JSON and re-validate.
    Do NOT proceed until format-check passes.
  allowed-tools: Bash Read Write Glob Grep WebFetch

Agent(provenance-analyst):
  prompt: |
    You are a provenance analyst. Follow the instructions in
    /tmp/signatory-handoff-provenance.md exactly.
    
    Your output is a v1-schema JSON file. Before writing it, read
    filestore/analysis/thefuck-provenance-v1.json as a schema
    reference — your JSON must match this structure exactly.
    
    Write your output to:
      filestore/analysis/{target-name}-provenance-v1.json
    
    After writing, validate:
      signatory format-check filestore/analysis/{target-name}-provenance-v1.json
    
    If format-check fails, fix the JSON and re-validate.
    Do NOT proceed until format-check passes.
  allowed-tools: Bash Read Write Glob Grep WebFetch
```

Wait for BOTH agents to complete before proceeding.

## Step 4 — Ingest into the signatory store

After both agents report success, ingest their output:

```bash
signatory ingest filestore/analysis/{target-name}-security-v1.json
signatory ingest filestore/analysis/{target-name}-provenance-v1.json
```

Verify both landed:

```bash
signatory show-analyses --target "$CANONICAL_URI"
```

You should see two entries (security + provenance). If either ingest
fails, check the format-check output from the agent — the JSON may
have a structural issue the agent didn't fully resolve.

## Step 5 — Dispatch synthesist agent

Generate the synthesis handoff prompt and dispatch a synthesist agent.
The synthesist reads the store (not source) and produces the combined
assessment. Its instructions come from the template, not from this
skill — the template IS the single source of truth for how to
synthesize.

```bash
signatory handoff synthesist "$TARGET" -o /tmp/signatory-handoff-synthesis.md
```

Read the generated prompt to verify it rendered correctly.

Then dispatch the synthesist:

```
Agent(synthesist):
  prompt: |
    You are a synthesist. Follow the instructions in
    /tmp/signatory-handoff-synthesis.md exactly.
    
    Your output is a narrative trust assessment (markdown), not
    v1-schema JSON. Present it directly — it will be shown to the
    user as the final pipeline output.
    
    IMPORTANT: Read ALL conclusions and positive absences from the
    store before writing anything. The posture tier goes at the TOP
    of your output, before the reasoning — commit first, justify
    second.
  allowed-tools: Bash Read Write Glob Grep
```

The synthesist's output is the human-readable assessment that makes
the pipeline's data meaningful. Present it to the user as the final
result.

## Step 6 — Record posture (with user confirmation)

**The decision is the user's.** Present the recommendation; do not
record it without confirmation.

If the user approves a posture:

```bash
signatory posture set --tier "$TIER" --rationale "$RATIONALE" "$CANONICAL_URI"
```

"Analysis only — no posture recorded" is a valid terminal state for
non-dependency targets.

## Important constraints

- **Do not skip Step 0.** The store may already have the answer.
  Re-collecting signals from GitHub's API is slow, rate-limited, and
  redundant if the data exists.

- **Do not merge the two analysts into one agent.** The dual-analyst
  pattern exists because security and provenance require different
  focus areas, different methodologies, and different blind spots.
  Merging them produces a generalist that's weaker at both.

- **Do not skip format-check.** Invalid JSON silently fails at ingest.
  The analyst agents MUST validate before reporting success.

- **Do not skip synthesis.** Raw conclusions without synthesis are
  data without interpretation. The synthesis is what makes the
  pipeline useful — it's the step where an LLM adds the value that
  a database query cannot.

- **The handoff IS the instructions.** Do not invent your own
  analyst instructions. `signatory handoff` generates role-specific
  prompts from templates that encode signatory's trust model. Those
  templates are the single source of truth for what each analyst
  should do.
