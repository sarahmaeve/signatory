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
	return "READ THIS FIRST on a fresh session to orient on what signatory is and which tools match which user questions. Contains a question→tool lookup, key-concept glossary (signals vs. findings, posture vs. burns), and failure-mode notes (e.g. NotFound means 'nothing in store', not 'tool broken')."
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
findings, postures, and burns produced by analyst agents.

## When to reach for signatory

When the user asks about any of:

- dependency safety / "is X safe to use?"
- supply-chain risk for a specific package or repo
- trust posture of a dep or repo
- findings, concerns, or positives recorded for a target
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
| "What findings exist for X?" / "Any concerns about X?" | signatory_show_findings |
| "What patterns does signatory look for?" | signatory_show_methodology |
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
  **Findings** are Layer 2 conclusions the analyst built from
  signals. Use signatory_signals for signals, signatory_show_findings
  for findings.

- **Posture** is a human trust decision attached to an entity. Tiers:
  vetted-frozen > trusted-for-now > unexamined > unknown-provenance >
  rejected. An entity can have signals and findings without a posture.

- **Burns** are hard-reject markers independent of posture. A burn
  means "do not use, regardless of other signals."

- **Methodology patterns** are the reusable detection recipes that
  produce findings. Filterable by automation hint (how grep-friendly)
  and by hit_on_target (did this pattern produce a finding on the
  current target).

## Failure modes the caller should distinguish

- NotFound from signatory_analyze / signatory_signals / signatory_detail:
  the target is not in the store. Tell the user; do not invent data.

- Empty array from signatory_show_analyses / show_findings /
  show_methodology: matched nothing under the given filters. Try
  broader filters or tell the user.

- CodeNotFound from signatory_survey: the v0.1 limitation, not a
  real failure. Fall back to calling signatory_analyze per-dep.

- SchemaViolation: the tool arguments don't match the schema. Fix the
  call, do not retry blindly.

## Out of scope for signatory

- Live network scans. signatory queries a local store.
- Generating new analyses. v0.1 is read-only; write paths are Phase 2.
- Cross-ecosystem vulnerability databases (CVE, OSV). signatory tracks
  its own findings, not public advisories.

For the server's version and transport, read signatory://config.
`
