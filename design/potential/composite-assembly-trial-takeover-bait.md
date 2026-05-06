# Composite Assembly Trial — `takeover-bait` over Existing Analyses

**Date:** 2026-04-21
**Trigger:** After the 2026-04-21 Vercel / Context.ai threat-landscape
entry and the accompanying `takeover-bait` composite spec landed, the
open question was whether the composite could be mechanically assembled
from conclusions already in the store, without new collection or code
changes.

**Method:** Read-only `show-conclusions --target <...>` against the
three most recent npm-ecosystem analyses in the store, inspect whether
the four composite inputs are present as discrete, query-able
conclusion codes.

## Targets surveyed

| Target | Canonical URI in store | Date ingested |
|---|---|---|
| invariant@2.2.4 | `pkg:npm/invariant@2.2.4` | 2026-04-21 |
| escape-html | `repo:github/component/escape-html` | 2026-04-21 |
| msgpack-lite (v0.1.26) | `repo:github/kawanet/msgpack-lite` | 2026-04-20 |

All three were later posture-decided (invariant → `trusted-for-now`,
the other two → `rejected`). All three fit the `takeover-bait`
rationale shape on human reading of the synthesis.

## Composite-input matrix

| `takeover-bait` input | invariant@2.2.4 | escape-html | msgpack-lite |
|---|---|---|---|
| `vitality_unmaintained` | ✅ `vitality_unmaintained` [high] | ✅ `vitality_unmaintained` [high] | ✅ `vitality_unmaintained` [high] |
| `bus_factor_low` | code=`bus_factor` [medium] | code=`governance`, signal_key=`effective_maintainer_concentration` [medium] | code=`effective_maintainer_concentration` [high] |
| `trusted_publisher_absent` | code=`publish_chain_integrity` [medium] | code=`publish_chain` [medium] | code=`publication_integrity` [medium] |
| `network_critical_effect` | ❌ rationale-only ("~26M weekly downloads") | ❌ rationale-only ("~75M weekly downloads") | ❌ rationale-only ("1.25M weekly downloads") |

## Finding 1 — The composite's *shape* is validated

All three packages unambiguously fire `takeover-bait` on the
human-readable semantics. Every input is present in every analysis.
The model is real; a synthesist (or a human) reading these analyses
would confidently assert the composite fires. The design is not
invalidated.

## Finding 2 — The composite is NOT mechanizable today

A deterministic SQL-view-style query over `show-conclusions` cannot
assemble this composite from the current store. Three distinct
blockers:

### Blocker A — Conclusion-code vocabulary drift

Only `vitality_unmaintained` is a stable code string across runs.

- **Bus factor** appears under *three different code names* across
  three analyses: `bus_factor`, `governance` (with signal_key
  `effective_maintainer_concentration`), and `effective_maintainer_concentration`
  (code-is-signal-key). This is not a typo pattern — it is three
  different analyst runs reaching for three different
  conceptualizations of the same underlying phenomenon.
- **Trusted-publisher-absent** appears under three variants:
  `publish_chain_integrity`, `publish_chain`, `publication_integrity`.
  Again, not typos — the analyst prompt does not constrain the code
  vocabulary, so each run picks a plausible local name.

A `WHERE code = 'bus_factor'` query finds one of the three matches.
A query with a hand-maintained alias union (`code IN
('bus_factor', 'governance', 'effective_maintainer_concentration')`)
works as a stopgap for today but will silently miss the next synonym
an analyst invents.

### Blocker B — `network_critical_effect` is prose-only

Every analysis contains the raw download figure ("~26M / ~75M /
1.25M weekly downloads"), but only inside the rationale text of
another conclusion (typically `vitality_unmaintained`). There is no
structured observation with a threshold-comparable number and no
discrete code naming the criticality/blast-radius shape. The
threat-landscape entry already anticipated this; this trial
confirms it empirically.

### Blocker C — Canonical URI shape is not uniform for npm targets

The three npm-ecosystem analyses landed at three different URI shapes:

- `pkg:npm/invariant@2.2.4` — version-scoped, ecosystem-qualified
- `repo:github/component/escape-html` — repo-scoped, no ecosystem,
  no version
- `repo:github/kawanet/msgpack-lite` — repo-scoped, no ecosystem,
  no version

A composite that wants to scope "npm packages with takeover-bait
present" cannot filter by ecosystem, because the ecosystem is only
present in one of the three URIs. A query that unions package-scope
and repo-scope URIs would pick up these three but would over-match on
any repo-scope target that happens to live on GitHub regardless of
ecosystem.

*Canonicalization work is already partly underway as of 2026-04-21
(per Sarah); this trial records the problem empirically for the
in-flight work to resolve, not as a new blocker.*

## What this means for the composite spec

The `takeover-bait` composite as written is accurate. The blockers
are upstream of the composite — in the signal and URI layers it
reads from, not in the composite's own logic.

Two prerequisite workstreams enable a mechanical composite:

1. **Conclusion-code vocabulary pinning.** The per-role handoff
   templates (`templates/handoffs/security-review-v1.md` and
   provenance equivalent) or `design/agent-output-contract.md` need
   to enumerate the canonical code strings analysts may emit, with
   any deviation producing a schema-validation error at ingest.
   Round-2 ingest or a one-shot migration can retrofit the three
   existing analyses onto the canonical vocabulary.
2. **Structured criticality observation.** The handoffs should
   require analysts to emit `weekly_downloads`,
   `dependent_package_count`, or the equivalent ecosystem-appropriate
   observation as structured data with a raw numeric value, rather
   than embedding the figure in rationale prose. Composites then
   derive the boolean `network_critical_effect` via threshold
   comparison.

Canonical URI shape for npm targets is a separate in-flight
workstream.

## Stopgap option

If the `takeover-bait` composite is wanted in dogfood use *before*
vocabulary pinning lands, a "vocabulary-union matcher" can be
implemented as a read-time derivation:

```
takeover-bait(target) :=
    exists(conclusion where code = 'vitality_unmaintained')
  AND exists(conclusion where code IN ('bus_factor',
                                       'governance',
                                       'effective_maintainer_concentration'))
  AND exists(conclusion where code IN ('publish_chain_integrity',
                                       'publish_chain',
                                       'publication_integrity'))
  AND (rationale_mentions_downloads OR manual_criticality_flag)
```

This is fragile — each new analyst run may introduce a fourth
synonym silently — but it surfaces the composite on today's store
without code changes beyond the query, and each false negative is a
prompt to expand the alias union *and* tighten the handoff template.
Treating the alias list as temporary technical debt with a named
retirement condition (vocabulary pinned + existing analyses
migrated) keeps the stopgap from becoming permanent.

## What this trial does *not* do

- Does not re-analyze any of the three packages.
- Does not modify the composite spec. The spec is correct; the
  store is what is inadequate.
- Does not propose a URI canonicalization scheme — that work is in
  flight elsewhere.
- Does not promote any of the code aliases to canonical status; the
  pinning decision is a separate design call that should consider
  more than three data points.

## Cross-references

- [`../threat-landscape/2026-04-21-vercel-contextai-incident.md`](../threat-landscape/2026-04-21-vercel-contextai-incident.md)
  — the incident that motivated naming the composite in the first
  place
- `internal/signal/types.go` — the registered signal-type catalogue;
  adjacent to where a conclusion-code vocabulary would live
- `filestore/analysis/invariant-2.2.4-synthesis.md`,
  `escape-html-synthesis.md`, `msgpack-lite-synthesis.md` — the
  narrative syntheses this trial's matrix is derived from
