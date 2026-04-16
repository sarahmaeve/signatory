# Signatory: MCP Interface Design

## Interaction Philosophy

The LLM is an analyst, not a decision-maker. Three boundaries govern
the human/LLM interaction:

1. **Resource expenditure** — the LLM asks permission before triggering
   network calls (API queries, signal collection). It should surface
   something like: "I'd recommend package X, but I don't have trust
   data on it. Should I investigate?" The user approves before any
   scanning begins.

2. **Investigation depth** — the LLM provides summaries by default.
   The user prompts for drill-down when they want more detail. The
   LLM does not dump full signal data unprompted.

3. **Trust decisions** — the LLM can recommend a posture tier or action,
   with a brief rationale explaining *why*. The decision is the user's.
   The LLM never sets posture, burns entities, or makes trust commitments
   autonomously.

This mirrors the analyst/principal relationship in intelligence work.
The analyst surfaces information and recommendations; the principal
makes the call.

## MCP Tools

### signatory_analyze

Retrieve or collect the trust profile for a package, repo, or identity.

**Input:**
- `target` (string) — package name, repo URL, or identity identifier
- `refresh` (boolean, default false) — whether to collect fresh signals
  from network sources

**Behavior:**
- If cached data exists and `refresh` is false: return cached profile
- If no cached data exists and `refresh` is false: return a response
  indicating no data is available, suggesting the user approve a scan
- If `refresh` is true: collect signals from network sources, update
  cache, return updated profile

**Returns:** Entity profile summary including:
- Entity type and identifier
- Temporal era classification
- Current posture tier (if set)
- Burn status (if any)
- Key signals grouped by category (vitality, governance, publication
  integrity, hygiene, criticality)
- Anomalies or notable conclusions highlighted
- Forgery resistance level for each signal

**Design note:** The LLM should use this tool with `refresh: false`
first. If no data exists, it tells the user and asks whether to
investigate. Only after user approval does it call with `refresh: true`.

### signatory_survey

Assess the trust posture of a project's dependency tree.

**Input:**
- `manifest_path` (string, optional) — path to package.json, go.mod,
  etc. Defaults to current directory.
- `refresh` (boolean, default false) — whether to refresh cached signals

**Behavior:**
- Parses the dependency manifest
- Returns posture overview for all dependencies
- Highlights: unexamined dependencies, degraded signals, burned entities,
  anomalies, criticality concentrations

**Returns:** Summary including:
- Total dependency count (direct + transitive)
- Breakdown by posture tier (vetted / trusted / unexamined / unknown)
- Top anomalies or concerns (sorted by criticality × severity)
- Dependencies with no cached data (candidates for investigation)

### signatory_set_posture

Record an organizational trust decision about a dependency.

**Input:**
- `target` (string) — entity identifier
- `tier` (enum) — vetted_frozen | trusted_for_now | unexamined | unknown_provenance
- `rationale` (string) — brief explanation of the decision
- `version` (string, optional) — specific version being attested

**Behavior:** Records the posture decision in the local database with
timestamp and rationale. For `vetted_frozen`, the version should be
specified.

**Design note:** The LLM should present a recommendation with rationale
and ask the user to confirm before calling this tool. Example:

> Based on the analysis, I'd suggest marking lodash@4.17.21 as
> "trusted-for-now" — it has strong vitality signals, verified
> maintainers, and no anomalies, but we haven't done a deep review.
> Should I record that?

### signatory_burn

Record a burn against an entity.

**Input:**
- `target` (string) — entity identifier
- `reason` (string) — explanation for the burn
- `scope` (enum, optional) — local | propagate_to_dependents

**Returns:** Confirmation + blast radius summary (what depends on this
entity and would inherit degraded trust).

**Design note:** Burns should always be confirmed by the user. The LLM
should present the blast radius *before* the user confirms.

### signatory_refresh

Update cached signals for an entity or the full dependency tree.

**Input:**
- `target` (string) — entity identifier, or "all" for full tree
- `signals` (array of strings, optional) — specific signal types to
  refresh. Default: all signals.

**Returns:** Delta — what changed since last scan. Only surfaces
differences, not the full profile.

### signatory_detail

Drill down into a specific signal group for an entity.

**Input:**
- `target` (string) — entity identifier
- `signal_group` (string) — vitality | governance | publication |
  hygiene | posture | criticality | history

**Returns:** Full signal detail for the requested group, including
raw values, sources, timestamps, and forgery resistance ratings.

**Design note:** This is what the LLM calls when the user says "tell
me more about the publication signals for axios." The summary from
`signatory_analyze` provides the overview; `signatory_detail` provides
the drill-down.

## MCP Resources

Static or slowly-changing data the LLM can read without tool calls:

### signatory://posture

Current dependency posture overview. How many dependencies are in each
tier, when posture was last updated.

### signatory://burns

Active burn list with reasons and sources (local vs. inherited).

### signatory://unexamined

List of dependencies with no posture decision — the "what haven't we
looked at?" view. Sorted by criticality (highest first).

### signatory://config

Current signatory configuration: cache TTLs, active ecosystem
providers, signal sources, temporal era boundaries.

## Example Interaction Flow

```
User: Let's add a markdown parser. How about marked?

LLM: [calls signatory_analyze(target="marked", refresh=false)]
     → no cached data

LLM: I'd suggest the "marked" package for markdown parsing, but I
     don't have trust data on it yet. Want me to investigate?

User: Yes, go ahead.

LLM: [calls signatory_analyze(target="marked", refresh=true)]
     → returns profile summary

LLM: Here's what I found on marked (npm):
     - Active project, last commit 3 weeks ago, 12 contributors
     - 6.2M weekly downloads — high criticality
     - Commits are not signed
     - Published via npm automation (no trusted publisher binding)
     - No anomalies in recent version history
     - Currently unexamined in our posture tracking

     I'd suggest marking it as "trusted-for-now" — it has strong
     vitality and adoption signals, but the lack of commit signing
     and trusted publishing means we're relying on npm account
     security. Want me to record that?

User: What about the publication signals specifically?

LLM: [calls signatory_detail(target="marked", signal_group="publication")]

LLM: Publication detail for marked:
     - Last 10 versions all published from the same npm account
     - No trusted publisher (OIDC) binding
     - All versions have corresponding git tags ✓
     - No new dependencies added in recent versions ✓
     - No dormant version branch activity ✓
     The main gap is the lack of trusted publishing — if the
     maintainer's npm credentials were compromised (like axios),
     there's no secondary verification.

User: OK, mark it as trusted-for-now.

LLM: [calls signatory_set_posture(target="marked",
      tier="trusted_for_now",
      rationale="Strong vitality and adoption, no anomalies,
      but lacks trusted publishing and commit signing")]

LLM: Recorded. marked is now tracked as trusted-for-now.
```

## Design Constraints

- **No autonomous network calls.** The LLM must surface the intent to
  scan and receive user approval before triggering `refresh: true` on
  any tool.

- **Summaries by default.** Tool responses should be concise enough for
  the LLM to relay without overwhelming the conversation. Full detail
  is available via `signatory_detail` when the user asks.

- **Recommendations, not decisions.** For any trust-modifying action
  (set_posture, burn), the LLM presents a recommendation with rationale
  and waits for user confirmation.

- **Offline-capable.** All tools work with cached data when available.
  The LLM should be able to answer questions about the dependency tree
  from cache without any network calls.
