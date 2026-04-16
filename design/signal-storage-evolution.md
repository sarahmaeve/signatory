# Signatory: Signal Storage Evolution

## Status

Design note — proposed migration `v4` for the signal storage schema.
Motivated by gaps surfaced during the atuin analysis (see
`design/analysis/atuin.md`) and, more generally, by the expectation
that signatory will be applied to many ecosystems beyond Go.

Pending approval before implementation.

## Motivation

The current signal storage (per `migrate.go:61`) carries a **single
scalar `TEXT` value** per observation:

```sql
CREATE TABLE signals (
    id                 TEXT PRIMARY KEY,
    entity_id          TEXT NOT NULL REFERENCES entities(id),
    type               TEXT NOT NULL,
    signal_group       TEXT NOT NULL,
    source             TEXT NOT NULL,
    forgery_resistance TEXT NOT NULL,
    value              TEXT NOT NULL,
    collected_at       TEXT NOT NULL,
    expires_at         TEXT NOT NULL
);
```

That shape was right for v0.1 — it kept the query surface simple, made
append-only enforcement trivial, and mapped cleanly to the signals in
`design/signals-v01.md` (most of which are scalars: stars, commit date,
maintainer count).

The atuin analysis exposed a class of signals where a scalar value is
lossy:

- **cargo-deny `advisory_suppressions`**: `[{advisory: "RUSTSEC-2022-0093", rationale: "not using paseto public key path"}, ...]` — a list of structured entries. Stringifying loses the rationale → advisory binding that makes this a positive signal.
- **`release_tooling`**: `{kind: "cargo-dist", version: "0.31.0", workflow_path: ".github/workflows/release.yml"}` — a small record.
- **`community_health`**: `{percentage: 75, missing_files: ["security_policy"]}` — percentage + list of missing pieces.
- **`identity_domain_consistency`**: `{github_email: "ellie@atuin.sh", project_domain: "atuin.sh", blog_domain: "ellie.wtf", email_project_match: true}` — a small graph of relationships.
- **`commit_activity_shape`**: a curve or shape summary (accelerating / flat / bursty), not a single number.

And a parallel need that also surfaced: **evidence retention for
auditability**. When we claim "atuin has cargo-deny configured," we
should be able to show the literal `deny.toml` content we parsed, on
the date we parsed it, with integrity checking on our own stored copy.
That evidence is often too large and too variable-shape for a scalar
column.

## Proposal

Two changes, both landing as migration `v4`.

### 4.1 Add an optional `details` JSON column to `signals`

```sql
ALTER TABLE signals ADD COLUMN details TEXT NOT NULL DEFAULT '';
```

Semantics:

- `value` remains the canonical scalar for the signal — what you'd
  show in a table or index on. For `advisory_suppressions` this could
  be `"2"` (the count). For `release_tooling` it could be
  `"cargo-dist"` (the kind).
- `details` holds a JSON document with the structured form. Empty
  string `""` means "no structured detail available" (or the signal
  doesn't use one). Using empty string rather than NULL keeps the
  NOT-NULL invariant consistent with the rest of the schema.
- Queryable via SQLite's JSON1 extension, which is built into the
  modernc driver's bundled SQLite (3.51+). Example:
  `SELECT entity_id FROM signals WHERE type = 'advisory_suppressions' AND json_array_length(details) > 0`
- No impact on append-only enforcement. The triggers installed in
  migration v3 fire on UPDATE/DELETE regardless of which columns the
  statement touches.

**Why JSON text, not BLOB:** human-readable stored format matches the
rest of the schema (TEXT-heavy, timestamps as RFC3339 strings, etc.),
supports SQLite's JSON1 operators directly, and keeps the evidence
vs. details distinction clean — binary storage belongs in the
evidence table below, not on the hot signal row.

**Why optional, not required:** most signals genuinely are scalars.
Forcing a JSON structure on `stars` or `last_commit_date` would be
overhead for no information gain. The registry (per
`design/signal-type-registry.md`) can declare which signal types use
`details` and what shape is expected — giving LLMs and other
consumers a schema to reason against.

### 4.2 Add a `signal_evidence` table for raw source material

```sql
CREATE TABLE signal_evidence (
    id           TEXT PRIMARY KEY,  -- UUID
    signal_id    TEXT NOT NULL REFERENCES signals(id),
    kind         TEXT NOT NULL,     -- 'file_snippet', 'api_response_fragment', 'commit_sha_range', 'tag_sha_map', ...
    origin       TEXT NOT NULL,     -- e.g. 'github-api:repos/atuinsh/atuin/contents/deny.toml@HEAD', 'npm-registry:express@4.18.2'
    content_hash TEXT NOT NULL,     -- sha256 of content, hex-encoded
    content      BLOB NOT NULL,     -- raw bytes (caller may gzip; `kind` may imply encoding)
    captured_at  TEXT NOT NULL
);

CREATE INDEX idx_evidence_signal ON signal_evidence(signal_id);
CREATE INDEX idx_evidence_kind ON signal_evidence(kind);
```

Also append-only, enforced by the same trigger pattern as v3:

```sql
CREATE TRIGGER signal_evidence_no_update BEFORE UPDATE ON signal_evidence
    BEGIN SELECT RAISE(ABORT, 'signal_evidence is append-only'); END;
CREATE TRIGGER signal_evidence_no_delete BEFORE DELETE ON signal_evidence
    BEGIN SELECT RAISE(ABORT, 'signal_evidence is append-only'); END;
```

Semantics:

- One signal row can have zero or more evidence rows attached. Most
  won't have any (no one needs to retain the raw 3 bytes that say
  `"29152"` for a star count).
- Evidence is for signals whose credibility depends on being able to
  reproduce the source. `deny.toml` presence, tag-SHA stability
  history, `package.json` snapshots, PASETO attestation payloads.
- `content` is `BLOB` (not `TEXT`) deliberately — evidence may be
  binary (attestation signatures, tarball fragments, gzipped API
  JSON). `kind` tells consumers what to do with the bytes; `origin`
  tells them where they came from.
- `content_hash` lets us verify integrity of our own stored evidence
  at audit time. If someone (including a compromised signatory) edits
  the blob directly in the DB file, the hash mismatch catches it.
  Hash is computed over the pre-compression bytes if the caller
  compresses — but the hash format should be standardized so
  downstream consumers can verify without needing collector
  internals. An explicit convention: `content_hash` is sha256 of the
  **stored bytes as-is**, whatever encoding/compression the caller
  chose. If we later need uncompressed-content hashing, it becomes a
  separate column rather than ambiguating this one.
- `origin` is a free-form human-readable string rather than a
  structured foreign key. Evidence origins are heterogeneous (API
  endpoints, file paths at commit SHAs, S3 URLs, registry tarball
  coordinates) and coercing them into a normalized table adds schema
  churn for limited query value. If a specific origin type earns its
  own table later, it can be added without migrating existing rows.

## What this unlocks

**For the signals surfaced in the atuin analysis** (and similar
ones we'll hit as we extend to more ecosystems):

| Signal | `value` (scalar) | `details` (JSON) | `signal_evidence` |
|--------|------------------|------------------|-------------------|
| `advisory_suppressions` | `"2"` (count) | `[{id, rationale}, ...]` | `deny.toml` file content |
| `release_tooling` | `"cargo-dist"` | `{kind, version, workflow_path}` | release workflow YAML |
| `community_health` | `"75"` (%) | `{percentage, missing_files}` | GitHub community-profile JSON |
| `identity_domain_consistency` | `"3-of-3"` (match count) | `{emails, domains, matches}` | commit authorship excerpts |
| `commit_activity_shape` | `"accelerating"` | `{yearly_counts, slope}` | commit-count paginated API responses |
| `unsafe_code_posture` | `"forbid"` | `{by_crate: {...}}` | `lib.rs` attribute excerpts |
| `trusted_publishing_status` | `"oidc"` or `"token"` or `"unknown"` | `{provider, workflow_ref}` | release workflow YAML snippet |

**For the deferred signals in `signals-v01.md`** that also need
structure:

- `git_tag_sha_stability` — `value`: `"stable"` / `"changed"`;
  `details`: `{tag -> {sha, first_seen, last_seen}}`; evidence: the
  paginated tag API responses across observations.
- `source_code_divergence_vs_git_tag` — `value`: `"divergent"`;
  `details`: `{tag, registry_sha_hint, git_sha, delta_files}`;
  evidence: a diff summary.
- `postinstall_hook` (npm) — `value`: `"present"` / `"absent"`;
  `details`: the hook command string; evidence: the `package.json`
  snippet.
- `pth_file_presence` (PyPI) — `value`: `"present"` / `"absent"`;
  `details`: `{paths, sizes}`; evidence: file listing.

## New signal types to register

Independently of the storage change, several signal types surfaced
by the atuin analyses should be added to the registry
(`design/signal-type-registry.md`). The first block came from the
initial GitHub-API-based pass; the second block came from the
local-clone deep dive on the same day (see
`design/analysis/atuin.md` §"2026-04-14 Extended").

### From initial API-based analysis

| Type | Group | Forgery resistance | Weight (1-10) | Polarity |
|------|-------|-------------------|---------------|----------|
| `supply_chain_policy_config` | hygiene | medium-declining | 6 | positive |
| `advisory_suppressions` | hygiene | medium-declining | 5 | contextual (count alone is noise; rationale presence is the real signal) |
| `unsafe_code_posture` | hygiene | medium-declining | 6 | positive |
| `release_tooling` | publication | medium | 6 | positive (standardization reduces ad-hoc release compromise risk) |
| `commit_activity_shape` | vitality | medium | 6 | contextual (acceleration is positive, deceleration is negative, flat is context-dependent) |
| `community_health_score` | hygiene | medium-declining | 4 | positive |
| `identity_domain_consistency` | governance | high | 8 | positive |
| `hosted_service_coupling` | criticality | n/a | 5 | amplifier (parallel trust decision, not a signal about the code) |
| `effective_maintainer_concentration` | governance | medium | 6 | negative (bus-factor risk, even when org-owned) |
| `crates_io_trusted_publishing` | publication | very-high | 10 | positive (Rust analog of npm trusted publishing) |

### From local-clone deep dive

| Type | Group | Forgery resistance | Weight (1-10) | Polarity |
|------|-------|-------------------|---------------|----------|
| `per_developer_commit_signing_ratio` | governance | high | 8 | positive (fraction of recent commits signed by the committing author, not by GitHub's web-flow key) |
| `web_flow_signing_ratio` | governance | medium | 5 | positive — but **distinct** from the above. GitHub's `verified: true` conflates them; signatory should separate. A project with 100% web-flow signing and 0% per-developer signing is a different profile than 100%/100%. |
| `tag_signing_status` | publication | high | 8 | categorical: `signed_annotated` / `annotated_unsigned` / `lightweight`. Lightweight-only (like atuin) is a gap for any publishing project. |
| `ci_supply_chain_gate` | hygiene | medium-declining | 7 | positive. Whether a declared supply-chain policy (deny.toml, .cargo-audit-ignore, govulncheck config) is actually invoked in a CI job. Policies that exist but aren't gated are weaker than gated ones. |
| `build_provenance_attestation` | publication | very-high | 9 | positive. Sigstore/SLSA-style artifact attestations (e.g., `actions/attest-build-provenance`). Distinct from both tag signing and registry trusted publishing — attests the build pipeline, not the source or the registry publish. |
| `identity_graph_depth` | governance | very-high | 9 | positive. `.mailmap`-derived count of confirmed identity mappings. Corporate→personal email migrations across multi-year windows are a sub-signal worth its own weight — expensive to fabricate across multiple contributors. |
| `self_updater_present` | criticality | n/a | 4 | amplifier — continuous attack surface. Combined with `build_provenance_attestation` it's moderated; alone it's a concern. |
| `third_party_install_inputs` | publication | medium | 5 | negative. External scripts/binaries pulled during `install.sh` (or equivalent) beyond the package manager. E.g., atuin curls `rcaloras/bash-preexec` from GitHub master during install. Not covered by the language package manager's supply-chain controls. |
| `ai_agent_runtime_capability` | criticality | n/a | 6 | amplifier. Project can execute LLM-generated commands under a permissions model. Expands blast radius to include AI-generated command execution; the permissions subsystem becomes a security-critical module in its own right. |
| `registry_publish_origin` | publication | very-high | 10 | categorical: `oidc_ci` / `long_lived_token_ci` / `local_maintainer_machine` / `unknown`. OIDC-in-CI is the gold standard; local-machine publishing is the lowest trust tier. For atuin, it's local-maintainer-machine. |
| `ci_action_pin_tightness` | hygiene | medium | 5 | categorical per action: `sha_pinned` / `major_version_pinned` / `master_pinned` / `unpinned`. Aggregate to a project-level distribution. Major-version pinning is typical baseline; SHA-pinning is the hardened position. |

### From external security review (code-path grounded)

These signals came from a separate security-focused agent review of
the same atuin commit, preserved in
`design/analysis/atuin-security-review-external.md`. They are the
class of signals discoverable only by reading source code — the
provenance model alone doesn't surface them.

| Type | Group | Forgery resistance | Weight (1-10) | Polarity |
|------|-------|-------------------|---------------|----------|
| `unbypassable_hosted_callback` | publication | high | 8 | negative. Hardcoded phone-home endpoints (URLs in HTTP-client code, not configurable) that fire regardless of user configuration. atuin's `update_check → api.atuin.sh` is the documented example. |
| `documented_unbypassable_callbacks` | governance | very-high | 9 | negative. Subset of the above, where the hardcoded callback is explicitly justified in a merged PR or docs. Distinguishes "oversight" (might get fixed) from "design intent" (won't). atuin's PR #3301 ("Call ensure_hub_session even if primary sync endpoint is self-hosted") is the example. This is worse than undocumented because it won't be fixed without a policy change. |
| `default_on_risky_features` | governance | medium | 6 | negative. Risky features (network, tool execution, data collection) that default to "yes" in interactive setup flows. Measures whether the project biases defaults toward capability or toward least-privilege. atuin's `atuin setup` defaults `ai.enabled = Y` and `daemon.enabled = Y`. |
| `secret_file_permission_hygiene` | hygiene | medium | 5 | positive when present. Whether the project explicitly chmod's sensitive files (keys, auth tokens, session caches) to `0o600` on creation, rather than relying on umask. Binary or per-file. Source-grounded signal. |
| `local_ipc_auth_mechanism` | hygiene | high | 7 | categorical per platform: `uds_filesystem_perms` / `named_pipe_acl` / `tcp_with_token` / `tcp_with_tls_mtls` / `tcp_unauthenticated`. atuin on Windows is `tcp_unauthenticated`. The security impact depends on multi-tenancy of the host. |
| `remote_tool_call_surface` | criticality | n/a | 7 | amplifier, structured. For AI-integrated projects: enumerate the client-side tools a remote server can request (read_file, exec, etc.), the permission model that gates them, and the default stance when no rule matches. This is the concrete form of `ai_agent_runtime_capability` — don't just say "it's an agent runtime," say what the agent can do. |
| `capability_allowlist_enforcement` | hygiene | high | 7 | positive when present. Does the client reject server-advertised tool/capability calls it doesn't know? atuin's `stream.rs:292-311` does; projects without this accept arbitrary server expansion of tool surface. |
| `data_minimization_policy` | hygiene | medium-declining | 5 | positive, with nuance. Project-side redaction of secrets before storage or sync. Coverage is enumerable (regex list). Partial coverage is weaker than no coverage marketed as comprehensive — false-positive trust is worse than no trust. atuin's `secrets_filter` catches major cloud-provider formats but misses DATABASE_URL, generic bearer tokens, and unknown-named env vars. |
| `plugin_discovery_by_path` | hygiene | medium | 4 | contextual. Tools that spawn `${tool}-${subcmd}` from PATH (git, kubectl, brew, atuin). Standard pattern, but means typos → execution and PATH poisoning → hijack. Worth flagging for user awareness; not inherently a trust failure. |
| `temp_file_predictability` | hygiene | medium | 4 | negative when predictable, positive when randomized. Temp-file paths that are guessable by a local attacker + a shared `/tmp` enable TOCTOU / pre-creation attacks. Low severity in most cases but a pattern worth catching. |
| `shell_allowlist_sandbox_claim` | hygiene | high | 7 | negative when claimed, neutral when not. Projects that gate shell-tool execution with an allowlist like `Shell(git *)` are implicitly claiming sandbox properties the allowlist doesn't deliver — the target tool's own plugin/extension/alias mechanism composes with the allowlist into broader execution. Flag explicitly so users aren't over-trusting. |

### From security-review round 2 (dual-analyst handoff)

These signals came from the security analyst's second pass (see
`design/analysis/atuin-security-review-external-round2.md`) answering
code-verification questions from signatory's follow-up. Two of them
reframe a single existing signal into multiple; two are genuinely new
surfaces. The first one is the highest-leverage — it materially
downgraded a prior threat assessment.

| Type | Group | Forgery resistance | Weight (1-10) | Polarity |
|------|-------|-------------------|---------------|----------|
| `ai_capability_gating_model` | hygiene | high | 9 | categorical + positive when hardened. Values: `hardcoded_enum` / `config_driven` / `server_advertised` / `none`. For AI-agent-integrated projects this determines whether a compromised server can reach arbitrary client-side tools. atuin ships `hardcoded_enum` (positive, defense-in-depth) — the reviewer's round-2 trace through `tools/mod.rs`, `stream.rs`, and `settings.rs` is the reference implementation of how to verify this. |
| `sync_integrity_protection_split` | publication | high | 8 | positive when complete. **Replaces/refines the existing "E2E encrypted sync" signal, which conflated three distinct properties.** Sub-values: `confidentiality_protection` (per-record encryption), `per_record_integrity_protection` (can server tamper with record content?), `sequence_integrity_protection` (can server silently censor or reorder records?). atuin is **yes / yes / no** — censorship is possible because the client counts received records, not idx values, and persists local-max-idx as the sync high-water-mark. |
| `silent_privilege_escalation_via_env_var` | hygiene | high | 7 | negative. Environment variables that silently widen trust surface when set (disable TLS, bypass auth, add capabilities, enable debug-only tools in production builds). Structured list rather than boolean. atuin has `ATUIN_AI__ADDITIONAL_CAPS`. Worth enumerating across a project because these are classic "hardcoded-dev-knob leaked into production" vectors. |
| `env_inheritance_policy_on_spawn` | hygiene | medium | 5 | negative when absent, contextual when deliberate. For projects that spawn subprocesses (AI tools, shell execution, build steps, test runners), whether `env_clear` / equivalent is called before spawn. atuin's AI shell tool deliberately does not clear (needs SSH agent). Signal polarity depends on whether the spawn is user-initiated-but-arbitrary (atuin's case, acceptable) or runs-server-controlled-code (sandbox failure). |
| `analyst_self_correction` | governance | very-high | 9 | positive. **Meta-signal about the analyst, not the target.** When an analysis round explicitly supersedes a prior round's conclusion based on deeper grounding, that's a signal about analyst quality — analysts that revise their own assessments when the evidence warrants produce higher-fidelity output than ones that defend prior positions. Record as metadata on the analysis, not as a signal on the target. Feeds `design/mcp-dual-analyst-architecture.md`'s schema design. |
| `positive_absence_signal` | hygiene | medium | 5 | positive. Distinct from *unexamined absence*. When an analyst explicitly checks for a known-bad pattern (SQL injection shape, `unwrap` on network boundaries, git-pinned deps) and records "checked, not present," that's a different epistemic state than "not examined." Signatory's signal bookkeeping should distinguish these. Implementation: a `positive_absences` field on analyst output, cross-referenced against the checking-analyst's methodology catalog. |

### From the thefuck synthesis

These two signals were surfaced *only* at synthesis time during the
thefuck dual-analyst engagement (see `design/analysis/thefuck.md`).
Neither raw analyst output contained them; both emerged from
integrating the two perspectives. They are therefore distinct from
the prior signal types in an important way: they are
**synthesis-only** signals that the synthesist role is responsible
for emitting, not the per-analyst roles.

| Type | Group | Forgery resistance | Weight (1-10) | Polarity |
|------|-------|-------------------|---------------|----------|
| `fallow_status_amplifier` | criticality | n/a | 8 | amplifier (negative-direction). Sibling to `criticality` in mechanism but distinct in driver. When a project's vitality has dropped below a maintained threshold (no commits in 12+ months, no releases in 24+ months, open PRs unreviewed for 6+ months), every other negative conclusion's *effective* severity is amplified because the remediation path is closed. A `low` severity hygiene gap on a maintained project is "watch this on the next release"; the same gap on a fallow project is "permanent until forked." Mechanism: severity-multiplier overlay applied at synthesis time. The thefuck engagement is the reference case — provenance F001 (high-severity vitality conclusion) lifted the effective severity of security F002-F009 from "could be tightened upstream" to "won't be tightened, so account for it now." |
| `dual_analyst_self_confirmation` | governance | very-high | 9 | positive (when present). When both analysts independently report the absence of the same pattern (e.g., security-side "no telemetry" + provenance-side "no hardcoded callbacks in URL grep"), confidence in the absence compounds beyond either alone. The mechanism is information-theoretic: two independent-method false negatives are exponentially less likely than one. The thefuck engagement's "no telemetry" signal is the reference case — security F010 (positive: full source-tree URL grep returned no phone-home) and provenance positive-absence (no hardcoded callback patterns) converged on the same conclusion via different methods. Implementation: synthesist computes the intersection of positive_absences and `severity: positive` conclusions across all analyst inputs, emits one `dual_analyst_self_confirmation` for each pattern that appears in two or more independent analyst outputs. Cross-references all originating conclusions/absences. |

### A meta-observation about signal design

The `per_developer_commit_signing_ratio` vs. `web_flow_signing_ratio`
distinction is the clearest instance of a problem we'll hit repeatedly:
**upstream APIs conflate signals that signatory should keep separate**.
GitHub returns a single `verified: true/false` bit; a commit signed by
ellie's personal key and a commit signed by GitHub's web-flow key both
produce `verified: true`. Trust-model-wise, those are very different.
Signatory's collectors should extract the underlying distinction
(`verification.reason`, `verification.signature`) rather than accepting
the upstream-flattened boolean.

This is a pattern worth capturing as a collector-design principle:
**when an upstream source collapses multiple signals into one flag, our
collector should de-collapse them.**

The external security review generalizes this further: **each analysis
perspective surfaces a class of signals the other perspectives
systematically miss.** Code-reading finds hardcoded endpoints, default
values, file-permission calls, local-IPC posture. Metadata-reading
finds commit-signing distribution, tag types, build-provenance status,
identity-graph depth. A full `signatory analyze` needs both kinds of
collector. See
[`design/mcp-dual-analyst-architecture.md`](mcp-dual-analyst-architecture.md)
for the architectural response to this observation — two specialized
analyst roles, deterministic collectors underneath, caller-chosen
depth.

These weights are all proposals; real weight tuning is downstream work
per `design/signal-type-registry.md` open-question 2.

## Role taxonomy expansion

The existing role taxonomy (Runtime / Validation / Build-only /
Development) was written for libraries. The atuin analysis didn't fit
any of the four. Proposal:

| Role | Scope | Examples | Blast radius |
|------|-------|----------|--------------|
| Runtime | Compiled into production binary | kong, modernc/sqlite | Production compromise |
| Validation | Runs during `go test` / `pytest` / `cargo test` in CI | testify, pytest | CI-secret theft, silent test corruption |
| Build-only | Code generation / linting / formatting at build | protoc, buf, goimports | Injects into build output |
| Development | Editor plugins, local-only tooling | VS Code extensions, pre-commit hooks | Dev-workstation compromise |
| **Shell-augment / user-input capture** | Replaces or wraps a shell, REPL, or input surface | **atuin**, zsh plugins, browser password managers | **Per-keystroke credential harvester if compromised** |
| **Hosted-service coupled** | Binary + SaaS; using the SaaS is a second trust decision | atuin (with hosted sync), 1password CLI, npm with 2FA | Layered — binary compromise OR service compromise |

The new rows are the ones atuin forced us to acknowledge. They are not
strictly mutually exclusive with the original four — a tool can be
"Development" AND "Shell-augment" — so the role may need to become a
set of tags rather than a single-select classification.

## Migration plan (v4)

### Up (v3 → v4)

1. `ALTER TABLE signals ADD COLUMN details TEXT NOT NULL DEFAULT '';`
2. `CREATE TABLE signal_evidence (...)` with schema above.
3. `CREATE INDEX idx_evidence_signal ...` and `idx_evidence_kind ...`.
4. `CREATE TRIGGER signal_evidence_no_update ...` and `..._no_delete`.
5. Register new signal types in the registry (code change, not SQL).

### Down (v4 → v3)

1. `DROP TRIGGER IF EXISTS signal_evidence_no_delete;`
2. `DROP TRIGGER IF EXISTS signal_evidence_no_update;`
3. `DROP INDEX IF EXISTS idx_evidence_kind;`
4. `DROP INDEX IF EXISTS idx_evidence_signal;`
5. `DROP TABLE IF EXISTS signal_evidence;`
6. `ALTER TABLE signals DROP COLUMN details;` (SQLite 3.35+, supported
   by modernc driver per `migrate.go:259`)

Data loss on rollback: all evidence rows and any information in
`details` JSON that isn't also reflected in `value`. Acceptable per
`design/entity-model-v2.md`: rollback is a recovery mechanism, not
a feature.

## Open questions

1. **Evidence size caps.** Should we bound the size of a single
   evidence blob at the DB layer, or is that a collector-layer
   concern? Proposal: collector-layer cap, default 256 KB per
   evidence row. Larger artifacts (e.g., full tarball diffs) belong
   in a content-addressed filesystem store with the DB holding only
   the hash + path. Needs a separate pass.
2. **Evidence retention policy.** Evidence rows grow unboundedly in
   append-only mode. The file-audit-log rotation is "user's
   responsibility" (per entity-model-v2 §4). Same answer here, or do
   we want a per-kind retention window? Leaning toward same answer:
   user's responsibility.
3. **Compression convention.** Should the schema mandate that certain
   `kind` values are gzip-compressed (e.g., `kind = 'api_response_fragment_gz'`)
   or keep compression up to the caller? Mandating in the `kind`
   taxonomy is clearer but adds values to remember. Leaning toward
   caller's choice with the hash-over-stored-bytes convention.
4. **`details` schema enforcement.** We could store a JSON Schema per
   signal type in the registry and validate on insert. That's a lot
   of infrastructure for v0.2. For v0.1 of this change, document
   expected shapes in the registry comments and trust collectors to
   produce correct JSON. Revisit if we see drift in practice.
5. **Should `value` become derivable from `details`?** For signals
   that have both, `value` is just a summary scalar. We could require
   that relationship (value == `json_extract(details, '$.summary')`
   or similar) to prevent skew. Too rigid for v0.1; keep them
   independent with collectors responsible for consistency.
