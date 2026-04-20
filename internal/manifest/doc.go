// Package manifest parses dependency manifests (go.mod, package.json,
// requirements.txt, Cargo.toml, ...) into a common Dep type that
// signatory's survey command and other per-project tooling can
// consume uniformly.
//
// For v0.1, only Go manifests (go.mod) are supported — see
// internal/manifest/gomod. Additional ecosystems land alongside
// their matching collectors: npm with the npm registry collector,
// PyPI with the PyPI collector, etc.
//
// Design: the sub-packages (gomod, npm, pypi, cargo) are free to
// import this package for the shared Dep / ProjectInfo types, but
// this package does NOT depend on any of them. There is no Parser
// interface for v0.1 — YAGNI while only one implementation exists.
// A second implementation's arrival is the signal to generalize.
package manifest
