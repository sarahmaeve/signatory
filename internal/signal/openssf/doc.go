// Package openssf collects OpenSSF Scorecard signals for GitHub-hosted
// entities. The Scorecard project (https://scorecard.dev) maintains
// a regularly-refreshed corpus of automated supply-chain hygiene
// scores for public repositories — branch protection, signed
// releases, code review, dangerous workflows, dependency-update
// tooling, etc. The API at api.securityscorecards.dev returns the
// scored result as JSON.
//
// Why this collector exists: prior to it, dispatched analysts hit
// api.securityscorecards.dev directly via WebFetch during /analyze
// runs. That cost tokens (response body), wall-clock time
// (sequential network round-trip), and rate-limit budget every
// time, with no persistence — the score informed the analyst's
// conclusion but evaporated when the session ended. Surfaced by
// dogfood-metrics' "External calls (cache-miss candidates)"
// section across multiple captured sessions.
//
// Public surface:
//
//   - Client — narrow HTTP client for /projects/github.com/{owner}/{repo}
//   - Scorecard — parsed response shape (aggregate score + per-check)
//   - ErrNotFound — sentinel for 404s (project not in Scorecard's index)
//   - Collector — implements signal.Collector for GitHub-hosted entities
//
// The collector emits one signal type:
//
//   - scorecard-check — Hygiene group, ForgeryVeryHigh — the aggregate
//     score plus per-check breakdown. Forgery resistance is high
//     because Scorecard runs out-of-band; the project being scored
//     can't directly forge or suppress the result.
//
// Failure modes are explicit absences rather than silent skips:
//
//   - 404 → absence "not in scorecards index" (retryable=false; the
//     project simply hasn't been picked up by Scorecard's crawler)
//   - 5xx / network → failure with sanitized reason (retryable=true)
//   - non-github entity (no URL) → empty CollectionResult, nil error
//     so the collector can be included unconditionally in dispatch
package openssf
