// Package git implements a read-only signal collector that extracts
// trust signals from a local git clone.
//
// # Why shell out instead of using a Go git library
//
// The collector invokes the system `git` CLI via os/exec rather than
// linking a Go git library such as go-git. See
// design/v0.1-invariants.md §"Invariant 2" for the reasoning: the
// git CLI is already on every dogfood operator's machine, it handles
// signature verification by delegating to the system's gpg/ssh
// toolchain (which we explicitly do not want to own), its output
// formats with %-placeholders are stable across git versions, and
// avoiding go-git keeps the module graph narrow (which the Invariant
// 1 CI check separately enforces for LLM-client SDKs).
//
// # What this collector does and does not do
//
// Does:
//   - Read from an existing local clone at a caller-provided path.
//   - Parse git log / for-each-ref / show output with structured
//     ASCII control-character separators (0x1F / 0x1E) so no commit
//     metadata can collide with a delimiter.
//   - Emit signals via the standard CollectionResult interface, so
//     storage, deduplication, and TTL handling are identical to the
//     github collector's.
//
// Does not:
//   - Perform any network operation. No `git clone`, no `git fetch`,
//     no `git pull`. If the clone is stale, that is the operator's
//     explicit decision to refresh (via `signatory analyze --clone`
//     on a new target, which creates a new clone into a fresh dir).
//   - Write to the clone. No new branches, no commits, no tag
//     creation, no config modification.
//   - Verify commit signatures cryptographically in Go. The `%G?`
//     flag from `git log` reports the verification result that git
//     already computed; this package trusts that and classifies it.
//
// # Signal coverage
//
// Each signal ships in its own commit during the v0.1 build-out. The
// target set is:
//
//   - first_commit_date (Vitality) — from a single-line reverse log.
//   - per_developer_commit_signing_ratio (Governance) — classified
//     from `git log --format=%G?|%GS|%GK`, distinguishing web-flow
//     signatures (GitHub's managed key) from per-developer keys.
//   - web_flow_signing_ratio (Governance) — the counterpart ratio
//     from the same pass, surfaced separately because "high signing
//     but all via the platform's key" is a different trust posture
//     than "high signing across contributor keys".
//   - tag_signing_status (Publication) — per-tag classification:
//     signed-annotated, annotated-unsigned, or lightweight.
//   - identity_graph_depth (Governance) — .mailmap entry count.
//   - identity_domain_consistency (Governance) — distribution of
//     email domains across recent commits.
//   - effective_maintainer_concentration (Governance) — share of
//     recent commits held by the top N contributors.
//
// The default observation window is 12 months; override with
// Collector.WithWindow for targeted analyses.
package git
