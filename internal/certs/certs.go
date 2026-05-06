// Package certs manages signatory's local TLS trust setup so Claude
// Code's WebFetch tool can reach the pipeline service over HTTPS.
//
// Claude Code's WebFetch forces HTTPS on all URLs, so the pipeline
// message service (signatory serve) must present a cert that the
// Node runtime backing WebFetch trusts. mkcert creates a locally-
// trusted CA and issues a localhost cert against it; Node's TLS
// stack consults NODE_EXTRA_CA_CERTS at every handshake to load
// additional trusted CAs.
//
// The failure mode that drove this package into existence:
// NODE_EXTRA_CA_CERTS is an ambient env var. If it isn't exported
// from a shell profile, it vanishes across terminal restarts, GUI
// launches, and any subagent that inherits a clean env. Synthesist
// dispatches would succeed one run and fail the next with
// "unable to verify the first certificate."
//
// The package offers three operations:
//
//   - Check: non-interactive preflight. Exits non-zero with a
//     remediation message if the env var is missing or stale.
//     Called from signatory serve start and from /analyze Step 0a.
//
//   - Init: idempotent setup. Copies mkcert's root CA to a stable,
//     signatory-owned path (~/.signatory/certs/rootCA.pem by
//     default), decoupling the env var target from mkcert's
//     Application Support directory, which on macOS contains
//     spaces and isn't under signatory's control.
//
//   - WriteProfile: opt-in shell profile patching. Appends a
//     bracketed managed block to ~/.zshrc (or the configured
//     profile path) exporting NODE_EXTRA_CA_CERTS. Re-running
//     replaces the block in place rather than duplicating it.
package certs

// EnvVar is the environment variable Node's TLS stack reads at
// every HTTPS handshake to locate additional trusted CA bundles.
// Claude Code's WebFetch uses Node under the hood; subagents
// dispatched by /analyze must see this set (and pointing to a
// valid PEM) when they fetch the local pipeline service.
const EnvVar = "NODE_EXTRA_CA_CERTS"

// DefaultCertDir is the default for --cert-dir. Stable path under
// signatory's home — no spaces, no dependency on mkcert's CAROOT
// location, which on macOS is
// "/Users/<name>/Library/Application Support/mkcert" (has spaces,
// not controlled by signatory, subject to change if mkcert relocates).
const DefaultCertDir = "~/.signatory/certs"

// CAFileName is the filename under CertDir that NODE_EXTRA_CA_CERTS
// points at. Fixed so the exported env line is stable across
// installs.
const CAFileName = "rootCA.pem"

// DefaultCAPath returns the absolute path of the canonical CA anchor
// signatory manages: DefaultCertDir + CAFileName with `~/` expanded
// to the user's home directory. This is the single file all
// signatory-managed clients load to trust the local pipeline service's
// TLS cert; see design/tls-trust.md for the full trust architecture.
//
// Returns an error only if the user has no HOME — an exceptional
// environment in which almost nothing else would work either.
// Callers that can proceed without the anchor (e.g., Go clients that
// fall back to the system root pool) should tolerate the error by
// skipping the load rather than aborting.
func DefaultCAPath() (string, error) {
	return expandHome(DefaultCertDir + "/" + CAFileName)
}

// DefaultShellProfile is the target for --write-profile when no
// explicit --shell-profile-path is passed. zshrc is the interactive-
// shell entry point on macOS and matches how Claude Code is
// typically launched (via an interactive terminal). zprofile and
// zshenv are deliberately NOT the default: zprofile runs once at
// login and GUI-launched apps may skip it entirely; zshenv runs
// for every shell including non-interactive ones that shouldn't
// pay the cost.
const DefaultShellProfile = "~/.zshrc"

// ProfileMarkerBegin and ProfileMarkerEnd bracket signatory's
// managed block inside the shell profile. WriteProfile is
// idempotent: rerun replaces the block between these markers,
// leaving anything outside untouched.
//
// The markers are intentionally verbose — a user scanning their
// zshrc should immediately understand what wrote the block and
// how to regenerate or remove it.
const (
	ProfileMarkerBegin = "# signatory-managed: NODE_EXTRA_CA_CERTS — BEGIN (regenerate with `signatory certs init --write-profile`; remove this whole block to detach)"
	ProfileMarkerEnd   = "# signatory-managed: NODE_EXTRA_CA_CERTS — END"
)

// FailCode classifies why Check reported NotOK. Callers use it to
// pick an exit code and remediation hint; humans use the Message
// + Fix fields of CheckResult for the actual message text.
type FailCode int

const (
	// StatusOK means the preflight passed — env var is set, points
	// to a readable file that looks like a PEM cert bundle.
	StatusOK FailCode = iota

	// FailEnvUnset means NODE_EXTRA_CA_CERTS is empty or unset in
	// the current process env. Most common failure mode — the
	// ambient-env problem this package was built to close.
	FailEnvUnset

	// FailPathMissing means the env var points at a filesystem
	// path that doesn't exist. Usually happens after mkcert is
	// reinstalled into a different CAROOT without rerunning
	// `signatory certs init`.
	FailPathMissing

	// FailPathInvalid means the path exists but its contents
	// don't parse as PEM — truncation, wrong file, or a different
	// format entirely.
	FailPathInvalid
)

// ProfileAction names the mutation WriteProfile applied. Callers use
// this to decide whether to emit a "wrote" line or stay silent. A
// typed constant set (rather than raw strings) keeps the switch-over
// in the CLI grep-findable and lint-checkable.
type ProfileAction string

const (
	ProfileUnchanged ProfileAction = "unchanged"
	ProfileAppended  ProfileAction = "appended"
	ProfileReplaced  ProfileAction = "replaced"
	ProfileCreated   ProfileAction = "created"
)
