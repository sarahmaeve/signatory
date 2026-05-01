// Package signal — signal type registry.
//
// The registry is the canonical source of truth for (signal type →
// metadata). Before this existed, type-level facts were hardcoded at
// every collector call site AND duplicated in absence.go's
// signalGroupForType switch. The two copies disagreed in practice,
// and new signal types surfaced by analyses (atuin, thefuck, external
// security reviews) had nowhere to live.
//
// The registry resolves both by making Group and ForgeryResistance
// data-driven. Collectors pass a type string; the registry supplies
// the rest. See design/signal-type-registry.md for the design note.
//
// Per the v0.1 decision log, this pass intentionally excludes:
//   - Realm (deferred to enterprise work)
//   - Weight (deferred to user-configurable tuning)
//   - Polarity (deferred; drops amplifier-role signals from this batch)
//   - Per-entity-type overrides (deferred)
//
// The three documented "amplifier" signals (hosted_service_coupling,
// self_updater_present, ai_agent_runtime_capability) and the one
// synthesis-time amplifier (fallow_status_amplifier) are intentionally
// absent — they need the Polarity axis to be represented honestly.
// When Polarity lands, add them in the same change.

package signal

import (
	"cmp"
	"slices"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// SignalTypeInfo is the compile-time catalog entry for one signal type.
//
// Group and ForgeryResistance are the type-level defaults that every
// emitted observation of this type inherits. Collectors that need to
// override per-observation (rare) can still construct a profile.Signal
// directly rather than going through signal.Make.
//
// Description and Caveats are for human consumption — surfaced in
// --verbose output, in MCP resources when the MCP subsystem is wired,
// and in JSON output so LLM consumers can reason about the limits of
// a signal before citing it.
type SignalTypeInfo struct {
	// Type is the canonical signal name (e.g., "stars", "commit_signing").
	// Must be unique across the registry.
	Type string

	// Group is the question the signal answers. Inherited by every
	// observation of this type.
	Group profile.SignalGroup

	// ForgeryResistance is how hard the signal is to fake. Inherited by
	// every observation of this type.
	ForgeryResistance profile.ForgeryResistance

	// Description is a short human-readable explanation of what this
	// signal measures. One sentence; assume a reader who understands
	// the trust model but not the specific signal.
	Description string

	// Caveats lists known limitations of this signal — the reasons the
	// ForgeryResistance rating isn't higher, the ways it can mislead,
	// the conditions under which it doesn't apply. Empty when no
	// material caveats exist.
	Caveats []string
}

// GetSignalTypeInfo returns the registry entry for a signal type.
// Returns ok=false if the type is not registered — callers MUST
// treat unregistered types as a programming error (every signal a
// collector emits or an analyst produces should be registered here).
func GetSignalTypeInfo(signalType string) (SignalTypeInfo, bool) {
	info, ok := signalTypeRegistry[signalType]
	return info, ok
}

// SignalTypes returns all registered types, sorted by Type name for
// stable iteration. Intended for diagnostics, JSON output, and the
// eventual MCP resource — not for hot paths.
func SignalTypes() []SignalTypeInfo {
	out := make([]SignalTypeInfo, 0, len(signalTypeRegistry))
	for _, info := range signalTypeRegistry {
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b SignalTypeInfo) int {
		return cmp.Compare(a.Type, b.Type)
	})
	return out
}

// signalTypeRegistry is the canonical catalog. Grouped by SignalGroup
// for reading; order within a group is not semantically meaningful.
//
// When adding a new entry:
//   - Every emitted signal type MUST be registered before collection
//     can produce it (signal.Make panics on unregistered types).
//   - Descriptions are one sentence, audience "trust-model-literate".
//   - Caveats call out *why* the ForgeryResistance rating isn't higher
//     or the conditions under which the signal misleads. These are
//     surfaced to users and LLMs; they're not internal notes.
//   - If the signal's forgery resistance doesn't fit the existing
//     four tiers, DO NOT invent a new enum value — revisit the
//     classification with the trust model in hand.
var signalTypeRegistry = map[string]SignalTypeInfo{

	// ================================================================
	// Vitality — "Is anyone home?"
	// ================================================================

	"last_push": {
		Type:              "last_push",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Timestamp of the most recent push to the default branch.",
		Caveats: []string{
			"push dates can lag behind meaningful work in a tag-only release flow",
			"force-push can rewrite history and alter this value",
		},
	},
	"last_publish": {
		Type:              "last_publish",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Timestamp of the most recent publication of a package to its registry (npm, PyPI, crates.io, etc.).",
		Caveats: []string{
			"publication dates are set by the registry at receive time — they're harder to backdate than git commit timestamps, but a package published under an attacker's control still produces a publication event with a current timestamp",
			"a recent last_publish is not positive evidence of active maintenance — a compromised-account publish looks identical to a legitimate one in this signal alone",
			"a stale last_publish on a widely-depended-on package may indicate either fallow stability or abandonment; pair with maintainer activity to interpret",
		},
	},
	"repo_age": {
		Type:              "repo_age",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Age of the repository since creation.",
		Caveats: []string{
			"age alone is not positive — a one-commit-per-year fallow repo has high age and low vitality",
		},
	},
	"first_commit_date": {
		Type:              "first_commit_date",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Timestamp of the first commit in the default branch's history, derived from a local clone.",
		Caveats: []string{
			"commit dates are user-controllable in git; a rewritten history can backdate or forward-date the first commit",
			"requires a full clone — shallow clones truncate history and will report the oldest commit within the depth window rather than the repo's actual first commit",
			"distinct from repo_age, which reports the hosting platform's repository creation timestamp and is harder to forge once observed",
		},
	},
	"open_issues": {
		Type:              "open_issues",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Count of open issues (GitHub reports PRs in this count too).",
		Caveats: []string{
			"triage hygiene varies wildly; counts are comparable within a project, not across projects",
		},
	},
	"archived": {
		Type:              "archived",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the repository has been marked archived by its owner.",
		Caveats: []string{
			"archived implies read-only but not necessarily end-of-life — some projects archive after migrating to a successor",
		},
	},
	"last_commit": {
		Type:              "last_commit",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Timestamp of the most recent commit on the default branch.",
		Caveats: []string{
			"commit dates can be set arbitrarily in git; author date and committer date can disagree",
			"not identical to last_push — an unpushed branch doesn't update this",
		},
	},
	"total_commits": {
		Type:              "total_commits",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Total commit count on the default branch.",
		Caveats: []string{
			"low count on an old repo indicates write-once code, not maintenance activity",
		},
	},
	"commit_activity_shape": {
		Type:              "commit_activity_shape",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Shape of commit activity over time — accelerating, flat, bursty, or decelerating.",
		Caveats: []string{
			"noisy on projects with release-based flow where most activity happens in short windows",
			"derivation method (rolling window, slope calculation) affects the shape classification",
		},
	},
	"version_count": {
		Type:              "version_count",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Number of published versions for a package, sourced from the registry's append-only version list.",
		Caveats: []string{
			"a single version with high adoption is healthy — count alone is not positive",
			"high counts on a long-lived module reflect cumulative releases over time, not necessarily current activity — pair with last_publish",
			"some Go modules use a v0 version stream indefinitely; count of major versions is not directly comparable across ecosystems",
		},
	},

	// ================================================================
	// Governance — "Who's responsible?"
	// ================================================================

	"owner_type": {
		Type:              "owner_type",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the repo is owned by a user account or an organization.",
		Caveats: []string{
			"org-owned does not mean multi-maintainer — a one-person org is common",
		},
	},
	"owner_profile": {
		Type:              "owner_profile",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Repo owner's account metadata — tenure, public repos, followers, affiliation.",
		Caveats: []string{
			"account age is forgery-resistant once observed but can be faked forward by seeding a quiet account years before use",
			"follower counts are manipulable via fake-account rings",
		},
	},
	"contributors": {
		Type:              "contributors",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Contributor list with contribution counts.",
		Caveats: []string{
			"GitHub's contributor graph is commit-count based; drive-by commits appear as contributors",
			"merge-commit-based stats can hide the actual authorship distribution",
		},
	},
	"commit_signing": {
		Type:              "commit_signing",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Ratio of recent commits with verified GPG/SSH signatures.",
		Caveats: []string{
			"GitHub's verified:true flag conflates personal signing with web-flow signing — see per_developer_commit_signing_ratio for the split",
			"verification status depends on key validity at observation time; key revocation invalidates previously-verified commits",
		},
	},
	"go_dependencies": {
		Type:              "go_dependencies",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "go.mod direct and indirect dependency counts and direct-dependency list.",
		Caveats: []string{
			"indirect counts include transitive entries forced by minimum-version-selection and may misrepresent the project's intentional surface",
		},
	},
	"identity_domain_consistency": {
		Type:              "identity_domain_consistency",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Consistency between maintainer email domain, project domain, and other owned domains.",
		Caveats: []string{
			"requires domain ownership verification to be trustworthy; bare email-match is a weak form",
			"not applicable to projects whose maintainers have no published personal or corporate domain",
		},
	},
	"effective_maintainer_concentration": {
		Type:              "effective_maintainer_concentration",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Proportion of recent contribution concentrated in a small number of committers, regardless of org backing.",
		Caveats: []string{
			"bus-factor signal — high concentration is negative even when the project is organizationally backed",
		},
	},
	"per_developer_commit_signing_ratio": {
		Type:              "per_developer_commit_signing_ratio",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Fraction of recent commits signed by the committing author's own key, not by GitHub's web-flow key.",
		Caveats: []string{
			"requires parsing the verification.signature and verification.reason fields, not just the verified boolean",
			"depends on the project's signing policy being enforceable on all contributors",
		},
	},
	"web_flow_signing_ratio": {
		Type:              "web_flow_signing_ratio",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Fraction of recent commits signed by GitHub's web-flow key (merges and suggestion commits).",
		Caveats: []string{
			"a high ratio with low per-developer signing means trust is delegated to GitHub's platform, not to contributor identity",
		},
	},
	"identity_graph_depth": {
		Type:              "identity_graph_depth",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       ".mailmap-derived count of confirmed identity mappings across contributors.",
		Caveats: []string{
			"corporate-to-personal email migrations across multi-year windows are expensive to fabricate across multiple contributors",
			"projects without .mailmap produce no signal in either direction",
		},
	},
	"maintainer_count": {
		Type:              "maintainer_count",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Count and names of maintainers with publish rights on a package registry (npm, PyPI, etc.).",
		Caveats: []string{
			"maintainer accounts can be compromised independently of each other — a high count raises the cost of a full-takeover but doesn't prevent single-account credential theft",
			"npm's maintainers list is self-declared by the package owner; a packaged org can rotate maintainers without notice",
			"low count (bus-factor 1) is a governance concern independent of the individual maintainer's trustworthiness",
		},
	},
	"analyst_self_correction": {
		Type:              "analyst_self_correction",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Meta-signal: an analysis round explicitly supersedes a prior round's conclusion based on deeper grounding.",
		Caveats: []string{
			"emitted as metadata on the analysis record, not on the target entity",
			"absent an explicit supersedes-reference in analyst output, this cannot be inferred after the fact",
		},
	},
	"dual_analyst_self_confirmation": {
		Type:              "dual_analyst_self_confirmation",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Meta-signal: two analysts using independent methods converged on the same absence or positive conclusion.",
		Caveats: []string{
			"synthesis-only — emitted by the synthesist role, not by individual analysts",
			"information-theoretic: two independent-method false negatives compound, but common-mode analyst failures (same training blind spot) can still produce a shared false negative",
		},
	},

	// ================================================================
	// Publication — "How was this published?"
	// ================================================================

	"tags": {
		Type:              "tags",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Count and list of recent tags.",
		Caveats: []string{
			"tag names alone don't convey signing status — see tag_signing_status for the distinction",
			"a tag's existence doesn't imply a corresponding package publication",
		},
	},
	"release_tooling": {
		Type:              "release_tooling",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Kind, version, and workflow location of the project's release tooling (e.g., cargo-dist, goreleaser).",
		Caveats: []string{
			"standardized tooling reduces ad-hoc release-compromise risk but doesn't eliminate it",
		},
	},
	"tag_signing_status": {
		Type:              "tag_signing_status",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Classification of tag style — signed_annotated, annotated_unsigned, or lightweight.",
		Caveats: []string{
			"lightweight tags carry no signing information and are indistinguishable from branch-like refs",
		},
	},
	"build_provenance_attestation": {
		Type:              "build_provenance_attestation",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Presence of Sigstore/SLSA build provenance attestations on published artifacts.",
		Caveats: []string{
			"attestation alone is not trust — a verifier must check it against a known-good build configuration",
		},
	},
	"registry_publish_origin": {
		Type:              "registry_publish_origin",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Origin of registry publishing — oidc_ci, long_lived_token_ci, local_maintainer_machine, or unknown.",
		Caveats: []string{
			"oidc_ci is the hardened posture; local_maintainer_machine is the lowest trust tier",
			"CI-based publishing is only as trustworthy as the CI workflow's action-pin tightness",
		},
	},
	"crates_io_trusted_publishing": {
		Type:              "crates_io_trusted_publishing",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Whether crates.io trusted-publishing (OIDC) is configured for the crate.",
		Caveats: []string{
			"status is visible only after a first publish that used it — absence on a new crate is not automatically negative",
		},
	},
	"postinstall_present": {
		Type:              "postinstall_present",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the latest published package version declares a postinstall lifecycle script that executes on every install.",
		Caveats: []string{
			"presence alone is not negative — legitimate uses include native-binary builds and platform bootstrap",
			"the axios-case-study attack vector was modifying a package.json to add a postinstall pointing at malicious code; presence raises the bar for reviewing what the script does",
			"signal reports presence only; reviewing the script content is an analyst task, not a mechanical signal",
		},
	},
	"trusted_publishing": {
		Type:              "trusted_publishing",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Presence of an OIDC trusted-publishing attestation on the latest published package version (npm's dist.attestations).",
		Caveats: []string{
			"present-and-valid is a very high-quality provenance signal — the attestation cryptographically binds the published version to a source repo and commit SHA",
			"absence is not automatically negative — older published versions predate trusted publishing, and the maintainer may have not opted in yet",
			"absence on a package that previously used trusted publishing IS strongly negative — the axios attack pattern — but detecting the transition requires comparing across versions; publish_origin_consistency is the cross-version complement to this snapshot signal",
		},
	},
	"postinstall_introduced": {
		Type:              "postinstall_introduced",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether a postinstall lifecycle script appeared in the latest version of a package that had previously published versions without one. Longitudinal complement to postinstall_present.",
		Caveats: []string{
			"transitions have legitimate causes — native-binary build adoption, platform bootstrap migration, tooling change — so a true positive is an anomaly flag, not a verdict",
			"the axios 2026 supply-chain attack fit this pattern exactly: a postinstall was added to a package that had published without one for years",
			"window is bounded (last N versions by publish time); a postinstall introduced farther back looks indistinguishable from one that was always there",
		},
	},
	"publish_origin_consistency": {
		Type:              "publish_origin_consistency",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Consistency of publish provenance across recent versions: presence-transitions on OIDC attestations plus count of distinct publisher accounts.",
		Caveats: []string{
			"a single publisher across many versions with consistent attestation presence is the healthy pattern — transitions are anomaly signals, not verdicts",
			"legitimate reasons to transition include maintainer handoff, CI pipeline migration, or a first adoption of trusted publishing — these produce false positives worth investigating, not dismissing",
			"the axios 2026 forensic specifically called out the broken attestation chain as the detection-relevant fingerprint — this signal captures that shape across versions",
			"the _npmUser.name field is the registry's publisher stamp and cannot be rewritten post-publish; it's higher-forgery-resistance than maintainer lists which are self-declared",
		},
	},
	"transparency_log_present": {
		Type:              "transparency_log_present",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Whether sum.golang.org's transparency log has a record for the (module, version) pair. Append-only and publicly auditable.",
		Caveats: []string{
			"a successful lookup proves the module/version was committed to a globally-auditable Merkle tree at publish time — extremely high forgery resistance",
			"absence does not automatically mean tampering: pre-2019 versions, private modules, and proxy-only-cached modules can be absent for benign reasons; an honest investigation distinguishes",
			"presence does not validate the source repository — it certifies that this hash was published, not that the hash matches a particular VCS commit",
		},
	},
	"publish_origin": {
		Type:              "publish_origin",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Proxy-declared VCS source for a Go module version: VCS, URL, ref, and commit hash from proxy.golang.org's @v/<version>.info Origin block.",
		Caveats: []string{
			"present only for modules published with go ≥ 1.20; older versions lack the Origin section and produce an absence",
			"the Origin URL is the proxy's record of where the module was fetched from at publish time — cross-check against the entity's resolved repo URL to detect mismatches",
			"the hash is a commit SHA; when paired with sum.golang.org's transparency log it gives a reproducible proof-of-fetch chain",
		},
	},
	"version_pin_table": {
		Type:              "version_pin_table",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Per-version (version, sha, published_at) pin table from proxy.golang.org. Trust anchor consumed by source-evolution to attach matrix rows to commit SHAs.",
		Caveats: []string{
			"covers up to the 12 most-recent versions; long-history modules may not have full coverage in a single emission",
			"pre-Go-1.20 versions lacking the proxy Origin block land in missing_origin_versions[], not pins[] — source-evolution falls back to local refs/tags for those when reconstructing matrix rows",
			"fetch failures (proxy 5xx, network) land in fetch_failed_versions[] separately from missing-origin; the distinction is \"proxy doesn't know\" vs \"we couldn't ask\"",
			"v0.1 emits source: \"proxy.golang.org\" for every pin; the field is retained for forward compatibility with future registry-side pin sources",
		},
	},
	"source_evolution_matrix": {
		Type:              "source_evolution_matrix",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Per-tagged-version AST feature matrix for a Go module, anchored to version_pin_table SHAs. Surfaces sleeper-to-weaponized publication patterns through direct cross-version source comparison rather than tag-cadence correlatives.",
		Caveats: []string{
			"bounded by the source-evolution collector budget (last-N + leaves-of-each-major); long-history modules may have rows omitted",
			"Go-specific in v0.1; non-Go entities skip without emitting",
			"the AST count of init() does not distinguish legitimate package init from payload bootstrap — the analyst's job to interpret a spike row",
			"documented v0.1 coverage gaps include dot imports, three-level method chains, local-var-bound clients/encodings, and binary ^ inside regular = assignment",
			"missing-from-clone rows (proxy has a SHA the local --refresh did not fetch) are preserved with tag_sha_local_status and null analysis blocks, not dropped",
		},
	},
	"source_evolution_anomaly": {
		Type:              "source_evolution_anomaly",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Boolean+pointer summary derived from source_evolution_matrix: an inflection point exists between consecutive tagged versions where two or more feature counts cross from zero baseline. Names the suspect version pair and which features spiked.",
		Caveats: []string{
			"refactors and legitimate feature additions can also produce multi-feature spikes — the signal is an anomaly flag, not a verdict; the analyst reads the matrix row at the spike SHA to classify",
			"threshold is conservative (multi-feature joint, false-negative-heavy by design); false negatives are recoverable because the matrix itself is in the handoff and the analyst can still notice",
			"absence does not mean clean — a sleeper that has not yet been weaponized produces a flat matrix, no anomaly fires, and the operator's metadata signals (account age, tag signing) carry the load until source diverges",
		},
	},

	// ================================================================
	// Hygiene — "Does it look like they care?"
	// ================================================================

	"license": {
		Type:              "license",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryLowDeclining,
		Description:       "SPDX license identifier from the repository's declared license.",
		Caveats: []string{
			"a license file can be added without contributor consent on transfer of ownership",
			"some projects declare a license in README without a LICENSE file or vice versa",
		},
	},
	"repo_files": {
		Type:              "repo_files",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryLowDeclining,
		Description:       "Presence of conventional project-hygiene files at standard repo paths (README, SECURITY, CODEOWNERS, .mailmap, CHANGELOG, CONTRIBUTING, AUTHORS, MAINTAINERS, GOVERNANCE).",
		Caveats: []string{
			"presence indicates project hygiene, not maintainer legitimacy — these files can be added or removed without contributor review",
			"zero-byte files are reported as absent — a placeholder stub is the cheapest form of fake hygiene and is not counted",
			"CODEOWNERS presence reports the file exists at one of the three locations GitHub's parser reads from; casing drift (e.g. lowercased 'codeowners') means GitHub won't actually gate reviews on it — inspect the reported path to judge",
			"when multiple variants of a family exist (e.g. README.md alongside a bare README), the canonical spelling is surfaced in path; the rest appear in alt_paths for analyst review",
			"symlinks are resolved to their targets; the recorded path is the resolved file, not the link itself",
		},
	},
	"ci_cd": {
		Type:              "ci_cd",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Detected CI/CD providers (github-actions, travis-ci, circleci, etc.).",
		Caveats: []string{
			"presence doesn't imply the CI actually gates anything — see ci_supply_chain_gate for the is-it-enforced form",
		},
	},
	"community_health_score": {
		Type:              "community_health_score",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "GitHub community-health percentage and list of missing community files.",
		Caveats: []string{
			"GitHub's community profile checks a fixed list of files calibrated to open-source norms, not all projects",
		},
	},
	"supply_chain_policy_config": {
		Type:              "supply_chain_policy_config",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Presence of supply-chain policy configuration (deny.toml, .cargo-audit-ignore, govulncheck config, etc.).",
		Caveats: []string{
			"presence doesn't imply enforcement — see ci_supply_chain_gate for the gated-in-CI form",
		},
	},
	"ci_supply_chain_gate": {
		Type:              "ci_supply_chain_gate",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Whether a declared supply-chain policy is invoked by at least one CI workflow.",
		Caveats: []string{
			"invocation-present is weaker than gate-required-to-pass; separating the two is a future refinement",
		},
	},
	"ci_action_pin_tightness": {
		Type:              "ci_action_pin_tightness",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Distribution of CI action pinning — sha_pinned, major_version_pinned, master_pinned, or unpinned.",
		Caveats: []string{
			"major-version pinning is the common baseline; sha-pinning is the hardened posture",
			"unpinned or master-pinned references are a recognized supply-chain risk",
		},
	},
	"unsafe_code_posture": {
		Type:              "unsafe_code_posture",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Rust unsafe-code posture per crate — forbid, deny, allow, or unattributed.",
		Caveats: []string{
			"forbid at crate root is the strong form; deny can be overridden in submodules",
			"non-Rust projects produce no signal of this type",
		},
	},
	"third_party_install_inputs": {
		Type:              "third_party_install_inputs",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "External scripts or binaries fetched during install beyond the package manager.",
		Caveats: []string{
			"curl-to-bash install patterns are harder to audit than package-manager installs",
			"existence of third-party inputs is not automatically negative — legitimate uses exist (e.g., pulling shell integration hooks)",
		},
	},
	"advisory_suppressions": {
		Type:              "advisory_suppressions",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "List of supply-chain advisory suppressions (e.g., cargo-deny ignores) with their stated rationales.",
		Caveats: []string{
			"count alone is noise; presence of written rationales is the real quality signal",
			"stale suppressions accumulate — age and rationale-freshness should be tracked separately when surfaced",
		},
	},
	"positive_absence_signal": {
		Type:              "positive_absence_signal",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Analyst explicitly checked for a known-bad pattern and confirmed its absence. Distinct from 'not examined'.",
		Caveats: []string{
			"only trustworthy when the checking methodology is recorded — 'I looked and it wasn't there' is weaker than 'I ran X against the full tree'",
			"absence of a pattern is only as strong as the coverage of the check",
		},
	},
	"scorecard-check": {
		Type:              "scorecard-check",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "OpenSSF Scorecard aggregate score plus per-check breakdown for a GitHub-hosted project. Sourced from api.securityscorecards.dev — Scorecard runs out-of-band and produces a regularly-refreshed corpus of supply-chain hygiene signals (branch protection, signed releases, code review, dangerous workflows, dependency-update tooling, etc.).",
		Caveats: []string{
			"the aggregate score is a weighted average across ~18 individual checks; two projects with the same score can have very different per-check shapes — compare check-by-check when the comparison matters",
			"a check score of -1 means 'not applicable' or 'could not be determined' (e.g., Signed-Releases is N/A on a project with no releases); these are not failures and shouldn't be summed as zeros",
			"absence (404 on the Scorecard API) is a real condition — Scorecard's crawler hasn't indexed every public project; an absence is information, not an error",
			"scores reflect the commit Scorecard last analyzed (recorded in repo.commit); a project that recently fixed a check may still report the prior result until Scorecard re-runs (roughly weekly per indexed project)",
			"Scorecard's check set evolves across releases — when comparing scores across time, compare the scorecard.version too or the comparison may be apples-to-oranges",
		},
	},

	// ================================================================
	// Criticality — "How critical is this?"
	// ================================================================

	"stars": {
		Type:              "stars",
		Group:             profile.SignalGroupCriticality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "GitHub star count.",
		Caveats: []string{
			"silently mutable — no historical star count is exposed via GitHub API",
			"vulnerable to mass star/unstar manipulation campaigns",
			"no way to distinguish organic growth from manipulation in a single observation",
		},
	},
	"forks": {
		Type:              "forks",
		Group:             profile.SignalGroupCriticality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "GitHub fork count.",
		Caveats: []string{
			"like stars, vulnerable to manipulation campaigns",
			"a high fork count on an abandoned project indicates continuing dependence on a dead upstream",
		},
	},
	"adoption": {
		Type:              "adoption",
		Group:             profile.SignalGroupCriticality,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Ratio of go.mod references to stars, indicating direct-vs-transitive adoption shape.",
		Caveats: []string{
			"the GitHub search API count is an approximation — it excludes private repos and is subject to indexing lag",
		},
	},
	"weekly_downloads": {
		Type:              "weekly_downloads",
		Group:             profile.SignalGroupCriticality,
		ForgeryResistance: profile.ForgeryLowDeclining,
		Description:       "Download count for a package over the last week, as reported by its registry's stats endpoint.",
		Caveats: []string{
			"counts are trivially gameable by botting downloads; treat as a floor, never a ceiling",
			"CI mirrors, proxy caches, and container image bases inflate counts without corresponding human users",
			"low download count on a new package is not automatically negative — legitimate projects start small",
			"use as one input to a criticality picture, never as a sole basis for a trust decision",
		},
	},
}
