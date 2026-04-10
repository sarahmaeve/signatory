# Adversarial Review 1: Signatory Codebase (Post Entity-Model-v2)

**Date:** 2026-04-09
**Tree state:** `main` at commit `34c3421` (post entity-model-v2 merge)
**Agent:** Opus 4.6
**Scope:** Whole codebase — security regressions, database usage, weak constructions, weak tests
**Skills invoked:** `cc-skills-golang:golang-security`, `cc-skills-golang:golang-database`, `cc-skills-golang:golang-safety`, `cc-skills-golang:golang-error-handling`

## Executive summary

The codebase is in a transitional state: the v2 entity model landed with solid intent (UUIDs, append-only, versioned postures, dual-sink audit) but the perimeter between user input and "canonical" internal identifiers is porous, several subsystems are stubs masquerading as working code, and at least one security test is actively asserting the wrong property. Risk posture is **moderate-to-high** for a trust tool because most issues attack the exact property signatory claims to defend: the integrity of what it says about an entity. Across all severity levels: **5 critical, 7 high, 10 medium, 7 low/informational** (29 total), plus 6 test-quality issues and 4 positive observations.

The top three themes: (1) **user input bypasses canonical-URI validation** in `ensureEntity`/`PutEntity` and the rest of the system treats the result as trusted, (2) **append-only is a naming convention, not an enforced invariant** — nothing in the DB or the code blocks UPDATE/DELETE on the append-only tables, and (3) **security tests that silently pass on broken code** — `TestSecurity_CorruptedTimestampReturnsError` accepts both "error returned" AND "no error, zero time" as passing, which is the wrong contract.

Stop-the-press items: C1 (unvalidated CanonicalURI persistence), C2 (append-only tables enforced only by convention), C3 (security tests that pass on the broken behavior they claim to detect), C4 (audit log OpenFile without O_NOFOLLOW on symlink-target path), and C5 (`compare` command is a stub that silently exits 0 — an LLM agent calling `signatory compare a b` gets a reassuring empty output and exits success, which directly defeats the tool's purpose).

## Skills invoked

- **`cc-skills-golang:golang-security`** — surfaced C1 (unvalidated canonical URI as a trust-boundary leak), C4 (O_NOFOLLOW gap on audit log open), H1 (token leak via HTTP redirect scheme mismatch), H2 (path/query injection surface in `GetDirectoryContents`/`GetFileRaw` even though current callers use constants), M2 (log-injection via SIGNATORY_TEAM), and the pattern of storing attacker-controlled strings that will be rendered downstream.
- **`cc-skills-golang:golang-database`** — anchored C2 (append-only enforced by convention only; the skill's rule 14 explicitly says "never rely on application-layer invariants for schema integrity"), H3 (`GetLatestSignals` subquery not entity-scoped — cross-entity leak risk), H4 (symlink follow / TOCTOU on migration backup), M1 (SQLite file mode race: Ping creates file before Chmod), and M3 (migration `schema_version` bootstrap is not transactional so multi-process startup can double-apply). It also flagged the rollback-v2 determinism problem (`INSERT OR REPLACE` without ORDER BY — M4).
- **`cc-skills-golang:golang-safety`** — surfaced H5 (the brittle `err == nil` reuse in `burn.go:73` that flips meaning if someone changes `:=` to `=`), verified that `printCompactValue` map iteration over a nil map is safe (range-over-nil = 0 iterations), and flagged the non-deterministic iteration order as an audit/reproducibility concern.
- **`cc-skills-golang:golang-error-handling`** — surfaced H6 (GitHub client error-body leakage through `%s` on response bodies, which flow to stderr logs), M7 (`strings.Contains(errMsg, "not found")` fragile substring classification), M8 (swallowed errors from `json.Unmarshal` in `displayHuman`), and M9 (`detail, _ := json.Marshal(...)` in analyze/posture/burn — unchecked marshal errors). Confirmed that the `err == nil` burn reuse violates the single-handling/clarity principle.

Did not separately invoke `golang-concurrency`, `golang-context`, or `golang-testing` as dedicated passes — the codebase is mostly sequential and test-quality was covered by parallel reviews 2 and 3.

## Critical findings

### C1. User-supplied `target` flows into `Entity.CanonicalURI` without URI-scheme validation

**Location:** `cmd/signatory/posture.go:249-263` (`ensureEntity`), `internal/store/sqlite.go:151-173` (`PutEntity`)
**Category:** security

**Description:** `ensureEntity` has two code paths. The GitHub path validates input and produces a canonical URI via `NormalizeGitHubRepoInput`. The fallback path takes the raw `target` string — whatever the user typed at the CLI — and stores it verbatim in `entity.CanonicalURI` and `entity.ShortName`. `PutEntity` then only checks that these fields are non-empty (sqlite.go:155); there is no scheme-prefix check, no character allowlist, no length bound. Anything the user types becomes a "canonical" URI that the rest of the system trusts as stable and safe.

**Impact:** An attacker (or an LLM being fed attacker-controlled text) can do `signatory burn --reason x 'pkg:npm/legit AND 1=1--'` or `signatory posture set '\n{"actor":"admin"}' --tier vetted-frozen --rationale x` and get persistent entities with arbitrary strings as their canonical identifier. Concrete exploit paths: (a) the canonical URI is rendered in CLI output unescaped (`analyze.go:186`) and in JSON output — log-injection into anything that reads stdout/JSON downstream including an LLM agent; (b) a lookalike URI (`pkg:npm/lodаsh` with Cyrillic `а`) is indistinguishable from the real one — this fragments posture decisions across near-duplicates, which is exactly the kind of bug the canonical URI system was built to prevent (#53); (c) stored-XSS primitive if the entity profile is ever rendered in a web UI.

**Recommended test:** `TestSecurity_PutEntity_RejectsMalformedCanonicalURI` — table-driven, assert error for each of: empty-scheme `foo/bar`, control characters, null bytes, RFC3987 non-normalized Unicode, extremely long string (>1KB), strings containing newlines, strings not matching any of the known scheme prefixes (`pkg:`, `repo:`, `identity:`, `org:`, `patch:`).

**Suggested fix direction:** Centralize URI validation in `profile.ValidateCanonicalURI` (scheme must be in the known set, remainder must match a restrictive regex, max length enforced). Call from `PutEntity` before writing. `ensureEntity`'s fallback path should fail closed rather than wrapping arbitrary text as "canonical."

### C2. "Append-only" is convention, not an enforced invariant

**Location:** `internal/store/migrate.go:54-86` (v1 signals schema) and `:148-196` (v2 new tables), entire `sqlite.go` — no triggers, no `WITHOUT ROWID` tricks, no check constraints
**Category:** db

**Description:** The store comment at `sqlite.go:8` promises "signals, dependency observations, audit entries, and signal resolutions are APPEND-ONLY" and the code in `AppendSignals`/`AppendAuditEntry` etc. only issues INSERTs. But there is nothing in the schema preventing a future bug (or a malicious `signatory resolve` implementation, or a user running `sqlite3 signatory.db`) from running `UPDATE signals SET value='{}' WHERE id='...'` or `DELETE FROM audit_log WHERE ...`. The SQLite DB file is 0600 — but the threat model explicitly includes the tool's own code being attacked, so "we only call INSERT" is not an invariant.

**Impact:** The entire forensic/audit value of signatory depends on an attacker not being able to rewrite history. With no schema-level enforcement, any write path that takes a wrong turn — e.g., a new feature that uses `ExecContext` with a builder pattern — can silently erase or rewrite audit log entries without detection. The audit log is supposed to survive database compromise per the comment in `audit/logger.go:7-12`, but the DB-side half is a soft invariant.

**Recommended test:** `TestAppendOnly_NoUpdateOrDeletePathExists` — static check: walk the store package's SQL strings and assert none contain the substrings `UPDATE audit_log`, `DELETE FROM audit_log`, `UPDATE signals`, etc. Pair with `TestAppendOnly_TriggersBlockDirectMutation` — after opening the DB, directly run `UPDATE audit_log SET detail='tampered'` and assert the write fails with a trigger error.

**Suggested fix direction:** Add `CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log BEGIN SELECT RAISE(ABORT, 'append-only'); END;` and matching DELETE/UPDATE triggers for each append-only table. Store the triggers in the migration. A second line of defense: a chained Merkle hash column to audit_log so tampering is detectable even if triggers are bypassed.

### C3. Security tests pass on the broken behavior they claim to detect

**Location:** `internal/store/sqlite_security_test.go:16-65` (`TestSecurity_CorruptedTimestampReturnsError`, `TestSecurity_CorruptedBurnTimestampReturnsError`)
**Category:** tests

**Description:** Both tests insert a row with an invalid timestamp, read it back, and then use the pattern:
```go
if err == nil {
    assert.False(t, entity.CreatedAt.IsZero(), "...")
}
```
The if-gate means "if no error was returned, check the time isn't zero." **There is no assertion that an error was actually returned.** A correctly-failing test must `require.Error(t, err)`.

**Impact:** These tests give a false sense of coverage. The bug they were written to prevent (silent timestamp corruption being rendered as zero-time) can return and the tests will not notice. A parse error in a burn timestamp could render as `0001-01-01` and an LLM agent would conclude "this burn is ancient, probably safe to ignore."

**Recommended test:** Rewrite: `require.Error(t, err, "corrupted timestamp must surface as an error")` and delete the zero-time branch. Add positive control: insert a VALID timestamp and assert no error + non-zero parsed time.

**Suggested fix direction:** The assertion must fail if the corrupted data reads successfully in any form.

### C4. Audit log and DB files opened without `O_NOFOLLOW`; backup file opened without `O_EXCL`

**Location:** `internal/audit/logger.go:149` (audit file open), `internal/store/migrate.go:452` (backup file open), `internal/store/sqlite.go:65` (db file chmod TOCTOU)
**Category:** security

**Description:** Three related filesystem-safety gaps:

1. `audit/logger.go:149` opens `~/.signatory/audit.log` with `os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600`. If the path is a symlink to an arbitrary file (e.g., a writable SIEM input), the open follows it. No `O_NOFOLLOW`.
2. `migrate.go:452` creates the backup file with `os.O_CREATE|os.O_WRONLY` — no `O_EXCL`, no `O_NOFOLLOW`, no `O_TRUNC`. An attacker who can predict the backup filename (format is deterministic: `signatory.db.backup-v%d-YYYYMMDD-HHMMSS`) can pre-create a symlink pointing to `/etc/cron.d/signatory-backdoor` and the backup write will clobber it.
3. `sqlite.go:54-68` opens the DB, pings it (which creates the file with default umask, typically 0644), then chmods to 0600. Between the ping and the chmod, the file is world-readable if the parent dir is 0755.

**Impact:** (1) allows audit log redirection to an arbitrary file, which not only destroys audit integrity but also provides a "write to arbitrary file with content controlled by attacker" primitive via carefully-crafted actor/action/detail fields. (2) provides a "write to arbitrary file with copy of DB content" primitive. (3) is a local information disclosure window.

**Recommended test:** `TestSecurity_AuditLogRefusesSymlink`: pre-create a symlink at the audit path pointing at a sentinel file, call `Log`, assert failure. `TestSecurity_BackupRefusesSymlinkAtTarget`: same for backup. `TestSecurity_DBFilePermissionRace`: open the DB with a test hook that observes the file perms between ping and chmod.

**Suggested fix direction:** Add `syscall.O_NOFOLLOW` (on unix) and `O_EXCL` on create-new paths. For the DB, set umask before opening or create the file with explicit `os.OpenFile(..., O_CREATE|O_RDWR|O_EXCL, 0600)` before handing the path to `sql.Open`.

### C5. `compare` command is a silent stub — exits 0 with empty output

**Location:** `cmd/signatory/compare.go:12-16`
**Category:** construction

**Description:** The `compare` command has a parsed target pair, a real `Run` method, prints `"Comparing: %s vs %s"`, and returns `nil`. It never touches the engine, never opens the store, never collects signals, never compares anything. The CLI test (`cli_test.go:186-193`) asserts `err == nil` with no check on output, ratifying the stub.

**Impact:** **Direct threat to the stated threat model: "the tool is being run by an LLM agent that will act on its output."** An LLM agent instructed to compare two candidate dependencies runs `signatory compare pkg:npm/lodash pkg:npm/evil-lookalike`, sees exit 0 and a terse "Comparing: ..." line, and interprets that as "the tool finished and had nothing material to flag." This is indistinguishable from "equally trustworthy." The entire point of signatory is to answer this question.

**Recommended test:** `TestCompare_ReturnsErrorWhileUnimplemented` — assert `cmd.Run(...)` returns a non-nil error containing "not implemented". Pair with a CLI-level assertion that exit code is non-zero.

**Suggested fix direction:** Return an explicit `fmt.Errorf("compare: not implemented in v0.1, see design/ROADMAP.md")` so the exit code is non-zero and downstream agents can detect the stub. Same fix should be applied to `survey` even though it's deferred.

## High findings

### H1. `http.Client.CheckRedirect` only strips Authorization on host mismatch, not on scheme downgrade

**Location:** `internal/signal/github/client.go:45-55`
**Category:** security

**Description:** The redirect handler compares `req.URL.Host` to `via[0].URL.Host` and strips the Authorization header only when they differ. It does not check that the scheme is still `https`. If GitHub ever returned a 3xx redirecting to `http://api.github.com/...` (misconfiguration, MITM, or bug), the client would resend the Authorization header over plaintext HTTP, leaking the token.

**Recommended test:** `TestSecurity_RedirectToHTTPStripsAuth` — mock server that issues a 302 to an `http://` URL (same host, different scheme), assert the token is not in the redirected request.

**Suggested fix direction:** In `CheckRedirect`, strip `Authorization` if `req.URL.Scheme != "https"`. Consider returning an error on any scheme downgrade.

### H2. `GetDirectoryContents` and `GetFileRaw` pass `path` into URL without escaping

**Location:** `internal/signal/github/client.go:318-325, 334-354`
**Category:** security

**Description:** Both functions use `fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repoName, path)`. Today, all callers pass hardcoded constants. `owner`/`repoName` are pre-validated by `ParseRepoURL`. BUT the function itself does no validation of `path` — the moment a future caller forwards user-controlled content to this function, it's a path-injection + query-injection vulnerability.

**Recommended test:** `TestSecurity_GetFileRaw_RejectsRelativeOrQueryPath` — pass `../`, `?ref=x`, `#frag`, `%2e%2e`, URL-encoded slashes; assert all fail validation before the HTTP call is made.

**Suggested fix direction:** Add a `validContentPath` allowlist regex and reject at the top of `GetDirectoryContents`/`GetFileRaw`.

### H3. `GetLatestSignals` superseded-signal subquery is not entity-scoped

**Location:** `internal/store/sqlite.go:211-222`
**Category:** db

**Description:** The subquery `id NOT IN (SELECT superseded_signal_id FROM signal_resolutions)` scans `signal_resolutions` globally — it doesn't filter by `entity_id`. Today, signal IDs follow a globally-unique format so cross-entity collision is effectively impossible. But this couples the query's correctness to the ID-generation scheme.

**Impact:** An attacker with any write access to `signal_resolutions` can silently hide arbitrary signals from the "current state" view — including absence signals that serve as negative trust indicators.

**Recommended test:** `TestSecurity_GetLatestSignals_IsolatesEntities` — set up two entities, insert a resolution for entity A that references entity B's signal ID, assert entity B's signal still appears in its own GetLatestSignals result.

**Suggested fix direction:** Add `AND entity_id = ?` to the subquery. Index `signal_resolutions(entity_id, superseded_signal_id)` for performance.

### H4. Migration backup overwrites pre-existing files and doesn't use `O_EXCL`/`O_TRUNC`

**Location:** `internal/store/migrate.go:429-468` (`backupDatabase`)
**Category:** db / security

**Description:** Missing `O_EXCL` — if a file already exists at `backupPath`, it's opened for writing (following symlinks). Missing `O_TRUNC` — if the existing file is longer than the current DB, the tail is left intact. The backup filename uses second-precision timestamps; two backups within the same second collide silently.

**Recommended test:** `TestMigration_BackupRefusesClobber` — pre-create a file at the predicted backup path, assert backupDatabase errors (not clobbers).

**Suggested fix direction:** Add `O_EXCL|O_TRUNC`. Prefer `os.CreateTemp(dir, "signatory.db.backup-v*")` + rename.

### H5. Brittle `err == nil` reuse across statement boundaries in `burn.go`

**Location:** `cmd/signatory/burn.go:47-74`
**Category:** construction

**Description:** Line 73 uses `"overwrite": err == nil` to mean "did a prior burn exist." This works today because the `if err := s.SetBurn(...)` at line 65 uses `:=`, scoping its own `err`. If someone refactors line 65 to `if err = s.SetBurn(...)` (assignment instead of declaration), the meaning of `overwrite` flips silently to "did SetBurn succeed" — which is always true on the success path.

**Recommended test:** `TestBurn_AuditOverwriteFlagReflectsPriorBurn` — table-driven: (prior burn exists / doesn't), assert the audit entry's overwrite flag matches.

**Suggested fix direction:** Assign the prior burn into a local boolean at line 55: `hadPrior := err == nil`. Use `hadPrior` in the audit detail.

### H6. GitHub API error body flows into `error.Error()` and therefore stderr/logs

**Location:** `internal/signal/github/client.go:172-175, 222-225`
**Category:** security / error-handling

**Description:** `return fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(body))`. The body is limited to 4096 bytes but is otherwise unsanitized. It flows up to `Collect` → `analyze.go` → `main.go` → stderr. Any 4xx/5xx from a malicious or compromised upstream that echoes secrets, email addresses, internal IPs, or controlled tokens gets splatted to stderr where it will land in CI logs, SIEMs, and LLM agent transcripts.

**Impact:** `sanitizeErrorForStorage` exists and is called before storing to the signal database, but it is NOT called on the path from `GetRepo` (the only failure that aborts collection entirely). The sanitizer is a "sometimes shield," not a uniform one.

**Recommended test:** `TestSecurity_GetRepoErrorDoesNotLeakBody` — mock server returns 500 with a body containing a sentinel token; assert neither the wrapped error nor any downstream log contains the sentinel.

**Suggested fix direction:** Either `sanitizeErrorForStorage` should also wrap the raw-HTTP path in `client.go`, or the error returned should classify rather than embed (`"github API returned status %d"`, discarding the body entirely).

### H7. The `engine` package is dead wiring; its tests assert identity

**Location:** `internal/engine/engine.go:11-25`, `internal/engine/engine_test.go:103-215`
**Category:** tests / construction

**Description:** `engine.Engine` holds a store, collectors, and ecosystems, has no methods other than `New`, and is not imported by any command. The tests verify that `New` stores the arguments it was passed — tautologies that cannot catch any regression. The real analyze logic lives in `cmd/signatory/analyze.go`.

**Recommended test:** None needed — delete the package and its tests until there's real logic.

**Suggested fix direction:** Either delete `internal/engine/` or move the analyze logic out of `cmd/signatory/` and into it, then rewrite the tests against real behavior.

## Medium findings

### M1. DB file permission TOCTOU between `sql.Open`/`Ping` and `os.Chmod`

**Location:** `internal/store/sqlite.go:54-68`
**Category:** security

modernc's sqlite driver creates the DB file during `db.Ping()` with the process umask (typically 0644). Only after `Ping` returns does the code call `os.Chmod(path, 0600)`. Between those calls the file is potentially world-readable. Parent directory is 0700 via `MkdirAll`, which closes the window in practice — BUT `MkdirAll` does not narrow permissions on an existing parent directory.

**Suggested fix:** `syscall.Umask(0077)` before open/ping, restore after; or explicitly create the file with `O_CREATE|O_RDWR|O_EXCL, 0600` before handing the path to `sql.Open`.

### M2. `identity.Current()` trusts environment / file contents without sanitization

**Location:** `internal/identity/team.go:37-92`
**Category:** security

`SIGNATORY_TEAM` env var and `~/.signatory/team` file contents are trimmed and prefixed but not validated for length, character set, or control characters. The resulting string flows into audit log `Actor`, posture `SetBy`, burn `BurnedBy`, resolution `ResolvedBy`, and the on-disk JSON audit file.

**Suggested fix:** Enforce `^team:[A-Za-z0-9._+-]{1,128}$` at the normalize step; return an error instead of falling back silently.

### M3. Migration bootstrap (create `schema_version`, detect legacy) is not transactional

**Location:** `internal/store/migrate.go:268-297`
**Category:** db

`migrate()` creates the `schema_version` table, reads the current version, detects legacy tables, and inserts version 1 — all outside any transaction. Low probability in a single-user CLI but the codebase explicitly documents wanting concurrent access to be safe.

**Suggested fix:** Wrap bootstrap + first-insert in a single `BEGIN IMMEDIATE` transaction so concurrent openers serialize cleanly.

### M4. `migrationV2Down` posture collapse is non-deterministic

**Location:** `internal/store/migrate.go:236-239`
**Category:** db

The rollback does `INSERT OR REPLACE INTO postures_v1 SELECT ... FROM postures` with no ORDER BY. When multiple posture rows exist per entity, which row wins the `INSERT OR REPLACE` race depends on the unspecified row order SQLite returns.

**Suggested fix:** Add `ORDER BY set_at DESC` to the inner SELECT and use `INSERT OR IGNORE` (first-wins: newest-by-set_at).

### M5. `strings.Contains(errMsg, "not found")` is fragile error classification

**Location:** `internal/signal/github/collector.go:399-422` (`sanitizeErrorForStorage`)
**Category:** error-handling

The sanitizer classifies errors by substring match. A future error message containing the literal "not found" in an unusual context would be misclassified.

**Suggested fix:** Introduce sentinel errors in the client (`ErrNotFound`, `ErrRateLimited`) and use `errors.Is` in the sanitizer.

### M6. `json.Unmarshal` errors swallowed in `displayHuman`

**Location:** `cmd/signatory/analyze.go:246` (`_ = json.Unmarshal(s.Value, &val)`)
**Category:** error-handling

When displaying signals, the code silently discards JSON parse errors. A signal with a corrupted `Value` renders as an empty line with no indication that the underlying data is broken.

**Suggested fix:** Check the Unmarshal error, log a visible `[parse error]` next to the signal type.

### M7. `json.Marshal` errors on audit details are silently discarded

**Location:** `cmd/signatory/analyze.go:124`, `cmd/signatory/posture.go:163`, `cmd/signatory/burn.go:70`
**Category:** error-handling

`detail, _ := json.Marshal(...)`. Today the input can't fail; tomorrow it can, and the failure mode is silent audit truncation.

**Suggested fix:** Actually handle the error, return it or log it loudly.

### M8. `scanSignals` error message leaks up to 100 characters of signal value on parse failure

**Location:** `internal/store/sqlite.go:674`
**Category:** security / error-handling

The first 100 bytes of a potentially attacker-controlled signal value get embedded in an error that propagates up to stderr.

**Suggested fix:** `"invalid JSON in signal %s value (length=%d)"`. Log the bytes only at debug level.

### M9. `MkdirAll` does not narrow existing parent permissions

**Location:** `internal/store/sqlite.go:50` and `internal/audit/logger.go:145`
**Category:** security

`os.MkdirAll(dir, 0700)` only sets the mode on directories it creates; if the parent already exists with 0755 it stays 0755.

**Suggested fix:** After `MkdirAll`, explicitly `os.Chmod(dir, 0700)`.

### M10. `parseGoModDeps` has no size limit and is quadratic in the pathological case

**Location:** `internal/signal/github/collector.go:460-511`
**Category:** construction / DoS

`GetFileRaw` fetches up to 10MB decoded. `parseGoModDeps` does `strings.Split(content, "\n")` with no size check. An attacker controlling a repo's go.mod can return 10MB of newlines.

**Suggested fix:** Cap at 64KB of `go.mod` content; skip parsing with an absence record if larger.

## Low / informational

- **L1.** `len(commits)` division-by-zero is protected by an upstream if-guard at `internal/signal/github/collector.go:188` but the relationship is not documented.
- **L2.** `url.PathEscape` not applied to owner/repo in `GetRepo` at `internal/signal/github/client.go:247`. Defense-in-depth only.
- **L3.** `ListBurns` has no ORDER BY at `internal/store/sqlite.go:402`. Output order is unspecified.
- **L4.** `printCompactValue` map iteration is unordered at `cmd/signatory/analyze.go:273-282`. Humans notice; automated diffing of `signatory analyze` output breaks.
- **L5.** `audit/logger.go` opens and closes the file on every write. Minor; note for when the MCP mode arrives.
- **L6.** Empty packages left as scaffolding: `internal/cache/`, `internal/output/`, `internal/query/`, `internal/scanner/`, `internal/trust/`.
- **L7.** `parseTotalFromLink` has a typo in the delimiter set `">&>;"` at `internal/signal/github/client.go:382`.

## Tests worth strengthening

1. `internal/engine/engine_test.go` — 215 lines of identity assertions.
2. `internal/signal/collector_test.go` — exercises a mock defined in the test file itself. Does not exercise any real collector.
3. `internal/signal/github/collector_test.go:163-270` — heavy on "did we call the right endpoints" but light on "what if the endpoint returns surprising data."
4. `cmd/signatory/live_test.go:49, 71` — uses v1-style short-name lookups (`s.GetSignals(ctx, "alecthomas/kong")`) that cannot work against the v2 UUID schema.
5. Audit logger has no test for concurrent writes from multiple goroutines (the mutex is never exercised under load) or file injection via attacker-controlled Actor/Action fields.
6. No fuzz tests anywhere. Obvious targets: `NormalizeGitHubRepoInput`, `ParseRepoURL`, `parseGoModDeps`, `parseTotalFromLink`, `parseRateLimitReset`.

## Positive observations

1. **Append-only semantics are enforced in the Go layer with discipline**: all the append methods genuinely only issue INSERTs, the IDs include timestamps, and `TestAppendSignals_DuplicateIDFails` and `TestAppendSignals_IsAppendOnly` validate the contract.
2. **Response-size limits and redirect-aware auth stripping** in the GitHub client show real threat-model thinking.
3. **The v2 migration is actually reversible** and has a data-preservation round-trip test (`TestMigration_V2DataRoundTrip`).
4. **Canonical URI normalization + stability test** (`TestNormalizeGitHubRepoInput_Stable`) explicitly checks that all equivalent input forms collapse to the same URI.
5. **Dual-sink audit with the DB as authoritative and the file as a best-effort secondary** is the right architecture.
