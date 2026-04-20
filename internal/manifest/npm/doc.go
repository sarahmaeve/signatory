// Package npm parses an npm project's manifest (package.json,
// optionally alongside a package-lock.json v2/v3) into the
// ecosystem-neutral types defined in internal/manifest.
//
// The package-lock.json formats this parser supports are
// lockfileVersion 2 and 3 — both carry the flat `packages` map
// that modern npm (v7+) writes. lockfileVersion 1 uses a nested
// `dependencies` tree and is not supported in v0.1; surveys of
// projects locked by older npm will get direct-deps-only output.
//
// Canonical URIs: pkg:npm/<name> for registry-hosted deps,
// preserving @scope/ for scoped packages. Non-registry deps
// (file:, git:, github:, http:, https:, npm:alias, workspace:)
// are flagged with ecosystem="npm-local" and left without a
// canonical URI — there's no registry source to analyze remotely,
// and a survey renderer can message this explicitly rather than
// pretend to resolve.
package npm
