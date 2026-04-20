// Package npm provides registry-side signal collection for npm
// packages. The package houses two layers:
//
//   - Client — a narrow HTTP client against registry.npmjs.org and
//     api.npmjs.org that models only the response fields Phase A/B
//     collectors read. Defenses mirror internal/signal/github/client.go:
//     HTTPS-only redirects, bounded response size, error-body
//     sanitization, package-name validation before URL construction.
//
//   - Collector — an implementation of signal.Collector that acts on
//     entities with CanonicalURI prefix pkg:npm/. Emits flat metadata
//     signals; non-npm entities receive an empty result.
//
// See design/npm-plan.txt for the phase structure. This package is
// invariant-path-bound: design/v0.1-invariants.md §Invariant 2 binds
// npm-registry signals to internal/signal/registry/npm/.
package npm
