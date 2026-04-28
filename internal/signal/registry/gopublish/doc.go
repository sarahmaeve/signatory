// Package gopublish provides registry-side signal collection for
// Go modules, mining publish-provenance evidence from the two
// public Go-module data planes:
//
//   - proxy.golang.org — the canonical module proxy, which serves
//     metadata (.info), the version list (@v/list), and the latest
//     pointer (@latest). Origin information (VCS, ref, hash) lands
//     here when published with `go mod` ≥ 1.20.
//
//   - sum.golang.org   — the append-only transparency log. A
//     successful /lookup/<module>@<version> response means the
//     module/version was recorded into a globally-auditable Merkle
//     tree at publish time. Absence is a load-bearing signal: a
//     module that exists on the proxy but not on the checksum log
//     is either ancient (pre-2019), private, or proxy-only-cached
//     (rare; needs an honest investigation).
//
// The package houses two layers, mirroring the npm registry
// collector:
//
//   - Client — a narrow HTTP client with HTTPS-only redirects,
//     bounded response size, sanitized errors, and module-path
//     validation before URL construction. proxy.golang.org's
//     `!`-escape rule (uppercase-letter encoding) is applied at the
//     URL boundary so callers pass plain module paths.
//
//   - Collector — implements signal.Collector for entities whose
//     CanonicalURI starts with `pkg:golang/`. Emits flat metadata
//     signals; non-Go entities receive an empty result.
//
// Wiring: cmd/signatory/collectors.go selects this collector when
// entity.Ecosystem == "golang" (purl-spec canonical) or "go" (older
// signatory convention, kept for backwards compat alongside the
// resolver registry's dual registration).
//
// Replaces three curl/Bash calls per dogfood analysis on Go modules
// (proxy @v/list, proxy @v/<v>.info, sum lookup), folding them into
// the mechanistic collection layer where they're cached, retried,
// and visible in the local store.
package gopublish
