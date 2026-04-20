// Package survey assesses the trust posture of a project's
// dependency tree against signatory's store.
//
// The package is designed as a library with one load-bearing
// function: Run(ctx, store, manifestPath) returns a Result
// carrying per-dependency tier resolution plus aggregate
// summary counts. The signatory CLI's `survey` command is a
// thin renderer over Run; a future web UI's per-project page
// will import the same function and render HTML instead of
// terminal text.
//
// The tier-resolution logic is the heart of survey:
//
//   - Burns win absolutely (Layer 0 per design/trust-policy-v1.md).
//   - An exact-version posture match wins next.
//   - Postures-for-other-versions → tier is unexamined, but flagged
//     so the caller can tell a sophisticated user "you pinned v1.15
//     but v1.14 has a vetted-frozen decision you might want to
//     consult."
//   - Entity exists but no postures → unexamined (signals gathered
//     but no decision yet).
//   - Entity absent → not-in-store.
//
// This ordering matches the Layer 0 → Layer 1 → Layer 3 stack in
// the trust-policy sketch. Survey is a read-only observer of that
// stack; it doesn't compute scores itself.
package survey
