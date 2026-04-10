# Test Quality Review 3: signatory (Post Entity-Model-v2, Sonnet)

**Date:** 2026-04-09
**Tree state:** `main` at commit `34c3421` (post entity-model-v2 merge)
**Agent:** Sonnet 4.6 (with full project context)
**Scope:** Test quality — full codebase
**Skills invoked:** `cc-skills-golang:golang-testing`, `cc-skills-golang:golang-stretchr-testify`

## Executive summary

The signatory test suite is unusually comprehensive for a young codebase. The store-layer and GitHub-collector tests are genuine, assertive, and security-aware. However, three categories of weakness are material enough to threaten the reliability guarantee that a supply-chain trust tool must provide. First, the `engine` package's tests verify only constructor wiring and would pass if the engine did nothing. Second, several security-sensitive tests use a conditional (`if err == nil { assert... }`) structure that turns a "should always error" expectation into a vacuous pass — corrupted timestamp tests pass whether the store returns an error or silently returns zero. Third, the entire codebase lacks fuzz tests for its two security-sensitive parsers (`NormalizeGitHubRepoInput` and `ParseRepoURL`), and no `TestMain` / goroutine-leak detection exists anywhere. These gaps mean a regression in the trust-signal pipeline could ship undetected.

## Skills invoked

- **`cc-skills-golang:golang-testing`** (mandatory): Surfaced the complete absence of fuzz tests for parser functions, the lack of `t.Parallel()` across all SQLite store tests, missing `TestMain` goroutine-leak guards, and the structural weakness in conditional-body security tests. Used as the primary review lens throughout.
- **`cc-skills-golang:golang-stretchr-testify`**: Surfaced three specific anti-patterns: `assert.Equal(t, sql.ErrNoRows, err)` instead of `assert.ErrorIs`, conditional-body assertions that continue on the wrong branch, and `assert` used where `require` is necessary to prevent nil-dereference panics on subsequent lines.

## Critical findings

### C1. Corrupted-timestamp security tests are vacuously structured — they accept both "error returned" and "zero time returned" as passing

**File:** `internal/store/sqlite_security_test.go:16-65`

**What's wrong:** Both `TestSecurity_CorruptedTimestampReturnsError` and `TestSecurity_CorruptedBurnTimestampReturnsError` use an `if err == nil { assert.False(t, entity.CreatedAt.IsZero(), ...) }` pattern. This means:
- If the store returns an error (good behavior): the inner assertion is skipped — test passes.
- If the store returns `nil` error with a valid (non-zero) timestamp: the inner assertion passes — test passes.
- If the store returns `nil` error with a **zero timestamp** (the silently-swallowed corruption case the test claims to catch): the inner `assert.False(t, entity.CreatedAt.IsZero())` fails.

The test only catches the exact bad case. But crucially, it does not enforce that an error *must* be returned. The test names say "ReturnsError" but neither test enforces that an error is returned.

**Why it's a bug:** A corrupted `burned_at` timestamp in a burn record could mean "was this burn recent?" logic gives the wrong answer, leading an agent to trust a burned package. The test is supposed to be the regression gate for this specific integrity property, but it does not enforce the property it claims to enforce.

**Better test:**
```go
_, err := s.GetEntity(ctx, "corrupt-entity")
require.Error(t, err, "GetEntity with corrupted timestamp must return an error, not a zero-value time")
```

### C2. Engine package tests verify only struct wiring, not behavior — the engine's actual analysis logic has zero test coverage

**File:** `internal/engine/engine_test.go:103-214`

**What's wrong:** Every single test in `engine_test.go` tests only `New(...)` — the constructor. They call `New(s, collectors, ecosystems)` and then assert that the returned struct's exported fields (`engine.store`, `engine.collectors`, `engine.ecosystems`) hold the right values. `Engine` is a 25-line file that does nothing but store its constructor arguments. There is no `Analyze`, `Survey`, or any other method on `Engine` that is tested.

**Why it's a bug:** The engine is used by every CLI command. If the signal collection → entity creation → `AppendSignals` path had a bug, no engine test would catch it.

**Better test:** Tests for any methods `Engine` exposes (e.g., `Analyze(ctx, target, refresh bool)`) against a real or strongly-configured mock store that verifies: entity is created or looked up by URI, signals are appended with the correct entity ID, audit log is written, errors propagate correctly.

### C3. The live test has a latent entity-ID lookup bug that would pass even if the data is stored under the wrong key

**File:** `cmd/signatory/live_test.go:46-51`

**What's wrong:** `TestLive_AnalyzeKong` queries signals like this:
```go
signals, err := s.GetSignals(context.Background(), "alecthomas/kong")
```
But `GetSignals` takes an `entityID` — an internal UUID, not a canonical URI or a GitHub slug. The entity stored by `AnalyzeCmd` will have a UUID, so `GetSignals(ctx, "alecthomas/kong")` will return an empty slice and no error. This is a latent bug: the test would always fail when actually run.

**Better test:** Look up the entity by canonical URI first, then query signals by the UUID `entity.ID`, mirroring the pattern used in `functional_test.go:TestFunctional_AnalyzeRefreshWithMock`.

## High findings

### H1. `assert.Equal(t, sql.ErrNoRows, err)` should be `assert.ErrorIs` — fails on wrapped errors

**File:** `internal/store/migrate_test.go:119, 455, 488`

**What's wrong:** Three migration tests assert `assert.Equal(t, sql.ErrNoRows, err, ...)`. `assert.Equal` uses `reflect.DeepEqual`, not `errors.Is`. If the `database/sql` driver ever wraps `sql.ErrNoRows` in a richer error type, these assertions will fail spuriously. The testify skill explicitly flags this as an anti-pattern.

**Better test:**
```go
assert.ErrorIs(t, err, sql.ErrNoRows, "v1 name column should not exist after v2 up")
```

### H2. All SQLite store tests lack `t.Parallel()` — the entire store test suite runs single-threaded

**File:** `internal/store/sqlite_test.go`, `validation_test.go`, `sqlite_security_test.go`, `migrate_test.go`, `migrate_regression_test.go`

**What's wrong:** Every test uses `newTestDB(t)` which creates an isolated temp-dir database per test. The tests are fully independent — no shared state, no ordering dependencies. Yet zero tests call `t.Parallel()`. By contrast, `store_test.go` (the mockStore contract tests) calls `t.Parallel()` on all 26 of its tests. Adding `t.Parallel()` to the SQLite tests would cut test time significantly and surface any accidental shared state. The `fixedTime` variable in `validation_test.go` is a package-level var — currently harmless but a latent race if tests were parallel.

### H3. `TestCollector_MockSatisfiesInterface` and `TestProvider_MockSatisfiesInterface` test only the mock, not the real implementation

**File:** `internal/signal/collector_test.go:47-52`, `internal/ecosystem/provider_test.go:58-63`

**What's wrong:** These tests verify `mockCollector.Name()` works. The compile-time interface check (`var _ Collector = (*mockCollector)(nil)`) already enforces interface satisfaction at compile time. These tests provide zero signal about whether the real `github.Collector` or any future collector satisfies the contract correctly.

**Better test:** A compile-time check in the `github` package test file is sufficient. Delete the redundant tests.

### H4. `TestFunctional_PostureVersionedGetLatest` asserts `require.NoError` on display calls but never verifies which posture is displayed as "latest"

**File:** `cmd/signatory/functional_test.go:139-157`

**What's wrong:**
```go
// Get with no --version returns the latest (most recent set_at).
require.NoError(t, (&PostureGetCmd{Target: "alecthomas/kong"}).Run(globals))
```
The test comment claims to verify "no --version returns the latest" but the test only checks that `Run()` doesn't error. It doesn't verify which posture is actually returned.

**Better test:** Capture the output and assert the specific version and tier appear in the output, or use the store directly to verify which posture would be displayed.

### H5. `TestCollectionResult_Summary` tests the wrong thing — summary string format, not the count semantics

**File:** `internal/signal/absence_test.go:89-105`

**What's wrong:**
```go
assert.Contains(t, summary, "3 signals")
assert.Contains(t, summary, "1 failures")
```
The test locks in the current mixed-count behavior (2 signals + 1 absence reported as "3 signals") without naming it as intentional. `SignalCount()`, `AbsenceCount()` and `RetryableCount()` methods exist but are not tested against a `CollectionResult` that has both real signals and absences in its `Collected` slice.

## Medium findings

### M1. `TestLogger_WritesToStoreAndFile` opens the file twice with no cleanup

**File:** `internal/audit/logger_test.go:53-75`

Reads the file content, discards it (`_ = content`), then opens it again. Fragile pattern with dead code.

### M2. `TestConcurrentAccess` uses goroutines without synchronization discipline

**File:** `internal/store/sqlite_test.go:846-870`

Technically correct for exactly 10 goroutines and a buffered channel of 10. But if future modification changes the goroutine count to > 10, the channel send will block and the test will deadlock. No `require.NoError` — if the first goroutine returns an error, the test continues instead of stopping.

### M3. `TestNormalizeGitHubRepoInput_Rejects` uses `assert.Error` instead of `require.Error`

**File:** `internal/profile/uri_test.go:174-195`

`assert.Error` means that if the function unexpectedly returns `nil` error (i.e., accepts a malicious input), the test notes the failure but continues. For security-rejection tests, the idiomatic Go pattern is `require.Error` — especially for inputs like `"../etc/passwd"` and `"ale\x00thomas/kong"`.

### M4. `TestCollector_NilEntity` in `collector_test.go` passes on the mock, not the real collector

**File:** `internal/signal/collector_test.go:212-221`

The `mockCollector.Collect` is explicitly written to accept nil entity. But the real `github.Collector.Collect` does `target := entity.URL` and will panic on nil. There's no test for the real collector's nil-entity behavior.

### M5. `TestFunctional_AnalyzeJSONOutput` asserts only `require.NoError` — does not validate JSON is well-formed

**File:** `cmd/signatory/functional_test.go:346-351`

The `--json` flag is supposed to emit machine-readable JSON for LLM agent consumption. This test only verifies the command doesn't error. If `displayProfile` with `--json` produced `{}` or malformed JSON, this test would pass.

### M6. `TestStore_ContextCancellation` tests the mockStore's context check, not SQLite's

**File:** `internal/store/store_test.go:757-802`

Tests the mockStore's explicit `ctx.Err()` guards, not SQLite's driver-level cancellation propagation. There is no equivalent context-cancellation test in `sqlite_test.go`.

## Low / informational

- **L1.** `profile/entity_test.go:TestEntityTypeConstants` — constant-enumeration tests are tautological and also create a maintenance trap (`EntityOrg` is in `entity.go` but not in the test's `types` slice).
- **L2.** `TestMigration_BackupVersionTagUsesIterationVersion` creates two consecutive backups without a delay — timestamp-based filenames could collide silently.
- **L3.** `TestFunctional_BurnListWithEntries` and `TestFunctional_BurnListEmpty` only assert `require.NoError` — don't check content.
- **L4.** No `TestMain` with goroutine-leak detection anywhere. The `Logger` uses a mutex and writes files; the `github.Collector` makes HTTP requests. No `goleak.VerifyTestMain`.
- **L5.** `TestAnalyze_DisplayProfileErrorNotSwallowed` relies on implementation-internal error propagation — fragile.

## Tests worth strengthening

- **Fuzz targets are entirely absent.** `NormalizeGitHubRepoInput` and `ParseRepoURL` are pure string-processing functions at security boundaries with well-defined properties that are fuzzable.
- **`AppendSignals` with invalid JSON value is not explicitly tested** — the FK path is covered but not the JSON-validation path at `sqlite.go:272`.
- **`GetLatestSignals` with both superseded and non-superseded signals across multiple entities** is only tested within a single-entity context.
- **Audit log file permissions** not tested (SQLite DB perms are). `Logger.appendFile` opens with `0600` — no test verifies this.
- **`identity.Current()` is not tested for thread safety.** Reads environment variables and files. Under concurrent parallel-test runs, `t.Setenv` calls could interact.

## Positive observations

1. **Dedicated `_security_test.go` files** in `internal/store`, `internal/signal/github` — excellent structural pattern. Token-leakage, path-traversal rejection, and large-response tests are substantive and would catch real bugs.
2. **Migration tests are exceptionally thorough.** `migrate_test.go` and `migrate_regression_test.go` cover forward/backward migration, data preservation across v1→v2→v1→v2, backup creation, WAL data preservation, future-version rejection, sequential versioning.
3. **`TestFunctional_AnalyzeInputFormsCollapse` directly encodes the anti-fragmentation requirement (#53).** Testing that three equivalent input forms create exactly one entity row and accumulate signals correctly is the correct way to test this invariant.
4. **URI normalization tests cover adversarial inputs well.** Null bytes, path traversal, empty segments, pure whitespace.
5. **Append-only semantics are tested at both the mock and SQLite layers.** `TestStore_AppendSignals_IsAppendOnly` and `TestAppendSignals_IsAppendOnly` both exist. `TestAppendSignals_DuplicateIDFails` verifies the uniqueness constraint.

## Methodology notes

Read every `*_test.go` file in the repository (20 files total), the primary production code for each tested package, and the `go.mod` to understand available libraries. Ran the tests once with `-count=1 -race` to verify they pass and check for race conditions (they pass, no races detected).

Applied the `golang-testing` skill as the primary lens: table-driven structure, parallel tests, fuzz tests, goroutine leaks, assertion quality. Applied the `golang-stretchr-testify` skill specifically for `assert` vs `require` discipline, argument order, and `ErrorIs` vs `Equal` for sentinel errors.

Did not review design documents. The `live_test.go` file is gated behind a `network_access_ok` build tag and was not run, but was read and reviewed structurally. `cmd/signatory/main.go`, `burn.go`, `posture.go`, and `survey.go` not fully read but behavior indirectly covered through functional tests.
