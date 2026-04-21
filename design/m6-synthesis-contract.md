# M6: Synthesis Contract — Decision Record

**Status:** Accepted 2026-04-21. Implementation milestone of
[`agent-facing-contract.md`](agent-facing-contract.md) §5 M6, expanded
to five sub-milestones (M6a–M6e) after DEEP design review. This doc
records the design decisions that shaped the implementation so a
future refactor can see *why* the shape is what it is, not just *what*
it is.

**Scope:** How the synthesist role lands its output in signatory's
store, how the synthesist handoff delivers structured evidence to the
subagent, and how a user promotes the synthesist's tier recommendation
into a recorded posture. Out of scope: everything the synthesist does
*with* its inputs — that's Layer 3 judgment, template-governed, not
contract-governed.

## 1. Motivation

The synthesist role is the only analyst role that never migrated to
the v1 schema / store-first model mandated by [v0.1 Invariant
3](v0.1-invariants.md#invariant-3-sqlite-is-the-canonical-store-scratch-files-are-not).
As of 2026-04-21 the synthesist still writes markdown to
`filestore/analysis/<name>-synthesis.md` — contradicting Invariant 3
directly — and its handoff tells it to shell out to `signatory
show-conclusions` etc. to gather its inputs, which drove the 16+
minute / 50+ tool-call failure documented in
[`agent-facing-contract.md`](agent-facing-contract.md) §1.

M6 closes that gap. The synthesist becomes:

- **Input-scoped** via a handoff body that carries the full
  structured evidence it needs (§3.5 of the parent contract).
- **Output-structured** via an extension to the v1 schema that
  captures the synthesis-specific shape (§3.6).
- **Store-authoritative**: its record lands in SQLite; the markdown
  file that used to be primary is demoted to a render-on-demand
  view.
- **Accept-promotable**: a new `signatory posture accept <output-id>`
  verb reads the synthesis's `proposed_posture` and writes a real
  posture row, closing the "synthesist recommended → user recorded"
  workflow to two commands.

## 2. The three shape options we considered

Before locking on Option C, we considered three schema shapes for
carrying the synthesis-specific data through the v1 schema.

**Option A — separate `SynthesisOutput` envelope.** New top-level
type parallel to `AnalystOutput`, new ingest method, new
`synthesis_outputs` table + child tables, new MCP tool.
Self-documenting; strongest type-level separation; ~500 LOC
extra over the other options. Cleanest authorial boundary (security
and provenance analysts literally cannot populate `proposed_posture`
because they don't emit a `SynthesisOutput`).

**Option B — flat fields on `AnalystOutput`.** Add ~8 optional
top-level fields (reasoning, summary, concordance_strengths,
contradictions, conclusion_weights, gaps, action_items,
proposed_posture) to the existing type. Most fields empty on
security/provenance outputs. Smallest code footprint but uglies the
schema for all consumers.

**Option C — `SynthesisSupplement` sub-struct on `AnalystOutput`.**
Add one optional field: `SynthesisSupplement *SynthesisSupplement`.
That struct carries the synthesis-specific payload. Validator gates
the field by `attribution.analyst_id` prefix. Storage is a single
JSON column plus two denormalized columns for query-path fields.

**Why Option C.** After reviewing an actual synthesis artifact
(`~/signatory-output/mod-synthesis.md`, 311 lines), the synthesis
shape shares near-zero structure with `AnalystOutput`: a synthesis
has no new conclusions of its own (it re-ranks and interprets
analysts' conclusions by reference), no positive absences (those are
inherited), no methodology trace (patterns live in the analyst
layer). Forcing synthesis data into `Conclusions[]` / `Observations[]`
would blur Layer-2 (what an analyst found) with Layer-3 (the
synthesist's integrated judgment).

So Option B is out — its top-level fields would be vestigial on every
non-synthesist row and the shape would drift over time.

Option A gives the cleanest boundary but costs ~5× more code for an
M6 that has four siblings in the queue. And Summary (M7) already
walks `analyst_outputs` for the cross-URI query; an Option-A
synthesis would need a parallel `synthesis_outputs` walk, or each
read surface would have to merge two sources.

Option C preserves Option A's trust-boundary guarantee at the
validator layer (present-iff-synthesist), reuses the existing ingest
transport, existing read paths (Summary, show-analyses) surface
syntheses without new code, and keeps the schema honest about what
each row carries via a named sub-struct. ~440 LOC for M6a vs. ~700
LOC for equivalent Option A scaffolding.

The one tradeoff: the trust boundary is validator-enforced, not
type-enforced. Strictly weaker than an envelope. Mitigated by three
independent enforcement layers (§4 below) and by the fact that the
validator runs on every ingest path.

## 3. The schema

All new types live in `internal/exchange/`:

```go
// AnalystOutput gains one optional field. All other existing fields
// are unchanged.
type AnalystOutput struct {
    // ... existing fields ...
    SynthesisSupplement *SynthesisSupplement `json:"synthesis_supplement,omitempty"`
}

// SynthesisSupplement carries the synthesis-specific payload that has
// no natural home in the conclusion/observation/absence model.
// Populated only by outputs whose attribution.analyst_id identifies
// a synthesist role (validator-enforced).
type SynthesisSupplement struct {
    ProposedPosture         ProposedPosture      `json:"proposed_posture"`
    Reasoning               string               `json:"reasoning"`
    Summary                 string               `json:"summary"`
    ConcordanceStrengths    []ConcordanceEntry   `json:"concordance_strengths,omitempty"`
    ContradictionsDetected  []ContradictionEntry `json:"contradictions_detected,omitempty"`
    KeyConclusionRefs       []ConclusionRef      `json:"key_conclusion_refs,omitempty"`
    Gaps                    []string             `json:"gaps,omitempty"`
    ActionItems             []string             `json:"action_items,omitempty"`
    Notes                   string               `json:"notes,omitempty"`
}

// ProposedPosture is the synthesist's tier recommendation.
// `posture accept <output-id>` promotes this into a real Posture row.
type ProposedPosture struct {
    Tier             string `json:"tier"`               // PostureTier value
    VersionScope     string `json:"version_scope,omitempty"` // pkg @V or empty
    RationaleSummary string `json:"rationale_summary"`  // one-paragraph
}

// ConcordanceEntry: where two or more analysts independently reached
// a compatible conclusion. HIGH-confidence evidence class per the
// synthesist's calibration notes.
type ConcordanceEntry struct {
    Topic         string   `json:"topic"`
    Description   string   `json:"description"`
    AnalystRefs   []string `json:"analyst_refs"`      // e.g. ["signatory-provenance", "external-sec-v1"]
    ConclusionIDs []string `json:"conclusion_ids,omitempty"` // F-IDs that support it
    Confidence    string   `json:"confidence"`         // HIGH/MEDIUM/LOW
}

// ContradictionEntry: where two analysts disagree. The synthesist
// must state a resolution preference — silent unresolved contradictions
// are a failure mode of Layer 3.
type ContradictionEntry struct {
    Topic                string   `json:"topic"`
    Description          string   `json:"description"`
    SupportingAnalystA   string   `json:"supporting_analyst_a"`
    SupportingAnalystB   string   `json:"supporting_analyst_b"`
    ConclusionIDsA       []string `json:"conclusion_ids_a,omitempty"`
    ConclusionIDsB       []string `json:"conclusion_ids_b,omitempty"`
    ResolutionPreference string   `json:"resolution_preference,omitempty"`
}

// ConclusionRef: a weighted reference to a conclusion in an analyst
// output. Used to produce the "Key Conclusions (ranked)" section
// without duplicating the conclusion body.
type ConclusionRef struct {
    OutputID          string `json:"output_id"`          // analyst_outputs.id
    ConclusionLocalID string `json:"conclusion_local_id"` // F001, etc.
    Weight            int    `json:"weight"`              // 1 = most-weight on posture
    ForgeryResistance string `json:"forgery_resistance"`  // VERY HIGH / HIGH / MEDIUM / LOW
    RelevanceNote     string `json:"relevance_note,omitempty"`
}
```

**Why `Notes` is at the supplement level only.** Per-entry `Note`
fields on `ConcordanceEntry` / `ContradictionEntry` / `ConclusionRef`
were considered and deferred (see §9). The top-level `Notes` is the
synthesist's escape hatch for commentary that doesn't fit
`Reasoning` / `Gaps` / `ActionItems`. It parallels
`AnalystOutput.RoundNotes` (which operates at round scope);
`SynthesisSupplement.Notes` operates at synthesis-body scope. Both
may be populated independently.

## 4. Trust-boundary enforcement

The guarantee we need: only outputs with `analyst_id` identifying a
synthesist role carry a `SynthesisSupplement` (and therefore a
`proposed_posture`). Three independent layers enforce it:

1. **`AnalystOutput.Validate()`** — the single chokepoint every ingest
   path goes through. Rule: `supplement present iff analyst_id has
   prefix "signatory-synthesis"`. A security-role output with a
   supplement fails schema validation with
   `CodeSchemaViolation`.
2. **`insertAnalystOutputRow`** — defensive re-check at the store
   boundary. Catches a validator-bypass bug before it corrupts
   storage.
3. **`posture accept`** — verifies `proposed_tier IS NOT NULL` AND
   analyst_id matches on read. Belt-and-suspenders for a hand-edited
   DB row.

The prefix test (`strings.HasPrefix(analyst_id, "signatory-synthesis")`)
is deliberately generous: `signatory-synthesis-v1`,
`signatory-synthesis-v2`, `signatory-synthesis-experimental` all
match. The synthesist role is a shipped-by-signatory concept; third
parties don't impersonate it, they produce their own analyst roles
under their own `analyst_id` values.

## 5. Storage

Migration v8 adds three columns to `analyst_outputs`:

```sql
ALTER TABLE analyst_outputs ADD COLUMN synthesis_supplement_json TEXT;
ALTER TABLE analyst_outputs ADD COLUMN proposed_tier TEXT;
ALTER TABLE analyst_outputs ADD COLUMN proposed_version_scope TEXT;
```

**Why a JSON blob + two denormalized columns, not full
normalization.** The normalization question reduces to: "is any field
ever queried in SQL?"

| Field | Query use | Storage |
| --- | --- | --- |
| `proposed_posture.tier` | `posture accept`; Summary | Denormalized column |
| `proposed_posture.version_scope` | `posture accept` needs it | Denormalized column |
| `proposed_posture.rationale_summary` | Read on accept; never filtered on | JSON |
| `reasoning`, `summary` | Display only | JSON |
| `concordance_strengths[]` | Display only | JSON |
| `contradictions_detected[]` | Display only | JSON |
| `key_conclusion_refs[]` | Display only | JSON |
| `gaps[]`, `action_items[]`, `notes` | Display only | JSON |

The two denormalized columns are a reversible decision. If any
JSON-side field ever needs SQL access, we migrate it out — either
promoting to a column or splitting into a child table. The JSON blob
default captures "this is display data" and stays honest about it.

**No CHECK constraints.** The invariant
(supplement-iff-synthesist-prefix) is enforced by `Validate()`;
adding SQLite CHECK constraints that introspect JSON would duplicate
the validator at the storage layer and couple the schema to the v1
analyst-id prefix convention. Same discipline as the rest of the
schema — the Citation either-lines-or-scope rule isn't a CHECK, it's
`Citation.validate`.

## 6. Sub-milestones

M6 lands as five commits. Each is independently green
(tests pass, lint clean) and independently reviewable. Ordering is
load-bearing: M6a unblocks M6b/c by providing the type; M6b unblocks
M6c by providing the evidence rollup; M6c unblocks M6e's skill
update.

### M6a — Schema

Ships the `SynthesisSupplement` type, its validator rules,
migration v8, and the ingest / read round-trip. No CLI surface
change, no MCP surface change. Only callers are the existing
ingest paths (CLI `ingest`, MCP `signatory_ingest_analysis`) picking
up the new optional field for free.

Tests (strict red-green TDD):

1. `exchange/validate`: positive (synthesist output with full
   supplement validates), negative (security output with supplement
   rejected; synthesist output without supplement rejected;
   synthesist output with empty `Reasoning` rejected; etc.).
2. `store/analyst_output`: full round-trip — `IngestAnalystOutput` a
   synthesis payload, `GetAnalystOutput` reconstructs it identically.
3. `store/analyst_output`: `GetSynthesisProposal(outputID)` returns
   the `ProposedPosture` for a synthesis output; returns `ErrNotFound`
   for a non-synthesis output.
4. `store/migrate`: migration v8 applies cleanly on a v7 schema.

Budget: ~440 LOC (~200 production, ~240 tests).

### M6b — Evidence assembler

Ships `internal/synthesis/evidence.go` (or equivalent): an
`EvidenceAssembler` that composes the full structured evidence for a
target URI. Walks `analyst_outputs` via the M2 cross-URI link,
fetches conclusions / positive_absences / observations in full (not
just counts like Summary), assembles them into a shape the
synthesist handoff template consumes.

Critically: this is a sibling to `internal/summary`, not an
extension. Summary's shallow contract is load-bearing for MCP
response compactness; making it conditionally deep would betray that
promise. The evidence assembler lives in its own package, shares
store reads via interface, and serves the M6c handoff-body assembly
path only.

Budget: ~400 LOC (~200 production, ~200 tests).

### M6c — Handoff template + D9 fence

Rewrites `templates/handoffs/synthesis-v1.md` around the inlined
evidence structure, the output skeleton (v1 JSON now, not markdown),
and the cross-pollination prohibition. New placeholders
(`{EVIDENCE_JSON}`, others as needed) filled by the handoff-assembly
code that feeds through M6b.

Evidence body format: JSON in fenced code blocks per §3.5 of the
parent contract. The synthesist reads deterministically-shaped JSON,
not pre-formatted prose.

Also extends the D9 cross-pollination fence to the security and
provenance templates — a ~4-line prohibition addition per file plus
a contract test asserting presence.

Budget: ~700 LOC (~300 production, ~400 tests). Biggest M6 commit.

### M6d — `posture accept <synthesis-id>`

New `PostureAcceptCmd` subcommand. Semantics (b+override per §7 Q4
of the design discussion):

- Loads the synthesis output by `output_id`.
- Validates: output exists, is a synthesis, has `proposed_tier`.
- Prints the proposal (tier / version_scope / rationale_summary /
  synthesis short-name).
- In TTY: prompts for confirmation unless `--yes` passed.
- In non-TTY: requires `--yes` (else errors out loudly — no silent
  no-op).
- Writes a Posture row with: tier from proposal, version from
  proposal, rationale = proposal rationale_summary (unless
  overridden), actor = caller, audit detail includes the source
  synthesis `output_id`.
- `--tier <value>` / `--rationale <text>` / `--rationale-file <path>`
  / `--version <V>` override the proposed values, logged as
  `deviation` in the audit detail so the audit trail shows "user
  accepted with tier override: rejected → trusted-for-now".
- `--dry-run` prints what would be written without writing.

Budget: ~400 LOC (~150 production, ~250 tests).

### M6e — Markdown render + /analyze skill update

Extends `signatory detail <output-id>` to render synthesis outputs
as markdown matching today's `mod-synthesis.md` layout. One `switch`
in the detail path; supplement-present outputs take the synthesis
renderer, others take the existing renderer.

Updates `.claude/skills/analyze/SKILL.md` Step 4 so the synthesist
agent dispatches with `signatory_ingest_analysis` (not `Write` to
filestore), instructs the agent to produce v1 JSON with the
supplement, and Step 5 suggests `signatory posture accept <output-id>`
with the newly-ingested synthesis.

Budget: ~250 LOC (~100 production, ~150 tests + skill diff).

**M6 total:** ~2190 LOC across the five sub-milestones (~950
production, ~1240 tests). Parent contract's §5 estimate was 500+600;
we're running ~1.7× that, mostly because M6b grew its own package
and M6d's override-mode tests are load-bearing for the trust
boundary.

## 7. What this explicitly does NOT do

- **Does not change `AnalystOutput` semantics for existing analyst
  roles.** Security and provenance outputs continue to validate and
  ingest exactly as they do today, with one additional rule: their
  `synthesis_supplement` must be `nil`. Pre-M6 outputs (all are)
  satisfy this by construction.
- **Does not change the `signatory_ingest_analysis` MCP tool's input
  schema.** The tool accepts whatever JSON validates as
  `AnalystOutput`. Adding an optional field preserves backward
  compatibility.
- **Does not migrate existing filestore markdown.** Per D2 of the
  parent contract (drop and re-collect). The existing
  `filestore/analysis/*.md` files remain as historical artifacts;
  they are not re-ingested. After M6e, new synthesist work lands in
  the store; `signatory detail` renders it on demand.
- **Does not add a new MCP write tool.** `posture accept` is CLI-only
  in v0.1, matching the M4 policy that mutations stay CLI-side.

## 8. Open questions

**OQ1: Does `Summary` surface `proposed_tier` as a first-class field?**
Today `Summary` reports per-analyst rollups with severity counts. A
synthesis shows up with `ConclusionsCount=0`, which is correct but
leaves the `proposed_tier` inside the JSON column. Adding
`AnalysisRollup.ProposedTier string` (empty for non-syntheses) is a
~10 LOC change to M7 that makes the "pending recommendation" state
visible at summary time. **Punted to M6e or immediately after.**

**OQ2: How does `signatory show-analyses` differentiate syntheses
from analyses visually?** Either a new column in the table render
(`TYPE: synthesis | analysis`), or a separate section. Cosmetic;
decide during M6e based on what the table looks like with real data.

**OQ3: Multi-synthesis behavior.** Nothing in the schema prevents a
target from having multiple synthesis outputs (same-round
re-syntheses; round 1 + round 2; different synthesists). `posture
accept` takes an explicit `output_id` so the user selects which one
to promote, not "the latest synthesis for this target." This feels
right for v0.1. Revisit if dogfood surfaces multi-synthesis as a
real workflow.

## 9. Dogfood-gated deferrals

The following choices are YAGNI'd until real usage surfaces a
concrete need:

- **Per-entry `Note string` on `ConcordanceEntry` /
  `ContradictionEntry` / `ConclusionRef`.** Top-level
  `SynthesisSupplement.Notes` is the current escape hatch. If a
  synthesis output shows a recurring pattern of "I wanted to gloss
  this specific concordance with a caveat," we add the field to just
  the struct(s) that need it.
- **Normalizing any JSON-side field into a column.** Reserved for
  when someone actually wants to query by it. Likely candidates if
  they emerge: `proposed_posture.rationale_summary` (if we want to
  surface it in Summary without JSON unmarshaling), `action_items[]`
  (if users ask for "show me all pending action items across my
  dependencies").
- **A dedicated synthesis MCP write tool.** The existing ingest tool
  handles syntheses fine. A dedicated tool would be motivated only
  by a significant divergence in shape or audit needs that the v1
  schema can't express.
- **CHECK constraints in migration v8.** Validator is the single
  guard; CHECK would duplicate it. Reconsider only if a non-Go
  caller (a future sync-tool that writes to SQLite directly)
  materializes.

## 10. Cross-references

- [`agent-facing-contract.md`](agent-facing-contract.md) — parent
  contract; §3.5 (synthesist inputs), §3.6 (synthesist output), D3
  (store-first), D9 (analyst fence), §5 (milestone order).
- [`v0.1-invariants.md`](v0.1-invariants.md) — Invariant 3 (SQLite
  canonical) and Invariant 4 (named transports); the
  synthesist-via-MCP-ingest move completes these invariants.
- [`trust-model.md`](trust-model.md) — Layer 2 / Layer 3
  distinction this contract preserves.
- `internal/exchange/` — where the types land.
- `internal/store/` — migration v8 + ingest/query changes.
- `internal/synthesis/` (new in M6b) — evidence assembler.
- `templates/handoffs/synthesis-v1.md` — rewritten in M6c.
- `.claude/skills/analyze/SKILL.md` — updated in M6e.
- MEMORY `feedback_analysis_serialization_split` — agents produce
  natural-language analysis; deterministic Go code handles the JSON
  serialization. M6c embodies this: the synthesist reasons; the
  `handoff synthesist` code composes the structured evidence block.
