# Signatory: Developer-Identity Tracking Plan

## Status

Draft — beginning of the discussion. Recorded so the architectural
question doesn't get lost between sessions. Captures the schema
sketch, the connection to existing work, and the open questions
that need resolution before implementation.

## Motivation

The signatory trust model treats identity as load-bearing
(per `design/trust-model.md` core principles #1, #3, #4 and
the worked example at `design/example-identity-analysis-rsc.md`).
But the data layer doesn't currently support it.

Concrete gap: in every engagement we've run, rich identity data
ends up as **prose** in `Observation.body` or `Conclusion.rationale`
fields. From the atuin analysis alone, we collected:

- Ellie Huxtable: 6.7-year GitHub tenure, blog at ellie.wtf, three
  emails in `.mailmap` (`ellie@atuin.sh`, `ellie@elliehuxtable.com`,
  `e@elm.sh`), 1,604 followers, real name + Amsterdam location
- Michelle Tilley: emerging co-lead in 2026 (63 commits in 3.5
  months), in-lane scope trajectory, no touches to crypto/sync/auth
- Conrad Ludgate: migrated from `conrad.ludgate@truelayer.com` to
  personal email in 2019; mailmap-recorded
- Six other contributors with similar corporate→personal email
  migrations across multi-year windows (deep identity-graph signal)

For thefuck:
- Vladimir Iakovlev (`nvbn`): 15-year GitHub tenure, blog at
  `nvbn.github.io`, two name variants in git log
- Pablo Santiago Blum de Aguiar (`scorphus`): co-maintainer 2019-2023
  with year-by-year activity decline correlating with project fallow

None of this is queryable today. We can't ask:

- "Show me every project where Ellie is the lead maintainer."
- "What's nvbn's total contribution footprint across our analyses?"
- "Has any maintainer's identity-graph score changed since we last
  analyzed?"
- "Burn `identity:github/ellie` → list every signal in the database
  that was attributed to her."

The last one is the operational consequence of trust-model.md
principle #3 ("retroactive degradation across everything they
touched"). It can't be implemented without identity entities.

## What the existing schema offers

Relevant existing pieces:

- `EntityIdentity` is defined as a type constant in
  `internal/profile/entity.go:35`. The string is `"identity"`.
- The `identity:{platform}/{username}` URI scheme is documented in
  `design/entity-model-v2.md` and the validator at
  `internal/profile/uri.go` accepts it.
- `cmd/signatory/analyze.go:25` advertises "Package name, repo URL,
  or identity to analyze" in the `--target` help.
- The `internal/identity/` package exists for `identity.Current()`
  — the **team-identity** abstraction (sarah+claude-opus-4.6),
  which is a different concept (who's *making* trust decisions),
  not who's being analyzed.

Crucially, no code path actually:
- Creates an entity with `type = "identity"`
- Collects per-developer signals (account_age, follower_count,
  cross-platform-consistency)
- Links identity entities to project entities via a
  contribution/maintainership relationship
- Surfaces identity in any query

The schema has the *space*; nothing populates it.

## Trust model interactions

This work makes two trust-model principles operational that are
currently aspirational:

### Principle #3: retroactive degradation

> "Compromises are discovered after the fact. The system must
> support re-evaluating a window of history: 're-score everything
> this entity touched between date X and date Y.' Burns against an
> identity trigger retroactive signal degradation across everything
> they reviewed or approved."

This is a graph-traversal operation: identity → contributions →
signals/conclusions/observations that cited those contributions →
entities that depend on those signals. None of those edges exist in
the current data model. After this work:

```
BURN identity:github/ellie at 2026-04-14
  → traverse contribution_observations
  → identify all (entity, signal) pairs where ellie was the
    proximate cause (commit author of the cited code, maintainer
    at the time of the observation)
  → mark those signals as degraded with a burn reference
  → propagate to entities that depend on those signals
  → log the burn in audit_log with full traversal scope
```

### Principle #1: composable trust

> "Trust is multi-signal and compositional. ... This applies to
> identities, code patches, and projects alike."

Identity-as-entity means identity gets its own signal stream:
account_age, follower_count, public_repo_count,
cross_platform_consistency_score, identity_graph_depth,
contribution_continuity, account_takeover_signals (sudden activity
change, email change, 2FA disabled), etc. These compose into the
forgery-resistance assessment per principle #6.

## Proposed schema (migration v5+)

Lands after migration v4 (the ingestion-plan tables). The
identity work depends on having an analyst-output stream to
enrich.

### Identity entities

Identity gets the same `entities` row as any other entity,
with `type = "identity"`. Canonical URI per platform:

```
identity:github/ellie
identity:gitlab/scorphus
identity:email/nvbn.rm@gmail.com     -- email-only when no platform handle known
identity:blog/nvbn.github.io          -- blog-domain-only when that's all we have
identity:internal/sarah.chen          -- per entity-model-v2's enterprise realm
```

The same human can have multiple identity entities. Linking is
explicit (next section). The entities table needs no schema change;
we already have what we need.

### Identity links

```sql
CREATE TABLE identity_links (
    from_id     TEXT NOT NULL REFERENCES entities(id),
    to_id       TEXT NOT NULL REFERENCES entities(id),
    confidence  TEXT NOT NULL,   -- "asserted" | "verified" | "inferred"
    evidence    TEXT NOT NULL,   -- markdown — what proves they're the same
    source      TEXT NOT NULL,   -- "mailmap" | "github_profile_link" | "self_assertion" | "manual"
    linked_at   TEXT NOT NULL,
    PRIMARY KEY (from_id, to_id)
);
CREATE INDEX idx_identity_links_to ON identity_links(to_id);
```

Confidence levels:
- **`asserted`** — claimed by the identity itself (GitHub bio
  links to a blog; that link is asserted, not yet verified)
- **`verified`** — cryptographically or institutionally confirmed
  (.mailmap entries committed by the canonical identity;
  cross-platform identity verified via signed attestation;
  HR-system-verified internal identity)
- **`inferred`** — pattern-matched without confirmation (same
  name on two platforms; same email used at two different domains)

Same-direction links are non-symmetric on purpose: if A claims B
but B doesn't claim A, that's a one-way link with `confidence:
asserted`. Verifying the reverse direction makes it bidirectional.

### Contribution observations

```sql
CREATE TABLE contribution_observations (
    id            TEXT PRIMARY KEY,         -- UUID
    identity_id   TEXT NOT NULL REFERENCES entities(id),
    project_id    TEXT NOT NULL REFERENCES entities(id),
    role          TEXT NOT NULL,            -- "author" | "co-maintainer" | "lead" | "reviewer" | "former-maintainer"
    commit_count  INTEGER NOT NULL DEFAULT 0,
    first_commit  TEXT NOT NULL DEFAULT '', -- date of first observed contribution in window
    last_commit   TEXT NOT NULL DEFAULT '', -- date of last observed contribution in window
    observed_at   TEXT NOT NULL,            -- when this observation was recorded
    source        TEXT NOT NULL             -- "git_log" | "github_api" | "analyst_output_id:<uuid>"
);
CREATE INDEX idx_contribobs_identity ON contribution_observations(identity_id);
CREATE INDEX idx_contribobs_project ON contribution_observations(project_id);
CREATE INDEX idx_contribobs_role ON contribution_observations(role);
```

Append-only. Each ingestion or refresh adds new rows; "current
maintainership" is a query that picks the latest observation per
(identity, project, role). Mirrors the
`dependency_observations` table from entity-model-v2.

### Maintainer relationships (current state, derived)

A view over `contribution_observations`:

```sql
CREATE VIEW current_maintainerships AS
SELECT
    co.identity_id,
    co.project_id,
    co.role,
    co.commit_count,
    co.first_commit,
    co.last_commit,
    co.observed_at,
    julianday('now') - julianday(co.last_commit) AS days_since_last_commit
FROM contribution_observations co
INNER JOIN (
    SELECT identity_id, project_id, role, MAX(observed_at) AS latest
    FROM contribution_observations
    GROUP BY identity_id, project_id, role
) latest ON co.identity_id = latest.identity_id
        AND co.project_id = latest.project_id
        AND co.role = latest.role
        AND co.observed_at = latest.latest;
```

A view rather than a table because it's pure-derivation; can be
recomputed at any time.

### Identity-specific signals

The existing `signals` table already carries identity-related
signal types (e.g., `identity_domain_consistency` from the registry).
Identity entities just become a new shape that signal collectors
target. The signal types relevant to identity entities:

- `account_age_days`
- `follower_count`
- `public_repo_count`
- `cross_platform_consistency_score`
- `identity_graph_depth` (mailmap-derived)
- `contribution_continuity` (gaps in commit activity)
- `account_takeover_indicators` (sudden activity change, email
  change, profile-photo change, 2FA disabled, etc.)

No schema change needed for these — they're rows in the existing
`signals` table with `entity_id` pointing at an `identity:` entity.

## Identity collector

A new `internal/signal/identity/` package, structured like
`internal/signal/github/`:

```go
type Collector interface {
    // Collect identity signals for a given identity entity.
    // Calls the relevant platform API (GitHub for now; more later).
    CollectIdentity(ctx context.Context, entity *profile.Entity)
        (*signal.CollectionResult, error)
}
```

For GitHub identities, this is a thin wrapper over the user-profile
API plus contribution lookups. For email-only or blog-only
identities, the collector either no-ops (no signals available) or
emits a "platform-unknown" absence signal.

## Integration with ingestion

The ingestion plan (`design/ingestion-plan.md`) loads
AnalystOutput documents into the DB. Identity entities should be
created/updated at this time, in a *post-ingest enrichment pass*:

1. Ingest the AnalystOutput into the new analyst-output tables.
2. Walk the document for identity references:
   - `Attribution.AnalystID` — only relevant for team identities,
     skip for now
   - `Citation.commit_sha` if commits link to authors via the
     `git log` of the target repo (requires local clone access)
   - Heuristic: scan `Conclusion.rationale` and `Observation.body`
     prose for backtick-quoted GitHub usernames (e.g., `\`nvbn\``,
     `\`@ellie\``)
3. For each identified handle, ensure an identity entity exists.
4. Optionally trigger an identity-collector run for newly-discovered
   identities (rate-limit-aware).
5. Create `contribution_observations` rows linking identity →
   project → role, where the role can be inferred (author,
   co-maintainer, lead) from the conclusion/observation context.

The heuristic step (#2) is fuzzy by design. Worth flagging
discoveries as `confidence: inferred` until manual or programmatic
verification raises them to `asserted` or `verified`.

## Use cases this enables

After implementation, agent-facing queries that aren't currently
possible:

```
signatory show-identity identity:github/ellie
  → consolidated profile: account age, follower count, all linked
    identities, all projects she's contributed to, all signals
    attributed to her

signatory list-projects --maintained-by identity:github/ellie
  → cross-project query — what does she touch?

signatory list-identities --project=pkg:cargo/atuin --role=co-maintainer
  → who's actively maintaining atuin right now?

signatory burn identity:github/{compromised} --reason=...
  → automatic retroactive degradation across all linked
    contribution_observations and the signals/conclusions that cite
    them. Audit-logged with full traversal scope.

signatory show-identity-graph identity:github/ellie
  → linked identities (verified/asserted/inferred) — the audit-
    trail data that justifies our trust assessment of her

signatory query 'identities with cross_platform_consistency < 0.3 \
  who became co-maintainers in the last 30 days'
  → the XZ-attack early-warning query
```

That last query is the trust-model's whole point. Currently we
can't run it.

## Phasing

### Phase 1 — Schema + manual identity creation (1 day)
- Migration v5: `identity_links`, `contribution_observations`
  tables + the `current_maintainerships` view
- `internal/store/` Go API for identity entities and links
- CLI: `signatory identity create <uri>`, `signatory identity link
  <from> <to>`, `signatory identity show <uri>`
- Tests: round-trip ellie's three-email mailmap as identity links

### Phase 2 — GitHub identity collector (1-2 days)
- `internal/signal/identity/github/` package
- Signal types registered in the registry
- CLI integration: `signatory analyze identity:github/ellie`
  produces an identity-only analysis (no project crawl)

### Phase 3 — Ingestion enrichment (1 day, depends on Phase 1
of ingestion plan)
- During AnalystOutput ingestion, scan for identity references
  and create entities + contribution_observations
- The fuzzy prose-scanning heuristic
- A `signatory enrich-identities <output_id>` command for
  manual re-runs

### Phase 4 — Burn propagation (1-2 days)
- The `signatory burn identity:...` command becomes
  graph-traversal over contribution_observations + linked signals
- Audit log records the propagation scope
- The `current_maintainerships` view starts filtering burned
  identities

### Phase 5 — Query layer (later, depends on MCP work)
- The agent-facing queries above as MCP endpoints
- Cross-project identity dashboards

## Open questions

These are real unknowns that need resolution before or during
implementation. Recording so they don't get glossed over.

### How do we identify the same person across platforms?

The strong signals are:
- Same email used in multiple platforms' profile fields
- `.mailmap` entries (project-asserted equivalence)
- GitHub profile's blog/social links pointing to the same domain
  as the email
- Self-assertion in a public bio ("I'm @ellie on GitHub and
  ellie_huxtable on Twitter")
- Cryptographic: signed commits by the same key under different
  email addresses

The weak signals are:
- Same name on two platforms (high false-positive rate; many
  "Vladimir Iakovlev"s exist)
- Same profile photo
- Inferred from prose mentions ("ellie@atuin.sh wrote..." → links
  identity:email/ellie@atuin.sh to identity:github/ellie if
  ellie's GitHub email is the same)

Confidence levels above (`asserted` / `verified` / `inferred`)
are the schema's representation of this. The hard part is the
collector logic that produces them.

### Privacy and opt-out

Signatory tracks real people. What's the policy?

- **Public data only.** All signals come from public APIs (GitHub
  profile, public commit history). No scraping of private data.
  This is the current implicit policy; should be made explicit.
- **No PII beyond what's already public.** We don't combine
  data from leaked databases, don't track users across sites, etc.
- **Opt-out.** What does it mean for a maintainer to ask "stop
  tracking me"? At minimum, a `burn identity:... --reason=opt-out`
  that hides them from queries. But the data was public; we can't
  unsee it.
- **Internal identities.** Per entity-model-v2, internal-realm
  identities (e.g., `identity:internal/sarah.chen`) are
  organization-private. Different policy entirely — not public,
  not exported.

This deserves a `design/privacy-policy.md` of its own. Not blocking
implementation but should be addressed before any analysis tool
publicly cites identity data.

### Identity continuity vs. account takeover

If `identity:github/ellie` suddenly starts emitting commits with
substantially different patterns (different email, different
working hours, different code style), is that:
- Account takeover (degrade trust immediately)
- Job change / personal life change (do nothing; legitimate)
- Identity theft / impersonation (degrade and investigate)

The signal exists in the `account_takeover_indicators` shape, but
calibrating it is a real research question. Conservative default:
flag for human review rather than auto-degrade.

### Cross-organization migration

Conrad Ludgate left TrueLayer for personal email in 2019. That's
captured in the .mailmap as identity continuity (one person, two
emails). But what about the organization-affiliation signal? While
he was at TrueLayer, his work carried "TrueLayer" institutional
affiliation; afterward, "personal." The trust signal changed even
though the identity didn't.

Schema-wise, this is solvable via time-windowed organization
links: `identity:github/scorphus` had `affiliation:org:truelayer`
during 2017-2019, `affiliation:org:none` thereafter. But we don't
have an `affiliation` link type yet. Worth adding when the use
case becomes concrete.

### Synthesis-time identity extraction

The ingestion enrichment pass scans Conclusion.rationale prose for
identity references. That's a heuristic with both false positives
("ellie" as a generic word) and false negatives ("the maintainer"
unattributed). Two refinements worth considering:

- **Analyst-emitted identity references.** Add a structured field
  to AnalystOutput (or to Observation) for explicit identity
  references: `identifies: ["identity:github/ellie"]`. Future
  schema change to `internal/exchange/`.
- **LLM-driven extraction at ingestion time.** Run the prose
  through a smaller-model identity-extractor on ingest. More
  reliable than regex but adds a per-ingest LLM call.

Both deferred for v1; current plan is regex-based extraction with
all results flagged `confidence: inferred`.

### Burn propagation depth

When we burn `identity:github/ellie`, the immediate effect is on
direct contribution_observations. But there's a transitive
question: a project ellie reviewed and merged a PR for — does
*that* PR's signals get degraded? Does the project's overall
posture get re-evaluated?

Trust-model.md principle #3 says yes ("re-evaluating a window of
history"), but the implementation policy needs to be explicit. The
proposal: burn propagation degrades signals tagged with the burned
identity directly. Project-level posture re-evaluation is a
*recommendation* (the project gets flagged for re-analysis, not
auto-degraded). The user makes the final call.

## Cross-references

- `design/trust-model.md` — principles #1 (composable), #3
  (retroactive degradation), #6 (forgery-resistant signals)
- `design/example-identity-analysis-rsc.md` — worked example of
  the identity-trust analysis this enables
- `design/entity-model-v2.md` — entity types, append-only
  invariants, internal-realm identity discussion
- `design/ingestion-plan.md` — the enrichment pass that creates
  identity entities lives here
- `design/signal-storage-evolution.md` — `identity_domain_consistency`
  signal type already registered; this work makes it queryable
  rather than prose-only
- `internal/profile/entity.go` — `EntityIdentity` already defined
- `internal/identity/` — different concept (team identity for
  audit attribution); not affected by this work
