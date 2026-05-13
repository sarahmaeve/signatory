// Package deltas computes and renders structural differences between
// successive signal observations for a target entity.
//
// The package is the presentation layer over signatory's existing
// append-only signal store. The storage primitives (GetSignals,
// GetLatestSignals, signal_resolutions) already preserve a complete
// history; deltas adds a way to query "what changed between
// observations" without LLM cost. See design/deltas.md for the full
// design and design/stalesignals.md for the storage discipline this
// builds on.
//
// The core type is ValueDiff, produced by Diff(prior, current). The
// Diff function is pure — same inputs produce the same output, no
// I/O. Renderers consume ValueDiff and emit either human-readable
// text (highlighting newest changes, suppressing unchanged signals)
// or structured JSON (machine-readable, also the format the Phase 2
// MCP tool will use).
//
// Diff semantics in summary:
//
//   - Top-level keys: added (in current only), removed (in prior
//     only), changed (in both with differing values).
//   - Nested objects: recurse to a bounded depth.
//   - Arrays: per-position diff when same-length primitive arrays;
//     stable-key alignment for arrays of objects with a recognized
//     key field (version, login, tag_name, path, name); opaque
//     before/after otherwise.
//
// Design source: design/deltas.md.
package deltas
