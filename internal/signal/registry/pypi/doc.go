// Package pypi is the PyPI registry-side support for signatory's
// supply-chain trust analysis: an HTTP client for pypi.org's JSON
// API and the helpers that turn its responses into the canonical
// shapes signatory's other layers consume.
//
// V0.1 scope is the source-resolution slice — answer "what is the
// declared source repository for this PyPI package?" so callers
// like the analyst-agent dispatch, --clone-dir, and
// --network-precheck can route to a github (or future-platform)
// repo without a human or LLM hand-walking pypi.org. The Layer 5
// signal collector (last_publish, trusted_publishing, sdist
// hygiene, etc.) plugs into the same client when it ships.
//
// Mirrors internal/signal/registry/npm/'s shape: client.go for the
// HTTP plumbing, wire.go for the typed registry models,
// normalize.go for the pure URL/name normalizers, resolve.go for
// the GetProject → ResolveRepoURL composition.
package pypi
