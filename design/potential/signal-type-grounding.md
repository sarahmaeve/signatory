# Signal-Type Grounding: linking methodology patterns to threat-landscape case studies

**Status:** sketch, captured 2026-05-27. Captured during a survey of the
Revelara CLI (`filestore/clones/rvl-cli/`) — Revelara's matchers carry
structured failure-mode metadata (`FailureDescription`,
`IncidentFrequency`, `TypicalBlastRadius`, `TypicalMTTR`,
`RelatedControls`) and the question was whether to import similar
fields onto `MethodologyPattern`. The answer is no — but the survey
surfaced a real gap: our `design/threat-landscape/*.md` case studies
justify the signal model but are not visible to users of the CLI / MCP.
This doc sketches a minimal surface that links methodology patterns to
threat-landscape entries *without* dragging the attack content into LLM
context.

**Not a v0.1 commitment.** Treat as a design space — sections below name
hazards and trade-offs that need resolution before any code lands. The
curation-burden question (§Open questions) may be decisive.

## Background: what we considered and rejected from Revelara

Three Revelara patterns initially looked applicable to Signatory; reading
our design + code in depth showed two are category errors and one is
already covered by the existing schema:

1. **Structured failure-mode metadata on signal types.** Revelara's
   matchers carry `FailureDescription / IncidentFrequency /
   TypicalBlastRadius / TypicalMTTR / RelatedControls`. **Rejected.**
   Revelara's domain is reliability engineering, where SRE incidents
   have measurable MTTR and steady-state frequency. Supply-chain
   compromise has neither — blast radius is emergent from `signal ×
   target criticality` (already handled by
   `MethodologyPattern.ComposesWith`), and incident frequency is bursty
   per-attacker. Our existing axis — forgery resistance
   (`design/trust-model.md:290-315`) — is the durable "why this signal
   matters" framing. Adding incident-statistics fields would also
   violate the agent-output contract
   (`design/archive/agent-output-contract.md:1-23`): analysts can't
   reliably emit `IncidentFrequency: "12/year"` without hallucinating.

2. **A `Floor` / non-waivable bit on conclusions.** Revelara uses this
   for compliance findings that bypass the standard waiver path.
   **Rejected.** Three places in `design/trust-model.md` rule it out:
   Core Principle 1 (compositional, no single signal is definitive),
   posture tiers are consumer-relative (`vetted-frozen` for one org is
   `unknown-provenance` for another), and burns are the actual
   hard-stop mechanism (entity-level, federated, reversible, auditable
   — none of which a per-conclusion bit would be).

3. **Per-conclusion fingerprint for cross-analyst dedup.** Revelara
   uses `LocationFingerprint` to dedup the same finding across local
   and AI scanners. **Rejected.** Their fingerprint works because
   findings are pinned to (slug, file, line) — a concrete rule firing
   at a concrete location. Our conclusions are interpretive: two
   analysts flagging "weak release governance" can pick different
   `signal_type` values, attach citations to different files, and
   phrase the verdict differently. Semantic concordance is the job
   of the synthesist (`SynthesisSupplement.ConcordanceStrengths`),
   not a mechanical hash.

## The real gap

The design docs (`design/trust-model.md`, `design/anti-subversion.md`,
the 18 files in `design/threat-landscape/`) carry the justification for
why each signal class is in the model. These motivate our methodology
patterns in concrete terms: `content-injection-surface` is anchored in
Trapdoor (2026-05-24) and CamoLeak; account-takeover patterns are
anchored in axios (March 2026) and TanStack mini-Shai-Hulud
(2026-05-12); fallow-code degradation is anchored in xz-utils.

None of this is visible to a user reading `signatory show-methodology`
or `signatory show-conclusions`. They see structured patterns with
`SignalGroup`, `Description`, `CollectorHint`, but no link to the
historical incidents that anchor them. A user is left to take the
patterns on faith or go read `design/` directly — which is fine for
contributors but bad for trust-tool users.

## Proposal shape

Three load-bearing constraints, then the sketch:

### Constraints

1. **Case-study bodies never enter LLM context.** The
   `design/threat-landscape/*.md` files describe attack mechanics in
   detail; some quote payloads (base64 blobs, zero-width Unicode
   examples, exfiltration URL shapes). They are designed for humans
   analyzing past incidents, not safe for LLM ingestion. We'd be
   hand-rolling exactly the file class our own `contentinjection`
   scanner flags on packages. This rules out:
   - Any MCP tool that returns case-study bodies.
   - Any MCP resource (`signatory://threat-landscape/<slug>`) that
     returns case-study bodies.
   - Embedding case-study bodies in `synthesis/evidence.go`'s payload
     (would route them straight to our own Layer-3 prompt).
2. **The synthesist judges from analyst evidence, not from our incident
   catalog.** If we put a curated "this signal is grounded in
   Trapdoor + CamoLeak" decoration into the evidence rollup, the
   synthesist is no longer doing independent judgment — it's
   pattern-matching against priors we chose. Decoration is for
   *human* readers of CLI / HTML output only.
3. **Analyst-emitted output is unchanged.** No new field on
   `MethodologyPattern` or `Conclusion`. Analysts already emit
   `signal_type`; grounding lives at the signal-type catalog level,
   not per-emission. Token-for-judgment principle preserved
   (`design/archive/agent-output-contract.md`).

### The shape

A curated catalog file, embedded at build time, joined at human-render
time:

```yaml
# internal/signal/catalog/signal-types.yaml (proposed location)
- name: content_injection_surface
  signal_group: hygiene
  summary: Structural prompt-injection primitives in supply-chain artifacts.
  grounded_in:
    - slug: 2026-05-24-trapdoor-crypto-stealer
      label: Zero-width Unicode in .cursorrules / CLAUDE.md across npm/PyPI/crates.io
      date: 2026-05-24
    - slug: 2025-camoleak
      label: Markdown image-syntax exfiltration via rendering surfaces
      date: 2025-10-15

- name: account_takeover_pattern
  signal_group: identity
  summary: Privileged behavior change from a maintainer account after silence.
  grounded_in:
    - slug: 2026-03-axios-attack
      label: npm supply-chain compromise of axios via maintainer-account takeover
    - slug: 2026-05-12-tanstack-mini-shai-hulud
      label: Targeted credential exfil through compromised TanStack maintainer pipeline
```

Catalog values are deliberately thin: slug + one-line **defensive**
label + date. Phrasing names the signal class, not the attack mechanism.
No payload examples. No exploitation steps. Belt-and-braces: catalog
content runs through `internal/contentinjection` at build time — if our
own scanner flags a label, the build fails.

### Surfaces

Render-time decoration only:

- **`signatory show-methodology`** — for each pattern's
  `signal_type`, append `Grounded in: <label> (<date>)` lines from
  catalog lookup. No body, no URL fetch.
- **`signatory show-conclusions`** — same decoration on conclusions
  carrying a `signal_type`.
- **`signatory detail signal_group=…`** — when a user drills into a
  group, list which signal_types live in the group and which incidents
  anchor each.
- **HTML report (`show-synthesis --html`)** — per
  `design/potential/expanded-reporting.md`, the static HTML can carry
  outbound URLs to a future docs site (`https://signatory.dev/incidents/<slug>`),
  not embedded bodies.

What the user sees in CLI:

```
F003 [medium] content_injection_surface
  Verdict: README contains zero-width Unicode between code-block markers.
  Grounded in:
    - Trapdoor: zero-width Unicode in .cursorrules / CLAUDE.md (2026-05-24)
    - CamoLeak: markdown image-syntax exfiltration (2025-10-15)
```

What the synthesist sees (deliberately undecorated):

```
F003 [medium] content_injection_surface
  Verdict: README contains zero-width Unicode between code-block markers.
  ...
```

The asymmetry is the point: decoration is a *reader affordance*, not
input to judgment.

### Validator change (separable but adjacent)

The `signal_type` field on `Conclusion` and `MethodologyPattern` is
currently free text (`internal/exchange/types.go:183, 270`). With a
catalog in place, `exchange.Validate` can reject unknown signal_types
with the catalog listed in the error. This closes a long-running soft
gap. Migration concern: historical analyst outputs in the dogfood
store carry ad-hoc signal_types that won't match a curated catalog;
needs a backfill mapper or an `unknown` bucket for stragglers.

## Hazard analysis

The risks of getting this wrong, in rough order of severity:

1. **Case-study bodies leak into LLM context.** Either by mistakenly
   exposing them via MCP, or by a future contributor "harmonizing" the
   catalog to include the body inline ("why have two files?"). The
   contentinjection scanner running at build-time catches the most
   obvious form (zero-width chars, bidi controls in labels) but not
   the prose-vector form ("Trapdoor used U+200B/C/D embedded at
   position N to..."). The non-mechanical guard is: labels must read
   as defensive identifiers, not attack writeups. A code-review
   norm, not a test.
2. **Synthesist herding on the catalog.** Even slug + label in the
   evidence rollup primes the model toward our priors. The constraint
   "decoration is for human readers only" addresses this if held —
   but a future refactor that "consolidates rendering" could
   accidentally route the catalog through the synthesist handoff.
   Mitigation: the synthesist handoff builder is a distinct code
   path from CLI rendering, and tests assert the handoff body does
   not contain catalog labels.
3. **Curation lag.** A new threat-landscape case study lands as a
   `.md` file but no one updates `signal-types.yaml`. The catalog
   silently grows stale. Mitigation options: (a) a lint check that
   every `design/threat-landscape/*.md` has a frontmatter listing
   the signal_types it anchors, and the catalog is *generated* from
   frontmatter (single source of truth), or (b) accept the manual
   sync burden and document it.
4. **Prior-knowledge anchoring.** Naming "Trapdoor" or "axios" in a
   label is enough to trigger an LLM's training-data priors about
   those incidents. Counter: those priors are present regardless;
   any decent analyst LLM already knows xz-utils, axios,
   shai-hulud. Our catalog labels public events with their public
   names. We are not leaking novel data.

## Open questions

- **Catalog or URL-only?** Strictest reading of the case-study-body
  concern: don't ship a catalog at all. Publish the threat-landscape
  pages on a docs site and have the CLI emit a single URL by lookup,
  with no embedded label at all. Pros: zero risk of body leak.
  Cons: no useful CLI output offline; users need a browser to see
  the grounding. Probably the right answer if we don't yet have a
  docs site, since otherwise the URLs are dead.
- **Catalog source: hand-written YAML or generated from `.md`
  frontmatter?** Generated keeps a single source of truth and
  prevents lag, but means every threat-landscape file has to be
  parseable (frontmatter discipline). Hand-written is simpler but
  drifts. Lean: frontmatter, with the `.md` files being the
  authoritative source.
- **Should the catalog ship at all in v0.1 if there's no docs site
  to link out to?** Maybe not. A `Grounded in: Trapdoor (2026-05-24)`
  line in CLI output without a way to read further is a
  half-finished feature. Could land catalog + validator without the
  rendering decoration, then add decoration when there's somewhere
  to send the curious reader.
- **Signal-type vocabulary migration.** Closing the free-text gap on
  `signal_type` will reject historical outputs. What's the migration
  path — backfill mapper, schema-version flag, or accept-and-document
  the discontinuity?

## Relationship to other work

- **`design/anti-subversion.md` §"Where this slots into the
  architecture"** is the precedent for adding a new SignalGroup
  (`content-injection-surface`) without changing the schema. Same
  shape applies here — the catalog enriches that signal-group entry
  without changing the analyst-emission contract.
- **`design/potential/expanded-reporting.md`** sketches the HTML
  report surface; the catalog would feed that surface's grounding
  links if both ship.
- **`design/trust-model.md`** is the source of the constraint that
  ruled out Revelara's failure-mode-metadata approach. The
  compositional / multi-signal / consumer-relative principles are
  load-bearing here.

## Decision points before code

1. URL-only or embedded catalog? (Hinges on docs-site timing.)
2. Frontmatter-generated or hand-written YAML?
3. Signal-type vocabulary check now or later?
4. Does the synthesist deliberately not see catalog decoration, or
   do we want to revisit that?

These are not blockers for sketching; they are blockers for landing.
