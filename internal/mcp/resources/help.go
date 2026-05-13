package resources

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/mcp"
)

// HelpResource serves signatory://help — the deep-orientation text
// for an LLM client connecting to the server.
//
// Role versus Instructions: the initialize handshake's `instructions`
// field (see internal/mcp/handshake.go serverInstructions) carries a
// short routing nudge on every session start — it is always in
// context. This resource carries the fuller orientation: a concept
// map, a question→tool lookup table, and failure-mode notes. It is
// read at the client's discretion, typically when the model decides
// it needs more context than the instructions gave.
//
// Both texts coexist by design. Instructions are costly to keep
// short; this resource is free to be long. Updating both is a
// coordination cost, but the alternative — cramming the full map
// into every session's context — is worse.
//
// Maintenance: when adding a new tool or resource, update BOTH the
// "question → tool" section below AND (if the top-level framing
// changes) the serverInstructions constant in handshake.go.
type HelpResource struct{}

// URIPattern returns the literal URI for this static resource.
func (r *HelpResource) URIPattern() string {
	return "signatory://help"
}

// Description summarises the resource for resources/list. Written as
// an explicit affordance so an LLM scanning the list on first
// connect is nudged to read this before picking tools.
func (r *HelpResource) Description() string {
	return "READ THIS FIRST on a fresh session to orient on what signatory is and which tools match which user questions. Contains a question→tool lookup, key-concept glossary (signals vs. conclusions, posture vs. burns), and failure-mode notes (e.g. NotFound means 'nothing in store', not 'tool broken')."
}

// Read returns the static help text. No store access needed.
func (r *HelpResource) Read(_ context.Context, _ string) *mcp.Response {
	return mcp.OK(helpPayload{Content: helpText})
}

// helpPayload wraps the help string in an object so the response
// envelope.data is a consistent shape with the other resources
// (which all return objects, not bare strings).
type helpPayload struct {
	Content string `json:"content"`
}

// helpText is the full orientation guide. Kept as a const so there's
// exactly one source of truth; edits here and in handshake.go's
// serverInstructions should stay aligned.
const helpText = `# signatory — MCP server orientation

signatory is a supply-chain trust analysis tool. The MCP surface
provides read-only access to a local store of trust analyses,
conclusions, postures, and burns produced by analyst agents.

## When to reach for signatory

When the user asks about any of:

- dependency safety / "is X safe to use?"
- supply-chain risk for a specific package or repo
- trust posture of a dep or repo
- conclusions, concerns, or positives recorded for a target
- what's been analysed / not yet analysed
- methodology: how signatory assesses things

prefer the signatory_* tools and signatory:// resources over grep,
file search, or web lookups. The store is authoritative for "what
signatory has assessed"; the filesystem and the web are not.

## Question → tool lookup

| User question shape | First-choice tool |
|---|---|
| "Is X safe?" / "What's the trust profile of X?" | signatory_analyze |
| "What are the raw signals for X?" | signatory_signals |
| "What does the vitality/hygiene/governance of X look like?" | signatory_detail |
| "What has signatory analyzed?" | signatory_show_analyses |
| "What conclusions exist for X?" / "Any concerns about X?" | signatory_show_conclusions |
| "What patterns does signatory look for?" | signatory_show_methodology |
| "What changed for X recently?" / "Show transitions since <time>" | signatory_deltas |
| "What's the posture distribution?" / "How many deps assessed?" | signatory://posture |
| "What's unexamined?" / "What haven't I vetted?" | signatory://unexamined |
| "What has signatory burned?" / "Anything marked compromised?" | signatory://burns |
| "What signal types exist?" | signatory://signal-types |
| "What's signatory's setup?" | signatory://config |
| "How is the whole project's dep tree?" | signatory_survey (v0.1: stubbed) |

## Key concepts

- **Analyses** are ingested from analyst agents. They are NOT live
  scans. If a target hasn't been analysed, signatory_analyze returns
  NotFound — that's a real answer ("nothing in the store"), not a
  failure.

- **Signals** are Layer 1 evidence records (what the collectors saw).
  **Conclusions** are Layer 2 reasoned interpretations the analyst built from
  signals. Use signatory_signals for signals, signatory_show_conclusions
  for conclusions.

- **Posture** is a human trust decision attached to an entity. Tiers:
  vetted-frozen > trusted-for-now > unexamined > unknown-provenance >
  rejected. An entity can have signals and conclusions without a posture.

- **"Posture" has two meanings — do not confuse them.**
  (1) A Layer-2 trust decision stored in the ` + "`postures`" + ` table: query
  via the signatory://posture resource (aggregate) or via
  signatory_analyze's entity.posture field (per-target). Contains
  tier, rationale, set_by, set_at.
  (2) A Layer-1 signal group named "posture" that collectors can emit
  (e.g., "has a vetted-frozen marker in README"): query via
  signatory_detail with signal_group="posture". Contains raw evidence.
  If the user asks "what's the posture of X?" they almost always mean
  (1). If you pick (2) and find no records, double-check by reading
  signatory://posture before telling the user "no posture recorded."

- **Burns** are hard-reject markers independent of posture. A burn
  means "do not use, regardless of other signals."

- **Methodology patterns** are the reusable detection recipes that
  produce conclusions. Filterable by automation hint (how grep-friendly)
  and by hit_on_target (did this pattern produce a conclusion on the
  current target).

## Failure modes the caller should distinguish

- NotFound from signatory_analyze / signatory_signals / signatory_detail:
  the target is not in the store. Tell the user; do not invent data.

- Empty array from signatory_show_analyses / show_conclusions /
  show_methodology: matched nothing under the given filters. Try
  broader filters or tell the user.

- CodeNotFound from signatory_survey: the v0.1 limitation, not a
  real failure. Fall back to calling signatory_analyze per-dep.

- SchemaViolation: the tool arguments don't match the schema. Fix the
  call, do not retry blindly.

## Workflow: "is X safe?" for a new target

signatory's MCP surface is read-only. Collection and analysis happen
outside MCP (via the vet-dependency skill or CLI tools), and their
results are ingested into the store for future queries.

Expected sequence:
1. Check signatory_analyze(target=X). If data exists → answer from it.
2. If NotFound → the target hasn't been assessed. Tell the user.
3. Offer to run the vet-dependency skill (or equivalent collection
   tooling) to produce a fresh analysis document.
4. After the skill completes, the output is ingested via
   ` + "`signatory ingest <file>`" + ` (CLI) → populates the store.
5. Future signatory_analyze(target=X) calls return the ingested data.

Do NOT skip step 1 and go straight to vet-dependency. The store may
already have the answer, and re-collecting signals from GitHub's API
is slow and rate-limited.

Do NOT ask signatory MCP to "collect" or "refresh" — it cannot. The
refresh=true parameter on signatory_analyze is stubbed in v0.1. When
you see NotFound, the action is "offer to run the /analyze skill,"
not "retry with different parameters."

The /analyze skill is the v0.1 automated pipeline: it generates
handoff prompts via signatory CLI, dispatches security + provenance
analyst agents in parallel, validates and ingests their v1-schema
JSON output, and synthesizes a combined assessment. It IS the bridge
between "nothing in the store" and "store has data."

/vet-dependency is the manual human-readable fallback — use it only
when the user explicitly asks for a narrative document, not as the
default collection path.

## Out of scope for signatory MCP (v0.1)

- Live network scans or signal collection. signatory queries a local store.
- Generating new analyses. v0.1 is read-only; write paths are Phase 2.
- Cross-ecosystem vulnerability databases (CVE, OSV). signatory tracks
  its own conclusions, not public advisories.

For the server's version and transport, read signatory://config.
`
