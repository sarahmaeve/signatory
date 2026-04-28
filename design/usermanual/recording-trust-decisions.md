# Recording Trust Decisions

This document covers the commands for recording, retracting, and previewing trust decisions about dependencies: **postures** (how much do we trust this?) and **burns** (this has been compromised).

All commands shown work as of agent-facing-contract milestones M1, M4, and M5 (see [`../agent-facing-contract.md`](../agent-facing-contract.md)).

## Target grammar

Every target-accepting command accepts the same forms:

| Form | Example |
| --- | --- |
| GitHub shorthand | `alecthomas/kong` |
| Full URL | `https://github.com/alecthomas/kong` |
| Canonical repo URI | `repo:github/alecthomas/kong` |
| Package URI | `pkg:npm/express`, `pkg:golang/golang.org/x/mod`, `pkg:cargo/atuin` |
| Versioned package URI | `pkg:npm/invariant@2.2.4`, `pkg:golang/golang.org/x/mod@v0.35.0` |
| pkg.go.dev URL | `https://pkg.go.dev/github.com/alecthomas/kong`, `https://pkg.go.dev/golang.org/x/mod@v0.35.0` |
| npmjs.com URL | `https://www.npmjs.com/package/express` |
| npmjs.com version URL | `https://www.npmjs.com/package/invariant/v/2.2.4` |

A versioned URI is a **distinct identity** from its unversioned root. `pkg:npm/lodash@4.17.21` and `pkg:npm/lodash` can hold independent postures and burns.

## Posture

A posture is your organization's trust decision about a dependency: `vetted-frozen`, `trusted-for-now`, `unexamined`, `unknown-provenance`, or `rejected`.

### Setting a posture

```
signatory posture set pkg:npm/lodash@4.17.21 \
  --tier vetted-frozen \
  --rationale "audited by security team; no install scripts; Ed25519 signatures verified"
```

The `@version` in the URI populates the posture's version automatically — no `--version` flag needed. If you pass both, they must agree (EX_USAGE error otherwise).

### Multi-line rationale (agent-friendly form)

For synthesis-grade rationales that would otherwise need heredoc gymnastics, use `--rationale-file`:

```
# Agent writes the rationale to a file first
# (Claude Code: Write tool; human: any editor)
$EDITOR /tmp/lodash-rationale.md

signatory posture set pkg:npm/lodash@4.17.21 \
  --tier vetted-frozen \
  --rationale-file /tmp/lodash-rationale.md
```

`--rationale-file -` reads from stdin. A single trailing newline is stripped (editors add one); interior blank lines are preserved verbatim.

`--rationale` and `--rationale-file` are mutually exclusive — passing both errors loudly.

### Viewing a posture

```
signatory posture get pkg:npm/lodash@4.17.21
signatory posture get pkg:npm/lodash --all        # every version recorded
```

### Retracting a posture

When a posture turns out to have been premature or wrong (e.g., a CVE disclosure changes the picture):

```
signatory posture unset pkg:npm/lodash@4.17.21 \
  --reason "CVE-2026-12345 disclosed; needs fresh review"
```

The posture row is **soft-deleted** — it stays in the database with the withdrawal metadata filled in, and a future `posture set` on the same target reactivates the row with fresh metadata. Every set / unset / re-set event lands in the audit log, so the decision history is never lost.

### Dry-run

`--dry-run` previews a mutation without writing. Use when you want to verify a posture-set command is shaped correctly before committing:

```
signatory posture set pkg:npm/express@4.18.2 \
  --tier trusted-for-now \
  --rationale-file /tmp/rationale.md \
  --dry-run
```

Output shows what would be written; the store is untouched.

## Burn

A burn records that an identity is compromised and its trust signals should be degraded. Burns target the URI the caller supplied — `burn add pkg:npm/X@2.2.4` burns only that version-identity; `burn add pkg:npm/X` burns the root. The two are independent.

### Recording a burn

```
signatory burn add pkg:npm/invariant@2.2.4 \
  --reason "orphaned tag; commit not reachable from master"
```

Or for a wholesale package burn (compromised maintainer scenario):

```
signatory burn add pkg:npm/compromised-package \
  --reason "maintainer account takeover confirmed; see issue XYZ"
```

Multi-line reasons use `--reason-file`, same mechanism as `--rationale-file`.

### Retracting a burn

When a burn turns out to have been a false positive:

```
signatory burn remove pkg:npm/invariant@2.2.4 \
  --reason "false positive; tag was fine after all"
```

Same soft-delete semantics as posture: the row stays in the database with withdrawal metadata, and a future `burn add` on the same target reactivates the burn.

### Listing active burns

```
signatory burn list
```

Withdrawn burns are excluded from this list by default.

### Dry-run

```
signatory burn add pkg:npm/X \
  --reason "preview only; double-checking the flag shape" \
  --dry-run
```

## Exit codes

| Code | Meaning | When |
| --- | --- | --- |
| 0 | Success | Operation completed |
| 1 | Runtime error | Database error, I/O error, network error |
| 64 | EX_USAGE | Bad invocation: flag conflict, unknown target form, missing required input |

Scripts can branch on exit code 64 to distinguish "I passed bad flags" from "signatory had a runtime problem" without parsing stderr.

## Analyze output

When you run `signatory analyze <target>` on an entity with an active burn, the output surfaces the burn prominently:

```
*** BURNED: <reason> (by <actor>, <date>) ***
```

This appears after the posture section and before the signal groups. A future `signatory summary` verb will provide a richer cross-cutting view of posture + burn + analysis rollup in one call.

## Audit trail

Every posture set / unset / burn add / remove lands in `~/.signatory/audit.log` as a structured entry. The audit log is append-only — no mutation possible even via direct SQL. This is where decision history lives; the `withdrawn_at` columns on postures and burns are the fast-lookup shadow, not the authoritative record.

Actor identity comes from `signatory identity` (team-configured) and is recorded with every mutation.

## Related

- [`../agent-facing-contract.md`](../agent-facing-contract.md) — the design doc defining the invariants this command set satisfies
- [`../posture-relationships.md`](../posture-relationships.md) — how posture scope interacts with version identity
- [`../trust-model.md`](../trust-model.md) — what the tier labels mean in terms of trust semantics
