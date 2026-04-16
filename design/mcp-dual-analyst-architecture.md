# Signatory: Dual-Analyst Architecture for `signatory analyze`

## Status

Draft — captures the architectural pattern that emerged from applying
the trust model and a complementary security-focused code review to
atuin on 2026-04-14. Pending implementation in the MCP server.

Cross-references:
- `design/analysis/atuin.md` — the worked example that drove this
- `design/analysis/atuin-security-review-external.md` — the external
  security review that established the complementarity
- `design/signal-storage-evolution.md` — the signal types this
  architecture needs to produce and store
- `design/mcp-interface.md` — the existing MCP surface this extends

## Motivation

During a single session analyzing atuin, three distinct passes each
surfaced signal classes the others systematically missed:

| Pass | What it saw |
|------|-------------|
| API metadata (signatory skill, pass 1) | Stars, commit dates, contributor snapshot, workflow inventory, community health %, release cadence |
| Local clone (signatory skill, pass 2) | Commit signing distribution, tag types, `.mailmap` identity graph, lockfile composition, multi-year author distribution, CI supply-chain-gate absence |
| Security code review (external agent) | Hardcoded phone-home endpoints, unauthenticated TCP listeners, default-on risky features, enumerable AI tool surface, file-permission hygiene gaps, secrets-filter coverage gaps, shell-allowlist-not-sandbox composition, predictable temp-file paths |

The security-review pass found ten concrete, user-affecting conclusions
by *reading the source*. The signatory passes found an equally
important set by *reading metadata and git history*. Neither pass
was lazy; each was complete within its data grounding. The overlap
was modest — most conclusions appeared in only one of the three passes.

The trust analysis improved materially with all three. Trying to
collapse them into a single "mega-analyze" agent prompt would water
all three down: the provenance framework would lose precision against
novel threat shapes, the security pass would lose code-path-citation
discipline, and both would become expensive.

**This document proposes that `signatory analyze` ship as a
multi-layer system with two specialized analyst roles, deterministic
collectors underneath, optional synthesis on top, and a depth
parameter that lets callers pick scope.**

## The Three Axes

The three passes differed on three orthogonal axes. Keeping this
framing explicit makes the architecture legible.

### Axis 1: Data grounding

Where does the analysis look?

| Grounding | Examples |
|-----------|----------|
| Upstream metadata | GitHub / GitLab API, registry APIs (npm, PyPI, crates.io), workflow YAML returned as content |
| Local clone, surface | `go.mod`, `Cargo.toml`, `deny.toml`, `.mailmap`, `CHANGELOG`, `CONTRIBUTING`, `SECURITY` |
| Local clone, git history | Commit distribution, authorship by period, tag object types, signature status per commit |
| Local clone, source code | Function-level behaviors: hardcoded URLs, listeners, permissions, IPC shape, tool surface |

Signals cluster by grounding. A collector that reads the GitHub API
cannot surface "file permissions inherit umask" — that's not in the
API's projection. A collector that greps source code cannot surface
"build provenance attestation present" — that's in the workflow YAML
and the release job's `id-token: write` permission, not the `.rs`
files.

### Axis 2: Reasoning mode

How does the analysis decide what to look at?

| Mode | What it does |
|------|--------------|
| Framework-applied | "Check these N signal types against the target, emit values." Deterministic or near-deterministic. Uses the signal registry. |
| Generative-skeptical | "What could go wrong with this shape of tool? Go look." Requires understanding threat models, reading for intent, noticing anomalies. |

Framework-applied covers the known signal space well and is what
signatory's existing collectors do. Generative-skeptical catches
novel threat shapes — the update-check phone-home was discoverable
only because the security reviewer asked "what hardcoded endpoints
are in the HTTP client?" — a grep, but one only prompted by a
suspicious framing.

These modes don't blend cleanly in one prompt. A framework-applied
agent asked to also "think about novel threats" tends to either
rubber-stamp the framework or hallucinate novel issues. A
generative-skeptical agent asked to also "fill in the framework"
tends to skip framework cells while fixating on juicy conclusions.

### Axis 3: Output shape

What comes out?

| Output shape | Consumer |
|--------------|----------|
| Signal table + role + posture | Humans making trust decisions; downstream scoring |
| Code-path-cited threat list + defensive actions | Operators hardening a deployment; humans writing permission policies |
| Unified narrative + action plan | Decision-makers wanting one document |

Outputs have different readers. Signal tables are scanned for
anomalies. Threat lists are executed against. Unified narratives
are read once to make a decision.

## Proposed Architecture

Four layers, bottom-up.

### Layer 1: Deterministic collectors (no LLM)

Every signal that can be extracted mechanically should be. This is
the largest layer and the one that moves the fastest toward cheap
reproducible analysis.

**Existing or in-flight:**
- GitHub API signals (`internal/signal/github/`)
- npm registry signals (per `design/npm-plan.txt`)

**New collectors this architecture demands:**
- `internal/signal/git/` — local-clone git operations: commit
  signing distribution, tag object types, per-author commit counts
  by time window, `.mailmap` identity graph, first-commit date.
- `internal/signal/source/` — grep-class source-reading signals:
  hardcoded URL extraction, listener discovery (TcpListener,
  UnixListener, named pipes) + auth-middleware detection, file
  permission calls, temp-file path generation patterns, unsafe-code
  attribute presence per crate/module.
- `internal/signal/policy/` — policy-file parsing: `deny.toml`,
  `.cargo/audit.toml`, `govulncheck.yaml`, `.github/dependabot.yml`,
  CI workflow structural analysis (does it invoke cargo-deny? does
  it use `id-token: write`? what actions are pinned how?).
- `internal/signal/default/` — default-value extraction from config
  code / setup flows. For known patterns (Rust `#[serde(default = …)]`,
  Go `flag.String("x", "default", …)`, interactive prompts with
  default `Y`), emit the default-stance signal.

Every one of these is a Go module with table-driven tests. None
requires an LLM. Each populates signals per
`design/signal-storage-evolution.md` into the append-only `signals`
table with appropriate `details` JSON and optionally
`signal_evidence` rows.

**The design principle:** *if we found the signal in the atuin
analysis by running a command or reading a file, it belongs as a
collector.* Only signals requiring judgment stay in LLM territory.

### Layer 2: Two analyst roles (LLM)

Each is an MCP tool with a distinct system prompt, output schema, and
target model.

#### `signatory://analyze/provenance`

- **System prompt:** encodes the signatory trust model
  (`design/trust-model.md`), the signal set
  (`design/signals-v01.md`), the role taxonomy
  (`design/analysis/README.md` + expanded set in
  `design/signal-storage-evolution.md`), the temporal era framing,
  and the forgery-resistance hierarchy.
- **Input:** the signal bundle for the target (all collectors that
  apply, with signal values + `details` + optional evidence
  references).
- **Output schema:** structured document matching the shape used by
  `design/analysis/atuin.md` and `design/dogfood/testify.md`. Signal
  table, role tagging, trust-model-per-group assessment, gaps,
  risks, posture recommendation + rationale, action items.
- **Target model:** Sonnet-class should be sufficient for most
  targets once the system prompt is tight. Opus only when a target
  is flagged novel (see §"Novel-target routing" below).
- **Typical runtime:** seconds for cached signal bundles; tens of
  seconds including collection.

#### `signatory://analyze/security`

- **System prompt:** encodes a skeptical threat-modeling frame.
  "Read this code. Assume it will be compromised somehow. What can
  an attacker do if they control a release? A PR? An LLM response
  to a user's prompt? What's hardcoded that looks configurable?
  What's gated by user attention rather than by types? Cite
  file:line for every conclusion."
- **Input:** a local clone path (or a sandboxed read-only view) +
  the signal bundle from Layer 1 for context.
- **Output schema:** threat-list with file:line citations, defensive
  actions, clear severity framing. Shape matches
  `design/analysis/atuin-security-review-external.md`.
- **Target model:** Opus for now. The skeptical-threat-modeling
  capability isn't reliably replicable at smaller sizes yet. Revisit
  as Haiku / Sonnet capabilities improve.
- **Typical runtime:** longer. Reads source, which is expensive.

These are genuinely different tools. Their outputs can be consumed
independently. A caller who only needs provenance pays only for
provenance.

### Layer 3: Synthesist (optional)

- **System prompt:** integrate-and-reconcile. Given a provenance
  analysis output and a security analysis output, produce a unified
  decision document: what both agreed on, what each uniquely
  surfaced, the consolidated action list, the final posture
  recommendation.
- **Input:** both Layer 2 outputs + the raw signal bundle (for
  tiebreaking).
- **Output schema:** unified document. The shape of our
  `atuin.md` after the full three-pass integration is the template.
- **Target model:** Opus, when invoked. This is the high-judgment
  integration step.
- **Optional:** only runs when the caller asks for it. Not every
  analysis needs it — if the analysts agree on everything, the
  consumer can read both outputs side-by-side.

### Layer 4: Caller orchestration (CLI + MCP)

The caller picks depth. See next section.

## The Caller-Chooses-Depth Model

The architecture above enables different analysis scopes at different
costs. The caller — a human at the CLI, an LLM calling the MCP, or a
CI pipeline — picks which layers run.

### CLI surface (proposed)

```
signatory analyze <target>
signatory analyze <target> --depth=provenance   # default
signatory analyze <target> --depth=security
signatory analyze <target> --depth=deep         # provenance + security (both outputs, no synthesis)
signatory analyze <target> --depth=full         # provenance + security + synthesis
signatory analyze <target> --depth=signals      # Layer 1 only, no LLM

# Composable flags for specific behaviors
signatory analyze <target> --depth=security --local-path=~/git/atuin
signatory analyze <target> --depth=provenance --refresh      # re-collect signals, bypass cache
signatory analyze <target> --depth=full --model=sonnet       # override default model
```

Default behavior (`signatory analyze <target>` with no depth flag) is
**provenance**. This is the fast common case: "I'm adding a Go
library, give me the trust-model assessment." Seconds with cached
signals.

### MCP surface (proposed)

Analogous endpoints:

```
signatory://analyze                  # default (provenance)
signatory://analyze/provenance
signatory://analyze/security
signatory://analyze/deep             # both, no synthesis
signatory://analyze/full             # both + synthesis
signatory://signals                  # Layer 1 only
signatory://signal-types             # registry, already in design
```

Each endpoint returns a consistent envelope with `signals`,
`analysis`, and `metadata` (model used, timing, cache hits) so an
agent can reason about cost and freshness.

### Depth as scope, not just as spend

Different tasks legitimately want different depths:

| Task | Natural depth |
|------|---------------|
| "I'm adding a Go lib to my project" | `provenance` — fast, covers adoption decision |
| "Should our team run atuin on dev machines?" | `full` or `deep` — capture behavior matters |
| "Give me raw signals so I can write a custom rule" | `signals` |
| "Is this known-good maintainer still active?" | `provenance` with a dated range query |
| "We got a vuln report against dep X — what's the threat surface?" | `security` |
| "Formal vetting for vetted-frozen posture" | `full` |

A dependency-addition vet shouldn't trigger an Opus-grade security
review of every package in a 200-dep graph. A pre-adoption
evaluation of a tool that runs LLM-generated commands should.

### Caching across depths

Signals are already append-only (per `design/entity-model-v2.md`).
This architecture relies on that: `security` mode running after
`provenance` mode reuses the collected signals and only pays the
LLM cost for the security analyst. A second caller running `full`
on the same target reuses both analysts' cached outputs if they
haven't expired, paying only for synthesis.

Cache keys: `(target_canonical_uri, collector_version, signal_type)`
for Layer 1; `(target_canonical_uri, analyst_role, model, prompt_version)`
for Layers 2–3. Invalidate on `--refresh`.

## Novel-target routing

Not every target needs Opus-grade analysis. The trust model itself
tells us which do.

### Signals that indicate "route to big model"

- `ai_agent_runtime_capability` — the target can execute
  LLM-generated commands. Always deep.
- `unbypassable_hosted_callback` — hardcoded phone-home endpoints.
  Usually deep.
- New role categories (shell-augment, user-input-capture,
  hosted-service-coupled). Often deep.
- Crate/package count growing fast in the last 30 days combined
  with signature gaps. Sometimes deep.
- External security advisory filed against the target or a parent
  in the last 90 days. Always deep.
- Trust posture being *elevated* (trusted-for-now → vetted-frozen).
  Always deep — the whole point of vetted-frozen is that we looked
  hard.

### Signals that indicate "cheap model is fine"

- Known-mature library in a mature ecosystem (years-stable crate
  from an org we've analyzed before).
- Second or later analysis of the same target within the cache
  window.
- Target is a transitive with low `effective_maintainer_concentration`
  risk and strong identity-graph depth.

The routing itself can be a small deterministic decision — no LLM
needed. The provenance analyst can refuse to run (and return a flag
recommending `--depth=deep`) if its own signal check indicates novel
shape. This makes the CLI experience honest: you ask for
provenance, get either an answer or "this target looks like it
needs the deeper pass, here's why."

## What this architecture doesn't promise

- **It doesn't make security review fully automatic for all
  targets.** The security analyst is an Opus-grade cost and doesn't
  run by default. For targets outside the novel-routing criteria,
  the security pass is opt-in.
- **It doesn't eliminate human review.** A reviewer writing a
  `vetted-frozen` attestation should still read the output and the
  source. This tool gets them to 90%, not 100%.
- **It doesn't replace `cargo-audit` / `govulncheck` /
  OSV-Scanner.** Those remain ground-truth vulnerability data
  sources; signatory consumes their output as signals rather than
  duplicating their function.
- **It doesn't guarantee signal completeness.** New signal types
  will keep emerging as analyses surface them (the atuin session
  alone added 21 signal types). The registry is expected to grow.

## Open questions

1. **Security analyst sandboxing.** The security agent needs to
   read source — ideally in a sandbox that prevents it from executing
   code or making network calls. Where does that sandbox live?
   Ephemeral container per analysis? Chrooted worktree? Firecracker
   VM? This matters more than usual because we're inviting a model
   to read code that may be hostile.
2. **Evidence propagation.** When the security analyst cites
   `stream.rs:292-311`, should the evidence blob for that citation be
   captured and stored in `signal_evidence` automatically? Or only on
   explicit request? Auto-capture bloats the DB; on-request risks
   losing the evidence to future source changes.
3. **Synthesist-analyst feedback.** When the synthesist notices that
   the two analysts contradict each other, what does it return? A
   flag for re-analysis? A "pending resolution" state (per
   `entity-model-v2.md` §"Conflict Resolution Model")? An automatic
   "investigate" action?
4. **Cost accounting per caller.** The MCP should probably expose
   per-analysis token/time cost in the response envelope so callers
   can reason about spend. Format TBD.
5. **Model choice as a first-class signal.** If a provenance analysis
   was produced by Haiku and another by Opus, consumers probably want
   to know — analyses from different-capability models are different
   signals. Record in the analysis metadata.
6. **When does the security analyst itself need a signal-registry
   entry?** If we start signing its outputs, they become a form of
   signed attestation — probably highest-tier if the team identity
   is strong (per trust-model.md on signed organizational
   attestation).

## Immediate implications for in-flight work

1. The npm provider + collector currently in design
   (`design/npm-plan.txt`) should produce signals cleanly consumable
   by both analysts. No changes needed to that plan; it slots into
   Layer 1.
2. The `signals.details` JSON column proposed in
   `design/signal-storage-evolution.md` is required for this
   architecture. Security-analyst conclusions and structured provenance
   signals both land there.
3. The MCP interface doc (`design/mcp-interface.md`) needs a section
   for the depth parameter and the analyst role split. This
   document is the input to that revision.
4. The `vet-dependency` skill (currently the prototype of Layer 2
   provenance) is the template for the provenance-analyst system
   prompt. Keep iterating on the skill; it's the specification the
   MCP will implement.

## Reference: the atuin session as a worked example

The architecture above is built backwards from what made the atuin
analysis good. Mapping the session to the proposed layers:

| Session pass | Architecture layer | Notes |
|--------------|-------------------|-------|
| API metadata collection | Layer 1 (existing) | Worked cleanly |
| Signal model application | Layer 2 provenance | System prompt currently lives in the `vet-dependency` skill |
| Local-clone deep dive | Layer 1 (new git + source collectors) | Much of this should be deterministic; the LLM only reasons about signal combinations |
| External security review | Layer 2 security | Standalone Opus agent; the prompt framing is the template |
| Integration in `atuin.md` | Layer 3 synthesis | Hand-authored; this is what the synthesist replicates |

The concrete conclusions from the security review fall into a few
categories, each mapping to a collector vs. analyst decision:

| Conclusion | Implementation |
|------------|---------------|
| Hardcoded `api.atuin.sh` | Collector (grep) |
| Unauthenticated Windows TCP daemon | Collector (grep + auth-middleware presence check) |
| `ai.enabled` defaults to Y | Collector (parse setup flow defaults) |
| `secrets_filter` coverage gaps | Mixed — collector extracts the regex set, analyst reasons about what it misses |
| Shell-allowlist-not-sandbox | Analyst (requires reasoning about tool composition) |
| AI tool surface enumeration | Collector (AST walk of tools/mod.rs enum) + analyst commentary |
| Prompt-fatigue failure mode | Pure analyst — this is a threat-model observation |

Most conclusions have a collector component. The analyst layer earns
its cost on the composition/reasoning conclusions, not the
enumeration conclusions. This is the right split.

---

## Appendix: Why not a single unified agent?

Worth addressing directly because it's a tempting simplification.

You could write one big prompt that says: "apply the trust model
AND think skeptically about code AND produce a unified document."
With Opus, this probably even works for simple targets.

Three reasons not to:

1. **Cost.** Every analysis pays for the full framing, always. No
   option to run just provenance cheap.
2. **Quality drift.** The two cognitive modes compete for attention.
   In our atuin case, if the same agent were responsible for both,
   one or the other would have been shorter. Which one gets
   shortened depends on the target's shape, introducing
   inconsistency.
3. **Auditability.** When analyst outputs are separate, a reader
   can assess each independently. When they're merged in one
   document produced by one agent, disagreement becomes invisible
   — the agent reconciles before emitting.

The dual-analyst architecture is the explicit "two roles, two
outputs, explicit reconciliation" version of what a single agent
would do implicitly. The explicit version is reviewable.

---

## Empirical validation: the round-2 handoff experiment

After drafting this architecture, we ran a concrete test of the
dual-analyst handoff pattern on the same atuin target. Signatory's
provenance analyst wrote a prose follow-up document asking the
security analyst specific code-verification questions (preserved
at `/tmp/atuin-provenance-followup.md`; not checked into repo).
The security analyst responded with
[`atuin-security-review-external-round2.md`](analysis/atuin-security-review-external-round2.md).

Results bearing on this architecture:

### What worked

1. **Prose handoff was sufficient as a transport.** No structured
   schema existed; prose sections with priority ordering carried
   the questions well. This suggests the schema (next section) is
   worth building for *ergonomics and queryability*, not because
   the underlying communication is blocked without it.
2. **Bidirectional flow emerged.** The security analyst's "Net
   assessment" picked up the provenance analyst's framing
   ("social-engineering surface is realer than false-flag") and
   confirmed it from code trajectory. Cross-analyst engagement
   worked without being explicitly scaffolded.
3. **Self-correction occurred and was trust-positive.** The
   security analyst's round-2 §7 materially revised its round-1
   characterization of atuin-ai based on deeper source tracing.
   That revision is a *better* analysis, not a worse one — but
   the schema needs to represent it.
4. **Methodology catalog produced on request.** Asking for "the
   patterns you grep for on every project" yielded ~60 concrete
   patterns organized by category, directly usable as Layer 1
   collector specs. The collector backlog is now concrete.

### What broke the proposed schema

The first-pass schema in
`design/signal-storage-evolution.md` handled most conclusions but
had specific gaps the round-2 output exposed:

1. **Severity isn't always a scalar.** A conclusion like "daemon
   unauthenticated" was rated "medium on shared hosts, low on
   single-user." Single-enum severity is too rigid; the schema
   needs:

   ```go
   type Severity struct {
       Default ConditionalSeverity
       ByContext map[string]ConditionalSeverity  // "shared_host" → medium
   }
   ```

2. **"Positive" severity is needed.** The round-2 §7 conclusion
   reduced prior risk rather than surfacing new risk. It doesn't
   fit critical/high/medium/low/informational. Add `positive` as
   a first-class enum value with semantics "this conclusion reduces
   a prior-assessed risk."
3. **"Informational by design" distinct from "informational."**
   `Command::env_clear` not called for AI shell tool is a
   deliberate design choice (user needs SSH agent), not a latent
   issue. The schema needs an `informational_by_design` variant
   or a flag — `severity: informational, design_intent: true` —
   so consumers can distinguish "safely known" from "we should
   watch this."
4. **Supersession between rounds.** The round-2 §7 explicitly
   supersedes the round-1 atuin-ai assessment. Without a
   `supersedes: []ConclusionID` field, the trail of "this was wrong,
   here's why" disappears — which is itself the trust signal
   about analyst quality.
5. **Verdict-vs-rationale split is load-bearing.** Every round-2
   section opened with a bolded one-sentence verdict
   ("**Verdict: absent. Medium severity.**") followed by the
   code-path trace. The schema should have both:

   ```go
   type Conclusion struct {
       Verdict   string  // one sentence, dense
       Rationale string  // markdown, multi-paragraph
       ...
   }
   ```

   This enables programmatic scanning (filter by verdict) without
   losing the reasoning (serve rationale to humans).
6. **Methodology catalog wants its own type.** The round-2 §B was
   a structured catalog of grep patterns organized by signal
   group. Not a Conclusion, not a free-text sidebar. Needs:

   ```go
   type MethodologyCatalog struct {
       Source   AgentAttribution
       Patterns []MethodologyPattern
   }
   type MethodologyPattern struct {
       SignalGroup   string  // "network_endpoints", "local_listeners", ...
       Pattern       string  // ripgrep-ready, or prose description
       CollectorHint string  // "can this be mechanized?"
       FalsePositiveNotes string
   }
   ```

   This lets us mechanically extract "candidates for Layer 1
   collector implementation" from analyst output.
7. **Positive-absence signals want a distinct type.** Round-2 §C
   listed "checked and chose not to flag" items: unsafe-code
   absent, panic paths absent from SSE parsing, SQL injection
   shape absent from sqlx usage. These are *not* conclusions —
   they're the inverse. Without representation they're
   indistinguishable from unexamined absences. Proposal:

   ```go
   type PositiveAbsence struct {
       PatternChecked  string      // references a MethodologyPattern
       Citations       []Citation  // what the analyst looked at
       Confidence      enum        // spot_checked / thoroughly_reviewed / exhaustive
   }
   ```

### What the schema should actually be

Integrating the above, the per-pass analyst-output shape should
now be:

```go
type AnalystOutput struct {
    Attribution        AgentAttribution
    Target             CanonicalURI
    Conclusions        []Conclusion
    PositiveAbsences   []PositiveAbsence
    MethodologyTrace   *MethodologyCatalog   // optional, requested
    Supersedes         []AnalystOutputID     // rounds before this one
    ReframesFrom       []string              // free-text notes on cross-analyst engagement
}

type Conclusion struct {
    ID              ConclusionID
    Verdict         string  // one sentence
    Rationale       string  // markdown body
    Severity        Severity
    DesignIntent    bool    // "this is deliberate, not a bug"
    Category        string
    SignalType      *string  // fk into registry
    Citations       []Citation
    Supersedes      []ConclusionID
    AnswersQuestion *QuestionID  // if this conclusion responds to a VerificationAsk
    RelatedConclusions []ConclusionID
}

type Severity struct {
    Default   SeverityValue
    ByContext map[string]SeverityValue
}

type SeverityValue enum {
    Critical
    High
    Medium
    Low
    Informational
    Positive  // conclusion reduces prior risk
}
```

Plus the request/question side from the original proposal
(`AnalysisRequest`, `VerificationAsk`, `OpenQuestion`, renamed for
consistency — a "question" is a question regardless of whether
it's verifying a prior claim or opening a new investigation).

### Self-correction as a first-class MCP invocation

The round-2 §7 revision is valuable enough to warrant its own
invocation pattern beyond `analyze`:

```
signatory://verify/{conclusion_id}?depth=security
```

Re-invoke a specific prior conclusion with updated grounding (newer
commit, different depth, different model). The output is a new
AnalystOutput that `Supersedes` the prior one. This makes
"re-analyze a specific uncertain conclusion" cheap relative to
"re-run the whole analysis."

### Methodology trace as a side output

The grep-catalog from round-2 §B is the direct input to Layer 1
collector implementation. The MCP response envelope should
expose it separately:

```json
{
  "analysis": { ... },
  "signals": [ ... ],
  "methodology_trace": {
    "patterns_checked": [ ... ],
    "collector_candidates": [ ... ]
  },
  "metadata": { ... }
}
```

Tooling can ingest `methodology_trace.collector_candidates`
directly into a collector-backlog issue tracker.

### Implications for the v0.1 schema implementation

When we build `internal/exchange/`, the shape above is the
target. The atuin round-2 output is the first real test case;
round 1 covers the pre-correction state; both rounds together
exercise the supersession relationship.

A reasonable v0.1 implementation discipline: **every struct
field should be explainable via a specific concrete conclusion
from the atuin analysis.** If we can't point to "this field
exists because this round-2 conclusion had this property," the
field is speculative and should wait.

---

## Schema revisions post-trial (2026-04-14)

We ran a targeted schema-validation trial with the security analyst:
emit §5, §7, §8 as `Conclusion`s, the round-2 §B catalog as a
`MethodologyCatalog`, and two §C items as `PositiveAbsence`s, in
structured JSON. The emission is preserved at
[`analysis/atuin-schema-trial-response.json`](analysis/atuin-schema-trial-response.json)
and the analyst's meta-feedback at
[`analysis/atuin-schema-trial-feedback.md`](analysis/atuin-schema-trial-feedback.md).

The schema held — every structure was used, no conclusions had to be
dropped or wholesale reshaped. But the meta-feedback identified
five concrete gaps and four field-shape refinements.

### Gaps to close (must-have for v1)

1. **`Conclusion.Prerequisites []string`** (or `ThreatModel`). Structural
   qualifiers like "requires sync-server compromise" (F002) or
   "requires `ATUIN_AI__ADDITIONAL_CAPS` env var set" (F001)
   belong alongside severity, not buried in rationale. Unqueryable
   otherwise.
2. **`Conclusion.RemediationHint`** (or `FixShape`). Machine-consumable
   fix suggestions — "chmod 0600 after bind", "add cargo-deny CI
   step". Downstream tooling (issue-tracker backfills, automated
   PR suggestions) needs these structured.
3. **`AnalystOutput.Observations []Observation`**. Top-level slot
   for analysis that isn't a conclusion. The Michelle-trajectory trust
   analysis from round 2 is the motivating case: trust-model
   observation about contributor shape, not a vuln, not an absence,
   not a methodology pattern. `RoundNotes` was too TL;DR-shaped.
4. **`MethodologyPattern.ComposesWith []string`**. The analyst
   flagged MP-ENV-01 × MP-CAP-01 as the pattern pair that surfaces
   the §7 story; neither alone does. Pattern composition is real.
5. **Scope-flexible citations.** `PositiveAbsence` citations for
   "I ripgrep'd the whole crate" don't have line numbers.
   `Citation.LineStart` must be nullable with an alternative
   `ScopeRef` for non-line-grounded references.

### Field-shape refinements

6. **`ConditionalSeverity` vocabulary needs structure.** The analyst
   used `single_user`, `shared_host`, `multi_user_windows` as
   ad-hoc keys. The last mixes platform and deployment-shape. Adopt
   their proposal: context as a tuple of named dimensions rather
   than a flat map.

   ```go
   type ContextSpec struct {
       HostIsolation string  // "single_user" | "shared_host" | "container" | "ci_runner"
       Platform      string  // "unix" | "windows" | "any"
   }
   type Severity struct {
       Default   SeverityValue
       ByContext []struct{ Context ContextSpec; Value SeverityValue }
   }
   ```

   Dimensions and values are a controlled vocabulary; add dimensions
   as we encounter them. Flat-map form is a trap — readers have no
   way to know what keys to expect.

7. **`CollectorHint` becomes multi-axis.** The three-way
   automatable/requires_reasoning/context_dependent collapsed most
   patterns into `context_dependent`. Replace with:

   ```go
   type CollectorHint struct {
       GrepPrecision      string // "high" | "narrows" | "useless"
       ReasoningDepth     string // "none" | "one_hop" | "multi_hop"
       MissMode           string // "false_positive_heavy" | "false_negative_heavy" | "balanced"
   }
   ```

   The miss-mode axis matters for collector prioritization:
   FP-heavy wastes triage time but doesn't miss vulns; FN-heavy
   misses vulns silently (much worse, harder to catch).

8. **Supersession gets explicit structure.** Synthetic IDs like
   `"r1-ai-subsystem-threat"` were hacky. Replace:

   ```go
   type Supersession struct {
       PriorID    string
       PriorRound int
       Kind       string // "corrects" | "refines" | "deprecates"
   }

   type Conclusion struct {
       ...
       Supersedes []Supersession   // was []string
   }
   ```

   The `Kind` distinction is useful: *corrects* = prior conclusion was
   wrong, *refines* = same conclusion with better detail, *deprecates*
   = the prior concern no longer applies (e.g., upstream fixed it).
   Same structure at AnalystOutput level for round-to-round
   supersession.

9. **Verdict style guide, not schema change.** The analyst found
   verdict-for-F001 thin because it didn't convey "this is a
   correction." No schema field fix — instead, the skill/prompt
   style guide should say: *for corrections (severity: positive,
   non-empty supersedes), the verdict should state the conclusion
   **and** frame it as correcting prior work.* Schema handles the
   structure; style handles the prose affordances.

### Fields to relocate or de-emphasize

- **`Attribution.TokenCost`, `DurationMS`, `PromptVersion`** — these
  are ops-telemetry, wrong at analyst-output layer. Relocate to a
  `RunMetadata` sibling attached at the MCP response envelope level,
  or drop from analyst output entirely. Don't block v1 on this.
- **`Citation.Quoted`** — keep as optional, expect often-empty.
  Style guide: fill only when the quote adds information beyond the
  path + line range.
- **`PositiveAbsence.PatternRef`** — keep; genuinely useful at
  corpus scale when absences cross-reference methodology patterns,
  but will often be empty for small runs.

### One meta-conclusion worth preserving

The analyst's answer to trial question 8:

> *For my thinking: mild positive — drafting verdict first is a
> useful forcing function.*

The schema isn't just transport. **Structured emission shapes
analyst cognition.** Forcing a one-sentence verdict before rationale
commits the analyst to a position before prose hedging can soften
it. That's a quality improvement orthogonal to mechanization.

Conversely, their "round-2 prose had a color the structured version
lacks" tells us: the fix isn't to abandon structure; it's to
preserve prose-color via the `Observations` slot and `round_notes`
field. Which is what gap #3 above addresses.

### Updated v1 `AnalystOutput` shape

Integrating all revisions:

```go
type AnalystOutput struct {
    Attribution      AgentAttribution          `json:"attribution"`
    Target           string                    `json:"target"`
    TargetCommit     string                    `json:"target_commit,omitempty"`
    Conclusions      []Conclusion              `json:"conclusions"`
    PositiveAbsences []PositiveAbsence         `json:"positive_absences,omitempty"`
    Observations     []Observation             `json:"observations,omitempty"` // NEW
    MethodologyTrace *MethodologyCatalog       `json:"methodology_trace,omitempty"`
    Supersedes       []Supersession            `json:"supersedes,omitempty"`   // CHANGED from []string
    ReframesFrom     []string                  `json:"reframes_from,omitempty"`
    RoundNotes       string                    `json:"round_notes,omitempty"`
}

type Observation struct {
    ID          string   `json:"id"`
    Title       string   `json:"title"`       // one line
    Body        string   `json:"body"`        // markdown, multi-paragraph
    Category    string   `json:"category"`    // "trust_model" | "project_personality" | "trajectory" | ...
    SignalType  *string  `json:"signal_type,omitempty"` // fk if applicable
    Citations   []Citation `json:"citations,omitempty"`
}

type Conclusion struct {
    ID              string          `json:"id"`
    Verdict         string          `json:"verdict"`
    Rationale       string          `json:"rationale"`
    Severity        Severity        `json:"severity"`
    DesignIntent    bool            `json:"design_intent,omitempty"`
    Category        string          `json:"category"`
    SignalType      *string         `json:"signal_type,omitempty"`
    Citations       []Citation      `json:"citations,omitempty"`
    Prerequisites   []string        `json:"prerequisites,omitempty"` // NEW
    RemediationHints []string       `json:"remediation_hints,omitempty"` // NEW
    Supersedes      []Supersession  `json:"supersedes,omitempty"`    // CHANGED
    AnswersQuestion *string         `json:"answers_question,omitempty"`
    RelatedConclusions []string     `json:"related_conclusions,omitempty"`
}

type Citation struct {
    Path      string     `json:"path"`
    LineStart *int       `json:"line_start,omitempty"` // CHANGED: now nullable
    LineEnd   *int       `json:"line_end,omitempty"`
    Scope     *ScopeRef  `json:"scope,omitempty"`      // NEW: alternative to line-ref
    CommitSHA *string    `json:"commit_sha,omitempty"`
    Quoted    *string    `json:"quoted,omitempty"`
}

type ScopeRef struct {
    Kind string `json:"kind"` // "crate" | "dir" | "tree" | "workspace"
    Path string `json:"path"`
}

type MethodologyPattern struct {
    ID                 string         `json:"id"`
    SignalGroup        string         `json:"signal_group"`
    Description        string         `json:"description"`
    Pattern            *string        `json:"pattern,omitempty"`
    CollectorHint      CollectorHint  `json:"collector_hint"`    // CHANGED: struct, not enum
    ComposesWith       []string       `json:"composes_with,omitempty"` // NEW
    FalsePositiveNotes string         `json:"false_positive_notes,omitempty"`
    HitOnTarget        *bool          `json:"hit_on_target,omitempty"`
}
```

### Readiness gate

The v1 schema is ready for implementation in `internal/exchange/`.
All revisions are backed by specific signal from the trial; none
are speculative. The atuin trial-response JSON is the first test
fixture; `internal/exchange/` round-trip tests should parse it,
re-serialize it, and match byte-for-byte after a normalization pass
(or at least produce semantically identical Go structs).

Not blocking v1 but worth tracking:

- Controlled vocabulary for `ContextSpec.HostIsolation` and
  `.Platform` — start with the handful of values the atuin case
  needed; add as surfaced by future analyses.
- `RunMetadata` envelope placement (MCP response vs. analyst
  output) — design decision, can land in a follow-up.
- Style guide for verdict phrasing (corrections, conditional
  severity, design-intent conclusions) — lives in the
  `vet-dependency` skill, not in the schema.
