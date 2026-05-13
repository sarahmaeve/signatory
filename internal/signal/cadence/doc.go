// Package cadence emits the commit_publish_cadence_divergence
// signal — a derived signal computed from sibling collectors'
// last_commit (or last_push) and last_publish emissions read via
// the orchestrator's in-run accumulator.
//
// The signal observes the temporal gap between most-recent push to
// the source repo and most-recent publish to the registry. Four
// shapes:
//
//   - synchronized: |divergence| ≤ 2 days; commit and publish
//     cadence match.
//   - active-repo-paused-publishes: commit_days_ago < publish_days_ago
//     by >2 days; the repo is being touched but no recent publish.
//     Post-incident hardening fits this shape (TanStack 2026-05-12
//     post-cleanup: last_commit=0 days, last_publish=6 days).
//   - active-publishes-fallow-repo: publish_days_ago < commit_days_ago
//     by >2 days; rare. Indicates registry-side activity not
//     mirrored in the source repo.
//   - both-fallow: both commit and publish > 60 days ago.
//     The package may be stable, with a history of many releases,
//     or abandoned, if it has few releases.
//     Trumps the divergence-based
//     classifications: a 90-day-old commit + 95-day-old publish
//     reports "both-fallow", not "synchronized".
//
// The collector reads inputs from the in-run accumulator and emits
// nothing of its own when either input is missing — partial data
// is silent skip, not absence. The signal does not distinguish
// causes; surfacing the pattern is the point. Cause attribution
// belongs at the analyst layer.
//
// When a version_count sibling signal is also in the in-run
// accumulator, the emission carries a prior_version_count field.
// This gives readers consuming the cadence signal in isolation
// (e.g., the deltas view filtered to one signal type) the
// disambiguating context that the same shape value can come from
// operationally-opposite trust postures — a long-running stable
// package on a publish pause versus a thin-history package on a
// pause. The field is silently omitted when no version_count is
// visible, mirroring the collector's posture toward absent inputs.
//
// Origin:
// design/threat-landscape/2026-05-12-tanstack-mini-shai-hulud.md
// §"Empirical: what the current signal model says at T+~21h"
// named the divergence as the first observation the entry did not
// anticipate. Sketched in /tmp/signal-sketch.md sketch 1.
package cadence
