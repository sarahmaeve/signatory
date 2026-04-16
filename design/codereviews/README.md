# Code Reviews

Adversarial reviews of the signatory codebase. Each review is a point-in-time
assessment against a specific tree state. Conclusions are tracked in GitHub issues
and should be closed as they are resolved — the report files are historical
artifacts, not living documents.

## Reviews

### 2026-04-09 — Post-entity-model-v2 merge

After the entity-model-v2 refactor landed to main (commit `34c3421`),
three adversarial reviews were commissioned to catch regressions,
security issues, and weak test coverage introduced by the large merge.

| # | File | Agent | Skills | Scope |
|---|------|-------|--------|-------|
| 1 | `review-1-post-v2-security-db-opus.md` | Opus 4.6 | `golang-security`, `golang-database`, `golang-safety`, `golang-error-handling` | Security regressions, database usage, weak constructions, weak tests — full codebase |
| 2 | `review-2-post-v2-blind-tests-opus.md` | Opus 4.6 (blind) | `golang-testing` | Test quality — full codebase, no prior context |
| 3 | `review-3-post-v2-tests-sonnet.md` | Sonnet 4.6 | `golang-testing`, `golang-stretchr-testify` | Test quality — full codebase, with context |

Reviews 2 and 3 ran in parallel. Review 2 was deliberately blind (no
priming about prior conclusions or project context) to surface novel
issues a fresh pair of eyes would catch. Review 3 had full project
context and was calibrated toward the same test-quality focus.

All conclusions from these three reviews were filed as GitHub issues
labelled `review-v2-entity` for triage.

## Process notes

- **Skills verification**: Each review explicitly reported which
  `cc-skills-golang:*` skills were invoked. Past experience showed that
  skill-driven review catches issues that generic code reading misses.
  Review 3 (Sonnet + `golang-stretchr-testify`) surfaced testify
  anti-patterns that Review 2 (Opus + testing only) missed — evidence
  that using multiple skills yields higher-quality conclusions.
- **Cross-corroboration**: Conclusions that appeared independently across
  two or more reviews are higher-confidence and should be prioritized.
  Three reviews all caught the corrupted-timestamp security tests
  (sqlite_security_test.go:16-65), giving that conclusion the highest
  confidence level.
- **Complementary depth**: Opus found package-scale structural issues
  (850+ lines of tautological mock-self-testing across three files);
  Sonnet found per-test idiom gaps (`assert.Equal` vs `assert.ErrorIs`,
  missing `t.Parallel()`). Neither alone would have found both classes.
