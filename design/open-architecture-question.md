# Open architecture question: synthesist handoff transport

**Status:** OPEN — awaiting Option B hypothesis-confirmation test
**Opened:** 2026-04-22
**Related:**
- ROADMAP §"Reliability investigations (dogfood-surfaced)"
- `sync/KAIZEN.md` 2026-04-22 synthesist TLS entry
- M6c (synthesist handoff assembly) — `design/m6-synthesis-contract.md`

## Problem

Synthesist subagent dispatches have failed 3 of 4 times since M6c shipped
(2026-04-21) with `unable to verify the first certificate` on the
WebFetch call against the localhost pipeline service. Analyst WebFetches
(security, provenance) to the same service in the same session succeed.
The failure correlates with the synthesist's tightened allowed-tools
set: analysts have `Read Glob Grep`; the synthesist has only
`WebFetch mcp__signatory__signatory_ingest_analysis`.

### Mechanism hypothesis

`NODE_EXTRA_CA_CERTS` is exported at shell-profile load as:

```bash
export NODE_EXTRA_CA_CERTS="$(mkcert -CAROOT)/rootCA.pem"
```

The `$(mkcert -CAROOT)` substitutes at profile-load time, so Claude Code
inherits a literal filesystem path (e.g. `/Users/sarah/Library/
Application Support/mkcert/rootCA.pem`). Node's TLS stack calls
`fs.readFileSync(process.env.NODE_EXTRA_CA_CERTS)` on every HTTPS
handshake to build the trust chain.

A subagent without Read cannot satisfy that syscall → CA chain
incomplete → mkcert's localhost cert fails to verify against the
system trust store (which doesn't contain it) → the observed error.

**Confirmation test:** Option B below. Documenting Option A here so
the design work survives if B confirms the hypothesis and we decide
to invest in the cleaner shape later.

## Option A: fetch-handoff MCP tool

Add a new read-only MCP tool, `signatory_fetch_handoff(session_id,
role) → handoff_markdown`, that delivers the synthesist's handoff
over stdio instead of HTTPS. MCP is already the transport for
`signatory_ingest_analysis` and the six read-only tools; no TLS
stack, no CA chain, no file-read dependency.

### Dispatch flow

```
Orchestrator (main loop)                     Synthesist subagent
        │                                            │
  (1) signatory handoff synthesist $TARGET           │
      Go reads store, assembles evidence,            │
      returns handoff markdown on stdout             │
        │                                            │
  (2) POST handoff to pipeline service               │
      (shell variable → curl --data-binary)          │
      Bytes never enter LLM attention                │
        │                                            │
  (3) Dispatch Agent(synthesist) with small          │
      prompt: {session_id, role, instruction         │
      to call signatory_fetch_handoff}               │
        │ ─────────────────────────────────────────→ │
        │                                            │
        │           MCP stdio (no TLS)               │
        │ ←────── fetch_handoff(sess, role) ──────── │
        │ ──────── handoff markdown (~50k) ────────→ │
        │                                            │
        │           synthesist reasons               │
        │                                            │
        │           MCP stdio (no TLS)               │
        │ ←──── ingest_analysis(synthesis JSON) ──── │
        │ ───────────── output_id ──────────────────→│
        │                                            │
  (4) signatory posture accept $output_id            │
```

The handoff's 50k tokens flow: Go stdout → shell variable →
pipeline POST → session store → MCP server read → synthesist
context. **They never enter the orchestrator's LLM attention.**
This is the central economic win over the inline-dispatch
alternative (Option C in the discussion thread, not documented
here).

### Why this shape

1. **Orchestrator context stays clean.** Inlining the handoff as
   an Agent-prompt parameter would cost the orchestrator ~50k input
   tokens (reading) plus ~50k output tokens (emitting), roughly
   6× the per-dispatch token cost. MCP-stdio delivery keeps the
   bytes out of the orchestrator's attention entirely.

2. **Preserves WebFetch for analysts.** They're not broken —
   their Read capability lets CA loading succeed. Don't rebuild
   what works.

3. **MCP is the signatory trust boundary.** Store reads already
   flow through it. Making handoff delivery an MCP concern puts
   envelope transport in the same architectural layer as evidence
   assembly.

### Cost estimate

- New MCP tool: ~100 LOC (handler + schema + registration).
- Tests: ~100 LOC (unit + invariants coverage).
- SKILL.md Step 4: ~5-line edit (swap WebFetch URL for tool call).
- Synthesist allowed-tools: `mcp__signatory__signatory_fetch_handoff`
  + `mcp__signatory__signatory_ingest_analysis` (no WebFetch, no
  Read/Glob/Grep).

### Open sub-questions

1. **Process boundary.** Does the MCP server share a process with
   `signatory serve`? If separate, the MCP server needs a way to
   read session state — either via HTTP callback to the pipeline
   service (same TLS concern unless we expose plain HTTP on a
   secondary port), or via a shared on-disk session store. Preferred
   answer: consolidate the two into one process, since they already
   share store access.

2. **What does the tool deliver?** The handoff *body* (same content
   `signatory handoff synthesist` emits, already POSTed to the
   session) or a *freshly rendered* handoff with evidence assembled
   at call time? Lean toward the former — assemble once on the
   orchestrator side, store in session, serve from session. Keeps
   orchestrator-produced and synthesist-consumed handoffs byte-
   identical.

3. **Do analysts eventually move over?** The same tool-set-gated
   TLS failure class could hit them if someone retries with a
   tighter allowed-tools set. Not blocking today; their WebFetch
   works. Migrate only if the analyst failure mode recurs.

4. **How does fetch_handoff authenticate the session?** The
   orchestrator creates the session and POSTs handoffs under a
   session ID. The synthesist subagent receives the session ID in
   its prompt. If the MCP server serves any session ID to any
   caller, that's fine for a local-only tool, but worth being
   explicit about as a non-requirement.

## Option B: add Read back to synthesist (hypothesis test)

Restore `Read Glob Grep` to the synthesist's allowed-tools. If the
mechanism hypothesis holds, WebFetch succeeds on first try and the
TLS flake class is closed with a one-line SKILL.md edit. If it
doesn't hold, we've paid one dogfood run (~150-200k tokens) to
falsify and must look elsewhere.

This is the cheaper confirmation path and is shipping **now**,
before committing to Option A's rework. Result determines whether
Option A is worth building.

## Decision matrix

| Option B outcome | Next step |
|---|---|
| Synthesist WebFetch succeeds first-try | Option A deferred; document mechanism in ROADMAP, leave Read in synthesist allowed-tools, revisit Option A if orchestrator-context economics or D9-fence concerns change |
| Synthesist WebFetch still fails | Hypothesis wrong or incomplete. Dig further before building Option A — could be env-var stripping, session-cache effect, or something we haven't pattern-matched yet |
| Intermittent (fails sometimes) | Investigate environmental nondeterminism before building anything; Option A doesn't help if the mechanism isn't understood |

## D9 concern with Option B

Giving the synthesist Read/Glob/Grep reopens — in capability, not
in instruction — the "browse filestore for prior analyses" attack
surface that M6's D9 fence was designed to close. Mitigations:

1. The handoff body explicitly instructs the synthesist to treat
   the evidence block as its complete source of truth (already in
   template as of M6c, CI-enforced by `TestIndependenceFence_
   PresentInAllHandoffs`).

2. The evidence assembler (`internal/synthesis/`) filters prior
   syntheses out *at the data layer* before they ever reach the
   handoff. A synthesist that Reads around filestore cannot find
   prior syntheses via the orchestrator's curated path; they'd
   have to go rummaging through raw analysis files, which is a
   clear deviation from instructions and would surface in the
   audit trail.

3. The SKILL.md prompt now includes an explicit negative: "Read/
   Glob/Grep are present only for CA trust-chain loading; MUST
   NOT be used to browse filestore or prior analyses."

Option A eliminates this concern mechanically (no file-read
capability); Option B leaves it as a soft instruction-level
enforcement. If Option B confirms the hypothesis but the D9
attack-surface exposure feels too large, that's the pressure
that justifies investing in Option A.

## 2026-04-22 post-test outcome

Option B shipped and tested. First `/analyze` under the fix
(`github.com/google/uuid`, 12m 12s end-to-end) completed with the
synthesist's WebFetch succeeding on first try. Hypothesis
confirmed: `NODE_EXTRA_CA_CERTS` is a literal filesystem path
and subagent Node-TLS handshakes require Read capability to load
it. TLS flake class closed at the capability layer; D9
independence now enforced at the prompt-instruction layer plus
the data layer (evidence assembler filters prior syntheses
before they reach the handoff).

**Option A is deferred, not abandoned.** The mechanically-
enforced shape remains correct if instruction-layer D9
enforcement proves insufficient. Escalation triggers:

- **D9 drift incident.** A dogfood synthesist visibly browses
  `filestore/analysis/*.md` or equivalent prior-analysis sources
  during synthesis — would indicate the "you have tools, don't
  use them for X" prompt is losing ground against the available
  capability.
- **Orchestrator-context pressure.** If handoff delivery over
  localhost HTTPS becomes cost-dominant for other reasons
  (e.g., larger evidence blocks, concurrent sessions), moving
  to MCP stdio delivery becomes attractive independent of the
  D9 story.
- **Analyst-side TLS failure.** If analyst WebFetches start
  failing with the same mechanism (e.g., if a future analyst
  role is dispatched without Read for independent reasons), the
  MCP path becomes attractive for all subagent roles at once.

**Instruction-drift observation from the uuid run.** The
synthesist used Grep on `internal/exchange/enums.go` to look up
`ForgeryResistance` enum values during output assembly. Not a D9
violation (enums are output schema, not prior analyses), but it
shows that granted file-read capability is used beyond its
stated purpose in SKILL.md. The correct response is template-
level: inline the enum vocabulary in the synthesis handoff body
so the synthesist has no legitimate reason to look elsewhere.
This is a smaller, cheaper fix than Option A and addresses the
specific observed behavior. Option A is not required for this
class of deviation.
