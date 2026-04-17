// Package invariants enforces the v0.1 architectural invariants declared
// in design/v0.1-invariants.md. Each invariant ships with at least one
// test in this package that fails if the invariant is violated.
//
// Design choice: the checks are Go tests rather than a separate CI step
// or a shell script. That way they run under every invocation of
// `go test ./...` — meaning the main CI workflow, the local pre-commit
// hook, and `make check` all enforce the invariants automatically with
// no extra wiring. Adding a new invariant check is a matter of adding
// a test file here; the gauntlet picks it up on the next run.
//
// If an invariant needs to be relaxed, update design/v0.1-invariants.md
// in the same commit that softens the test. Do not silence a test
// without touching the spec — the two are meant to drift together or
// not at all.
package invariants
