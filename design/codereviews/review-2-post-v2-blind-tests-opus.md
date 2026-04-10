# Test Quality Review 2: signatory (Post Entity-Model-v2, Blind)

**Date:** 2026-04-09
**Tree state:** `main` at commit `34c3421` (post entity-model-v2 merge)
**Agent:** Opus 4.6 (blind — no prior context or priming about known issues)
**Scope:** Test quality, correctness, coverage — full codebase
**Skills invoked:** `cc-skills-golang:golang-testing`

## Executive summary

The suite has respectable breadth — 25 test files, ~5,400 lines of test code across every package — but the depth is uneven and contains several anti-patterns that are textbook "tests that would pass on broken code." The most serious problem is pervasive **mock-self-testing**: three packages (`internal/store` contract tests, `internal/signal`, `internal/ecosystem`) have test suites that exercise only mocks defined in the test file itself, producing 800+ lines of tautological "tests" that verify nothing about the production code. A second cluster of issues is **CLI Run-tests that only assert `NoError`** without verifying any output, side-effect, or state change — these would pass even if the command silently did nothing. Other findings include a broken live test that queries by the wrong primary key, a concurrency test whose claim is contradicted by the production code, a few `if err == nil {…}` blocks that skip assertions on the only interesting path, and zero fuzz tests on five hand-rolled parsers that feed directly into trust-decision code.

There are also real strengths — the migration data round-trip, the SSRF rejection tests, the token-leak tests, and the sqlite append-only semantics are genuinely good work — so severity is calibrated against a suite that clearly knows how to write tests when it wants to.

## Skills invoked

- **`cc-skills-golang:golang-testing` (audit mode)** — surfaced: missing `goleak.VerifyTestMain`, missing `t.Parallel()` in every store integration test, missing fuzz targets for parsers, weak table-driven tests that don't exercise enough cases, no build tags for integration vs. unit separation, and the core "tests that mock away the thing being tested" pattern. This was the primary lens for the review.

Did not invoke additional skills beyond `golang-testing` — the anti-patterns in this suite are all within the scope of that one skill. `golang-concurrency` would have added color to finding C5 (the concurrency test), but the diagnosis did not require it.

## Critical findings

### C1. Contract tests against mockStore are tautological — no production code exercised

**File:** `internal/store/store_test.go:350-836` (the entire "Contract Tests" section)

**What's wrong:** `store_test.go` defines a `mockStore` (lines 23-343) that re-implements the `Store` interface as an in-memory map, then calls the mock from tests like `TestStore_EntityRoundTrip`, `TestStore_AppendSignals_IsAppendOnly`, `TestStore_GetLatestSignals_FiltersSuperseded`, and `TestStore_PostureVersionsCoexist`. The "Contract Tests" header (line 345) claims these target "the interface contract, not any specific implementation" and exist so "SQLite and any future implementations behave consistently" — but the mock IS the thing under test, so all that's verified is that the hand-written in-memory mock matches the expectations the test author also hand-wrote. The real `SQLite` implementation is never reached.

**Why it's a bug:** These tests cannot fail unless the mock itself is broken. Mutating `sqlite.go` to invert append-only semantics, drop the superseded-filter from `GetLatestSignals`, or silently overwrite postures would not cause a single one of these tests to turn red.

**Better test:** Delete the mockStore-based tests entirely. Replace with a single `runContractTests(t *testing.T, newStore func() Store)` helper that is invoked both from `TestSQLiteContract` and from any future backend's test file. If you want in-memory tests for speed, promote the mock to a real in-memory implementation in the `store` package that ships as `store.Memory`, and run the same contract suite against it.

### C2. `TestStore_ContextCancellation` tests the mock's explicit `ctx.Err()` check — not the Store contract

**File:** `internal/store/store_test.go:757-802`

**What's wrong:** This test pre-cancels a context and then calls every mock method, asserting each returns `context.Canceled`. That behavior exists because the mock explicitly checks `ctx.Err()` at the top of every method. The real `SQLite` implementation makes no such explicit check — it relies on `QueryRowContext`/`ExecContext` to honor the context at the driver level, and those only check `ctx.Done()` around I/O syscalls.

**Why it's a bug:** The test claims to enforce a contract ("all store methods respect context cancellation") that the real implementation does not satisfy.

**Better test:** Write this test once against the real SQLite store, using a long-running operation where the cancellation actually has a chance to land.

### C3. `internal/signal/collector_test.go` tests the test's own mock

**File:** `internal/signal/collector_test.go:47-222`

**What's wrong:** The `signal` package defines an `interface Collector` and nothing else — no production implementation lives in this package. The test file declares a `mockCollector` and a `contextAwareCollector`, then writes nine tests that exercise these mocks. Every assertion is of the form "mock returns what I told it to return."

**Better test:** The only thing worth testing in this file is the interface compile-time check. Delete the rest. The `CollectionResult` type does have real methods (`Signals`, `SignalCount`, `HasFailures`, `Summary`) — those are worth testing directly and are partially covered in `absence_test.go`.

### C4. `internal/ecosystem/provider_test.go` is 299 lines of mock-self-testing

**File:** `internal/ecosystem/provider_test.go:58-197`

**What's wrong:** Identical structure to C3. The `ecosystem` package defines only an interface (`Provider`) and a struct (`Dependency`) — no real provider implementation lives in the package. The test file defines `mockProvider` and `contextAwareProvider` and writes 10 tests that exercise them.

**Better test:** Delete `TestProvider_*` tests through line 197. Keep the JSON tests at lines 199-299 which are real tests of real code.

### C5. `TestConcurrentAccess` claims to test WAL concurrency but SQLite has `SetMaxOpenConns(1)`

**File:** `internal/store/sqlite_test.go:846-870`

**What's wrong:** The test spawns 10 goroutines that each Put+Get an entity, and the comment says "WAL mode should allow concurrent reads and writes." But `OpenSQLite` sets `db.SetMaxOpenConns(1)` at `sqlite.go:76`, so the database/sql connection pool will serialize every one of those goroutines onto a single connection with no real concurrency.

**Better test:** Either (a) rename the test to `TestSerializedAccessDoesNotDeadlock` and drop the WAL framing, or (b) open multiple *separate* `*SQLite` instances against the same path, each with its own connection, and verify that concurrent writes don't corrupt the file.

## High findings

### H1. `if err == nil { … }` swallows the important assertion in corrupted-timestamp tests

**File:** `internal/store/sqlite_security_test.go:16-37` and `40-65`

**What's wrong:** Both `TestSecurity_CorruptedTimestampReturnsError` and `TestSecurity_CorruptedBurnTimestampReturnsError` insert a row with an invalid timestamp, call the getter, and then do:
```go
if err == nil {
    assert.False(t, entity.CreatedAt.IsZero(), "...")
}
```
If the getter returns an error (current correct behavior), the test passes without asserting anything. There's no `require.Error(t, err)`.

**Better test:**
```go
_, err := s.GetEntity(ctx, "corrupt-entity")
require.Error(t, err)
require.NotErrorIs(t, err, ErrNotFound, "corruption must not masquerade as not-found")
assert.Contains(t, err.Error(), "parse", "error should identify the parse failure")
```

### H2. CLI `TestXxxCmd_Run` tests assert only `NoError` — every command's success path is unverified

**File:** `cmd/signatory/cli_test.go:109-122, 144-151, 186-193, 227-238, 301-312, 314-325, 336-343`

**What's wrong:** Tests like `TestAnalyzeCmd_Run_NoData`, `TestSurveyCmd_Run`, `TestCompareCmd_Run`, `TestBurnAddCmd_Run`, `TestPostureGetCmd_Run`, `TestPostureSetCmd_Run`, `TestVersionCmd_Run` all assert only that Run returns nil. No stdout check, no state verification, nothing. A `return nil` stub would pass.

**Better test:** Delete the `TestXxxCmd_Run` tests that only check `NoError`. The functional tests cover actual success paths. Where a command has behavior worth pinning, capture stdout via a writer field and assert on the string.

### H3. `TestFunctional_Analyze*` tests don't verify any analyzed output

**File:** `cmd/signatory/functional_test.go:274-366`

**What's wrong:** `TestFunctional_AnalyzeRefreshWithMock` verifies signal count, but `TestFunctional_AnalyzeCachedFromMock`, `TestFunctional_AnalyzeNoDataNoRefresh`, `TestFunctional_AnalyzeJSONOutput`, and `TestFunctional_AnalyzeWithPostureAndBurn` only assert `NoError`. `--json` output has no test that parses the JSON. The "cached" path doesn't verify that the second call *actually read from cache* instead of re-collecting.

**Better test:** Capture `stdout`, parse the JSON, assert on fields. For the cached-path test, assert the mock collector's `callCount` shows only one call across two invocations.

### H4. Token-leak test doesn't check Failures slice

**File:** `internal/signal/github/security_test.go:63-74`

**What's wrong:** The test iterates `result.Signals()` (line 69) and skips the `Failures` slice entirely. A regression that leaked the raw error into `Failures` but not into the absence signal would pass.

**Better test:** Extend the loop to also walk `result.Failures` and assert the `Reason` field doesn't contain the token.

### H5. `TestLive_AnalyzeKong` queries `GetSignals` with the wrong ID type — test is impossible to pass

**File:** `cmd/signatory/live_test.go:38-52`

**What's wrong:** Line 49 does `signals, err := s.GetSignals(context.Background(), "alecthomas/kong")` — passing the string short-name as the entity ID. But entity IDs are now UUIDs. This test cannot pass on any correct implementation.

**Better test:** Look up the entity via `s.FindEntityByURI(ctx, "repo:github/alecthomas/kong")` first and use the returned `entity.ID`.

### H6. `TestConcurrentAccess` uses goroutines without race detection in CI

**File:** `internal/store/sqlite_test.go:846-870`

**What's wrong:** Compound with C5. The test spawns 10 goroutines that share one `*SQLite` and mutate the file. There's no project config mandating `-race`. Even if C5 is fixed, without `-race` in CI the test is checking for symptoms only.

**Suggested fix:** Add a CI config that runs `go test -race ./...`.

## Medium findings

### M1. `TestBoundaryAccessorsAreImmutable` is a tautology

**File:** `internal/profile/entity_test.go:59-70`

Calls `PreLLMEnd()` twice and asserts the two values are equal. Since `time.Time` is a value type returned from an accessor over a package-level constant, this is equivalent to `assert.Equal(x, x)`.

### M2. Constant enumeration tests

**File:** `internal/profile/entity_test.go:12-26`, `signal_test.go:12-33, 35-54`, `posture_test.go:12-41`

`TestEntityTypeConstants`, `TestSignalGroupConstants`, etc. verify that manually-listed constants are non-empty and distinct. Neither condition exercises production behavior.

### M3. `TestEngine_New_*` tests are struct-field assignment checks against a 24-line production file

**File:** `internal/engine/engine_test.go:103-214`

214 lines of tests for a 24-line production file. All tests verify that `New()` stored its arguments in the struct's fields. These tests cannot catch any bug that matters.

### M4. `TestCollector_PartialCollection` never asserts which signals are absent vs collected

**File:** `internal/signal/github/collector_test.go:363-443`

Test sets up a complex httptest mux where some endpoints succeed and others rate-limit. Assertions are just `HasFailures` true, `collected > 0`, `absences > 0`. No assertion about *which* signals are collected vs absent.

### M5. `TestCollector_Collect` only checks signal types are present — never values

**File:** `internal/signal/github/collector_test.go:163-195`

Notably absent value checks: `last_push`, `repo_age`, `forks`, `open_issues`, `archived`, `owner_type`, `license`.

### M6. `TestParseGoModDeps` is missing adversarial inputs

**File:** `internal/signal/github/collector_test.go:511-584`

Four test cases. Missing: `replace` directives, retract directives, nested require blocks, tabs vs spaces, Windows line endings, extremely malformed go.mod. No `FuzzParseGoModDeps`.

### M7. `TestSecurity_LargeResponseRejected` test is weak — only checks that *an* error occurs, not which

**File:** `internal/signal/github/security_test.go:221-250`

The test writes an 11MB body of malformed JSON and asserts `client.get` returns an error. But `json.Unmarshal` on 11MB of invalid JSON would also error. If someone deleted the size-limit check, the test would still pass.

### M8. Audit log test doesn't verify file path leaking or permission

**File:** `internal/audit/logger_test.go:33-75`

Does not assert the file's mode is `0600` (which the code explicitly sets). The SQLite test does check `0600` on the DB file. Audit file has no parity check.

### M9. `TestFunctional_AuditLogWrittenOnPostureSet` is the only functional audit test

**File:** `cmd/signatory/functional_test.go:373-396`

Only `set_posture` has an end-to-end audit assertion. `burn`, `analyze` have no equivalent. Missing: test that the `"overwrite": true` flag in the audit detail is written correctly on re-burn (which would catch any regression in the tricky variable shadowing at `burn.go:47-73`).

## Low / informational

- **L1.** No tests exercise `Parse*` for any enum type. The CLI accepts `--tier` strings and casts directly via `profile.PostureTier(cmd.Tier)` with no validation on deserialization.
- **L2.** No fuzz tests at all. Candidates: `ParseRepoURL`, `NormalizeGitHubRepoInput`, `parseGoModDeps`, `parseTotalFromLink`, `parseRateLimitReset`, `detailOrEmpty`.
- **L3.** No `goleak.VerifyTestMain` anywhere. The GitHub collector uses `http.Client` with a 60s timeout.
- **L4.** No build-tag separation of unit vs. integration. SQLite tests run on every `go test ./...`.
- **L5.** `testEntity` helper ignores the `Description` field at `internal/store/sqlite_test.go:32-41`.
- **L6.** `TestSecurity_ZeroValueToSignalDoesNotPanic` only checks non-panic, not the sentinel return.
- **L7.** `TestFunctional_PostureVersionedGetLatest` is three `NoError` assertions in a row at `cmd/signatory/functional_test.go:139-158`.
- **L8.** `TestMigration_BackupVersionTagUsesIterationVersion` mixes two different scenarios in one test.
- **L9.** Migration backup filename uses second-granular timestamp with no `O_EXCL`.

## Tests worth strengthening

- `sanitizeErrorForStorage` has no direct test.
- `isRetryable` has no direct test.
- `NormalizeGitHubRepoInput` has good positive cases but missing Unicode, IDN, long-input stress, SSH gitlab prefix stripping.
- Entity `UpdatedAt` isn't asserted by any test.
- `scanEntity`, `scanSignals`, `scanPosture`, `scanPostureRow` parse-error paths not directly tested.
- `TestAppendResolution_RoundTrip` doesn't verify FK enforcement.
- `auditLog.LogAction` failure never tested in the CLI path.
- `TestFunctional_BurnOverwriteExisting` doesn't check the `"overwrite":true` audit detail.
- No test for `SetBurn`'s inherited+source_org combination interaction with `ListBurns`.
- `BurnListCmd.Run` output format has no test.
- TTL math (`CollectedAt.Add(24 * time.Hour)`) never asserted exactly.
- Migration downgrade loses data but the test does not verify what is explicitly dropped.

## Positive observations

- **`TestMigration_V2DataRoundTrip`** is genuinely excellent. Seeds realistic v1 data, migrates up, verifies the translation, migrates down, verifies v1 is restored, then migrates up again.
- **`TestSecurity_ParseRepoURL_RejectsPathTraversal`** is a tight, focused adversarial test with clear table-driven input.
- **`TestAnalyze_CorruptedEntityErrorNotSwallowed`** shows the TDD-for-security-bugs pattern the project's design docs ask for. Asserts `Error`, not soft `if err == nil { ... }`.
- **`TestAppendSignals_IsAppendOnly`** and **`TestAppendSignals_DuplicateIDFails`** correctly pin the v2 append-only semantic at the real-implementation level.
- **`TestFunctional_AnalyzeInputFormsCollapse`** verifies the issue-#53 deduplication contract by actually counting rows in the database.

## Methodology notes

Worked through every `*_test.go` file in the repository (25 files). For each, read the corresponding production code first to understand what the tests should be verifying, then read the tests to see whether they actually verify it. Grepped for patterns like `if err == nil { assert`, `NoError` without any follow-up assertion, `mock.CalledCount`, and `assert.Equal(x, x)`-shaped tautologies.

Did not run the live tests (build-tag gated), did not run `-race`, and did not invoke mutation testing.

Across 25 files: 9 findings at C/H severity, 9 at M, 9 at L/informational — 27 total. The pattern is not "broken tests" but "tests that look impressive but don't bite." A motivated mutator would find the suite provides substantially less protection than its line count suggests, particularly in `internal/store/store_test.go`, `internal/signal/collector_test.go`, `internal/ecosystem/provider_test.go`, and the seven `TestXxxCmd_Run` tests in `cli_test.go`. Fixing C1–C4 would remove roughly 1,300 lines of low-value tests and clarify what's actually covered.
