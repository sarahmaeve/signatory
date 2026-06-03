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
// the rest.
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
			"the direct list excludes entries marked // indirect (those are counted in indirect_count only); it is sorted and de-duplicated so it diffs through the same set-diff path as the other ecosystems' *_dependencies signals",
			"unlike the registry ecosystems, indirect_count is a real count (go.mod exposes the MVS-forced transitive set via // indirect) rather than always 0; total_count is therefore direct + indirect, not equal to direct_count",
		},
	},
	"cargo_dependencies": {
		Type:              "cargo_dependencies",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Declared direct-dependency surface (normal + build, dev excluded) of the latest non-yanked crates.io version.",
		Caveats: []string{
			"crates.io's dependencies endpoint returns only directly-declared edges for the requested version; the resolved transitive graph is never available, so indirect_count is always 0 and total_count equals direct_count",
			"dev-dependencies are excluded as they are not pulled transitively by downstream consumers; build-dependencies are included because build.rs executes at consumer build time",
			"reflects the latest non-yanked version only; a dependency added then removed across intermediate versions is not surfaced",
		},
	},
	"npm_dependencies": {
		Type:              "npm_dependencies",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Declared direct-dependency surface (dependencies + optionalDependencies) of the latest published npm version.",
		Caveats: []string{
			"npm packument exposes only declared direct dependencies; the resolved transitive graph is never available, so indirect_count is always 0 and total_count equals direct_count",
			"reflects the latest published version only; a dependency added then removed across intermediate versions is not surfaced",
		},
	},
	"maven_dependencies": {
		Type:              "maven_dependencies",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Declared direct-dependency surface (project <dependencies>, test scope excluded) of the latest release POM, as groupId:artifactId coordinates.",
		Caveats: []string{
			"the POM declares only direct dependencies; the resolved transitive graph is never available, so indirect_count is always 0 and total_count equals direct_count",
			"test-scoped dependencies are excluded as they are not consumed transitively by downstream; <dependencyManagement> version pins are excluded as they are not actual dependencies",
			"version-managed dependencies whose version is inherited from a parent or BOM still surface by coordinate; only the groupId:artifactId identity is tracked, not the resolved version",
			"reflects the latest release version only; a dependency added then removed across intermediate versions is not surfaced",
		},
	},
	"gem_dependencies": {
		Type:              "gem_dependencies",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Declared runtime-dependency surface (development dependencies excluded) of the gem's displayed version, by dependency name.",
		Caveats: []string{
			"the rubygems.org gem JSON exposes only the displayed version's directly-declared dependencies; the resolved transitive graph is never available, so indirect_count is always 0 and total_count equals direct_count",
			"development dependencies are excluded as they are the gem's own test/build tooling and are not pulled transitively by downstream consumers",
			"reflects the displayed version only; a dependency added then removed across intermediate versions is not surfaced",
		},
	},
	"pypi_dependencies": {
		Type:              "pypi_dependencies",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Declared dependency surface (info.requires_dist, PEP 508 names, PEP 503-normalized) of the project's displayed version.",
		Caveats: []string{
			"requires_dist declares only direct dependencies; the resolved transitive graph is never available, so indirect_count is always 0 and total_count equals direct_count",
			"every requires_dist entry is included regardless of environment marker or extra gate; PyPI has no clean runtime/dev partition, and including all entries is the only policy that surfaces a dependency injected under an innocuous extra",
			"requires_dist is null for some old sdist-only releases, which surface as a zero dependency surface",
			"reflects the displayed version only; a dependency added then removed across intermediate versions is not surfaced",
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
	"commit_signing_keys": {
		Type:              "commit_signing_keys",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Distinct per-developer GPG key IDs that signed commits within the observation window. Web-flow keys (GitHub's managed signing key) are excluded.",
		Caveats: []string{
			"key IDs are taken from git's %GK placeholder — long key IDs (16 hex chars), not full fingerprints; collision-resistant in practice but cryptographically weaker than %GF would be",
			"signature validity is filtered upstream (only G/U/X/Y status — see signing.go classifySigning); revoked keys (R) and unsigned commits do not contribute key IDs",
			"a person rotating GPG keys produces distinct key IDs across rotations; burning one key does not catch the same human's earlier or later keys until identity-equivalence work lands (entity-burn1.md §11)",
			"web-flow keys are intentionally excluded — they are platform-managed credentials, not per-developer identities, and minting an entity for them would conflate platform trust with individual signer trust",
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
	"maintainer_email_set": {
		Type:              "maintainer_email_set",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "SHA-256 hashes of the maintainers' lowercased email addresses (sorted, no raw PII). Diffed by deltas to surface the axios-shape ATO precursor: a maintainer's associated email flipping to an attacker address.",
		Caveats: []string{
			"hashed for privacy — detects change, not identity; a benign email correction also registers as a transition and needs human triage",
			"npm exposes maintainer email in the packument and the owner controls it; absence of email on a maintainer entry is normal and contributes no hash",
			"an attacker who keeps the same email while compromising the account is invisible to this signal (pair with publish_origin_consistency / attestation_consistency)",
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
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Presence of Sigstore/SLSA build provenance attestations on published artifacts.",
		Caveats: []string{
			"attestation alone is not trust — a verifier must check it against a known-good build configuration",
			"forgery resistance reflects 'requires CI pipeline compromise' (High), not 'requires cryptographic key compromise' (Very High). Sigstore's cryptographic primitives are Very High, but signal-presence is achievable via workflow-runtime exfiltration without breaking those primitives — TanStack 2026-05-11 is the demonstrated case. See design/threat-landscape/2026-05-12-tanstack-mini-shai-hulud.md",
		},
	},
	"registry_publish_origin": {
		Type:              "registry_publish_origin",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Origin of registry publishing — oidc_ci, long_lived_token_ci, local_maintainer_machine, or unknown.",
		Caveats: []string{
			"oidc_ci is the hardened posture; local_maintainer_machine is the lowest trust tier",
			"CI-based publishing is only as trustworthy as the CI workflow's action-pin tightness",
			"forgery resistance is High, not Very High: oidc_ci classification is derived from attestation presence; a CI-pipeline-compromise attack (TanStack 2026-05-11) produces oidc_ci classification while signing malicious content with the legitimate workflow's identity. The classification is correct about what the publisher uses; it's not a verdict about runtime integrity",
		},
	},
	"crates_io_trusted_publishing": {
		Type:              "crates_io_trusted_publishing",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether crates.io trusted-publishing (OIDC) is configured for the crate.",
		Caveats: []string{
			"status is visible only after a first publish that used it — absence on a new crate is not automatically negative",
			"forgery resistance is High, not Very High: the same TanStack-shape exposure as npm trusted_publishing — OIDC binding doesn't guarantee runtime integrity. A CI-pipeline-compromise attack produces a valid attestation with the project's normal builder identity while signing malicious content",
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
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Presence of an OIDC trusted-publishing attestation on the latest published package version (npm's dist.attestations).",
		Caveats: []string{
			"present-and-valid cryptographically binds the published version to a source repo and commit SHA — strong provenance evidence but not a verdict on artifact safety",
			"absence is not automatically negative — older published versions predate trusted publishing, and the maintainer may have not opted in yet",
			"absence on a package that previously used trusted publishing IS strongly negative — the axios attack pattern — but detecting the transition requires comparing across versions; publish_origin_consistency is the cross-version complement to this snapshot signal",
			"forgery resistance is High, not Very High: the OIDC binding cryptographically guarantees the attestation chain but doesn't guarantee the workflow's runtime memory was integrity-bounded at token issuance. TanStack 2026-05-11 produced valid attestations with the project's normal release.yml@refs/heads/main builder identity by extracting the runner's OIDC token from /proc memory after pull_request_target + cache-poisoning compromise. The signal correctly observes 'publisher uses trusted publishing'; readers should not transitively conclude 'artifact is safe'",
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
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Consistency of publish provenance across recent versions: presence-transitions on OIDC attestations plus count of distinct publisher accounts.",
		Caveats: []string{
			"a single publisher across many versions with consistent attestation presence is the healthy pattern — transitions are anomaly signals, not verdicts",
			"legitimate reasons to transition include maintainer handoff, CI pipeline migration, or a first adoption of trusted publishing — these produce false positives worth investigating, not dismissing",
			"the axios 2026 forensic specifically called out the broken attestation chain as the detection-relevant fingerprint — this signal captures that shape across versions",
			"the _npmUser.name field is the registry's publisher stamp and cannot be rewritten post-publish; it's higher-forgery-resistance than maintainer lists which are self-declared",
			"forgery resistance is High, not Very High: presence-transitions and publisher consistency are observable, but a CI-pipeline-compromise attack (TanStack 2026-05-11) preserves both — it rides the legitimate workflow and signs the malicious tarball with the project's normal attesting identity, leaving attestation_transitions=0 and unique_publishers=1. The PyPI sibling attestation_consistency has added workflow_ref_transitions to close the careful-variant gap; the npm side awaits the same extension (deferred behind the Rekor-vs-attestations-URL question)",
		},
	},
	"attestation_consistency": {
		Type:              "attestation_consistency",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Consistency of PEP 740 Sigstore attestations across recent versions. Detects two dimensions of break: transitions from attested to unattested publishing (the axios fingerprint of credential-theft attacks that bypass CI), and changes in the attesting workflow ref across attested versions (the TanStack-shape careful-variant where every version is attested but the builder identity changed).",
		Caveats: []string{
			"a transition from unattested to attested is positive (adoption) not negative",
			"publisher_changed=true across attested versions may indicate legitimate CI migration or may indicate account takeover — the analyst disambiguates",
			"bounded to last N versions; a gap farther back is invisible",
			"not emitted for packages that never adopted trusted publishing (progressive probe: latest + first prior both unattested → early exit)",
			"workflow_ref_transitions counts adjacent workflow-string differences across checked[] in newest-first order; presence transitions (e.g., '' → 'release.yml') count toward it because the strings differ — pair with transition_detected to disambiguate presence-change from workflow-change",
			"forgery resistance is High, not Very High: a TanStack-shape attack that rides the legitimate workflow preserves attestation chain consistency on every axis except workflow-ref content. workflow_ref_transitions catches the careful-variant where the workflow IDENTITY changes; presence-consistency and publisher-consistency alone are not sufficient verdicts on artifact safety",
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
			"differential by construction: does NOT fire on born-malicious packages whose first published version is already weaponized (no clean baseline to cross from). The companion signal source_evolution_concern covers that class.",
		},
	},
	"source_evolution_concern": {
		Type:              "source_evolution_concern",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Boolean+pointer summary derived from source_evolution_matrix: a single version's AST counts independently fire two or more rare-on-benign catalog features (env-credential reads, sensitive-path reads/writes, exec, XOR-assign, cloud-metadata calls, dynamic-eval, install-hook overrides, init, credential-decrypt), without requiring a cross-version transition. Names the first chronological concerning version and which features fired. Companion to source_evolution_anomaly: where anomaly catches clean→weaponized transitions, concern catches born-malicious packages whose first published version already carries the payload (the dominant typo-squat shape, exemplified by the 2026-05-24 Trapdoor cargo crates and the 2026-06-03 spadata PyPI Roblox-cookie stealer, which reads robloxcookies.dat and DPAPI-decrypts it).",
		Caveats: []string{
			"threshold is conservative (2+ rare-on-benign features required); a single field firing alone — even one ordinarily near-zero on benign code — does not trip the boolean",
			"three Counts fields are deliberately excluded from the rare-on-benign subset because they are naturally non-zero on legitimate code: import_time_call_sites (cargo build.rs idiom + python module-scope getLogger), network_call_sites (any HTTP client crate), base64_decode_calls (crypto crates). Concerning_features will never name them.",
			"a benign-but-high-risk package (e.g. an ssh keychain manager that legitimately reads ~/.ssh AND execs a shell) can fire the signal; the analyst reads the matrix row at the first concerning version to classify intent",
			"absence does not mean clean — a sleeper that does no catalog-matched I/O until exploited would produce zero rare-on-benign counts and stay quiet here; the metadata signals carry the load until source diverges",
		},
	},
	"artifact_source_concern": {
		Type:              "artifact_source_concern",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "The in-situ source_evolution concern (same Counts catalogs, same rare-on-benign subset) evaluated over the PUBLISHED registry artifact's source instead of the git clone. Catches a born-malicious payload that ships in the sdist/tarball but is absent from — or has no — source repo, which the clone-based source_evolution_concern structurally cannot see. Carries the artifact's AST Counts plus the concern verdict (present/version/features). Closes the artifact-not-clone half of the CVE-2024-3094 gap for source-level (not just file-presence) analysis; the spadata 2026-06 Roblox-cookie stealer is the motivating shape.",
		Caveats: []string{
			"single published version → in-situ concern only; there is no cross-version artifact history, so the differential anomaly is not computed on this path",
			"requires only the registry artifact_url, not a clone — but the current dispatch wiring still gates it behind clone resolution (a documented limitation: a package declaring NO source repo is skipped until the collector is moved to the registry layer)",
			"gem is not covered (its two-pass outer/inner archive walk isn't handled here) — a documented gap shared with the exfil and build-script artifact scanners",
			"reads artifact bytes as data only: streamed through the header-only walker, never written to disk, never executed; oversized source files are recorded in the walk's SkippedScans rather than analyzed",
			"the analyzer's conservative static-resolution gaps (no data-flow, f-strings, `+` concatenation) apply identically here — a false negative is acceptable, a false anomaly is not",
		},
	},
	"artifact_url": {
		Type:              "artifact_url",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "URL of the published source-distribution artifact (npm dist.tarball, PyPI sdist URL, etc.) plus the publisher-side metadata (version, integrity hash, gitHead when present) needed to fetch and pair it to a repo commit.",
		Caveats: []string{
			"emitted by the registry collector; CONSUMED by the artifact-vs-repo collector via the in-run accumulator — not a standalone analyst signal, but a structured handoff between collectors",
			"git_head is publisher-stamped and only npm v≥5 carries it reliably; older publishes and other registries omit it, forcing the downstream collector to fall back to tag-name matching",
			"integrity is the registry's own hash of the tarball, not a content hash signatory computed; useful as a cross-check, not as ground truth",
			"absence is meaningful: a registry response without dist.tarball is rare in modern publishes and itself a hygiene observation",
		},
	},
	"artifact_repo_divergence": {
		Type:              "artifact_repo_divergence",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "One-directional set-difference: files present in the release tarball but absent from the git tree at the corresponding commit/tag. Surfaces the load-bearing signature of CVE-2024-3094 (xz-utils, build-to-host.m4 shipped only in the dist tarball).",
		Caveats: []string{
			"one-directional by design: files in repo but absent from tarball are NOT surfaced — every healthy publishing pipeline (npm .npmignore, PyPI MANIFEST.in, etc.) intentionally excludes tests/docs/.github/, and the resulting ~200-entry samples were drowning out actual signal in dogfood",
			"header-only walk: file presence is detected, byte-level differences (same path, different content) are not — that's a separate phase deferred until dogfood traces show it's needed",
			"a wrapping top-level directory in the tarball (npm 'package/', autotools '<name>-<version>/', PyPI sdist same) is auto-stripped before comparison; without that, every tarball file would falsely register as divergent",
			"pair_confidence reports whether the tarball↔commit pairing was an exact gitHead match (npm v≥5) or a tag-name guess (everywhere else); the synthesist must weight tag-match evidence less heavily than exact-match evidence",
			"healthy autotools projects ship configure / Makefile.in / aclocal.m4 in the tarball but not in git; the categorizer marks these as 'generated' so the signal payload distinguishes legitimate dist-prep noise from suspicious extras",
			"unresolved pair_confidence is recorded as positive_absence rather than a divergence signal — 'we couldn't even pair this' is a hygiene fact about the project's release process, not a finding about its contents",
			"the categorizer emits an 'agent_config' bucket for AI-instruction files (.cursorrules, CLAUDE.md, AGENTS.md, .claude/, .cursor/rules/, .aider.conf.yml, .zed/, .continue/, .windsurfrules) per the Trapdoor 2026-05 campaign; a file in this bucket appearing in the tarball but absent from git at the paired commit is the xz-precedent applied to AI-config injection — Trapdoor weaponized exactly this carrier shape",
			"exfil_hosts_in_artifact carries exfil-host literals (the same exfilwatch host list as exfil_capture_host) found by content-scanning the PUBLISHED artifact's source — not just file presence; each hit is tagged path_in_repo, and a hit whose path is absent from the git tree is the strongest read (an exfil sink present only in what was published, the spadata 2026-06 / xz shape). Complements exfil_capture_host, which scans the source clone; this catches the case where the clone is clean but the uploaded sdist is not",
		},
	},
	"exfil_capture_host": {
		Type:              "exfil_capture_host",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Literal references in package source to HTTP-capture-as-a-service hosts (webhook.site, requestbin.com, beeceptor.com, oast.*, etc.) plus dual-use webhook exfil endpoints (discord.com/api/webhooks, discordapp.com/api/webhooks) — services whose operational properties (no signup or webhook-URL-as-secret, public-URL-keyed delivery) make their presence in published library code structurally malware-shaped. The BufferZoneCorp campaign (May 2026) exfiltrated to webhook.site/<UUID> from package init(); the spadata PyPI stealer (June 2026) POSTed a decrypted Roblox cookie to a hardcoded Discord webhook.",
		Caveats: []string{
			"literal substring match only; obfuscated literals (XOR, base64, runtime concatenation) defeat the scan and produce no hit — separate obfuscation patterns catch those",
			"a hit in test fixtures, README files, or webhook-debugging-tool source is data, not a verdict — the analyst weights by file role",
			"empty hits is a positive observation (we checked, found nothing), not silence; the signal is always emitted when a clone is available",
			"the payload also carries a skipped list: files not read because they exceed the 2 MiB scan cap (a host literal lives in human-written source, never this large) or are not regular files; a non-empty skipped list means those paths were not examined, so the absence of a hit there is a gap, not a clearance",
			"the host list is curated in-binary at compile time; updating membership is a code commit, not a remote pull (per ANTIPATTERNS.md no-subscription-list rule)",
		},
	},
	"build_script_concern": {
		Type:              "build_script_concern",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Heuristic content scrutiny of author-written build/install scripts (setup.py, build.rs, extconf.rb, configure.ac, hand-written *.m4) in the PUBLISHED artifact — the content follow-on to the presence-only build_script_present signals and item #2 of the CVE-2024-3094 gap analysis. Each finding names a behaviour class (decode / eval_exec / network_fetch / high_entropy_literal), a severity, and whether the path is also in the git tree.",
		Caveats: []string{
			"heuristic token + entropy scan, NOT a parser — language-neutral by design so it covers m4 / shell / Ruby that have no AST analyzer; obfuscation that splits tokens across constructs can evade it (the AST analyzer is the deeper, per-language complement)",
			"generated/vendored autotools output (configure, config.status, aclocal.m4, libtool lt*.m4) is deliberately excluded — huge and legitimately full of eval/base64-shaped content; the xz payload lived in a hand-written macro, which IS scanned",
			"severity is the load-bearing field: a single behaviour class alone (a configure.ac that merely shells out) is informational; only a high-entropy literal or two co-occurring classes (decode+exec, fetch+exec) escalate to strong — mirrors the source_evolution concern rare-on-benign discipline",
			"path_in_repo=false on a strong finding is the strongest read (a weaponized build input present only in what was published, the xz shape); findings are descriptive evidence with a snippet, not a verdict — a Layer-2 analyst judges intent",
			"emitted only when at least one finding is present; its absence alongside a present artifact_repo_divergence means the build scripts were scanned and nothing was flagged",
			"coverage gaps: package.json lifecycle scripts (needs scripts-field extraction) and the gem two-pass walk (scanners not yet threaded through the inner data.tar.gz) are not scanned — documented, shared with the exfil scanner",
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
	"recent_downloads": {
		Type:              "recent_downloads",
		Group:             profile.SignalGroupCriticality,
		ForgeryResistance: profile.ForgeryLowDeclining,
		Description:       "Recent download count for a crate from crates.io's first-party stats (last 90 days).",
		Caveats: []string{
			"counts are trivially gameable by botting downloads; treat as a floor, never a ceiling",
			"crates.io's recent_downloads window is ~90 days; not directly comparable to npm's 7-day window",
			"first-party stat — no third-party endpoint needed, unlike npm",
		},
	},

	// ================================================================
	// Publication — Cargo-specific signals
	// ================================================================

	"build_script_present": {
		Type:              "build_script_present",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the latest published crate version declares a build.rs script — Rust's equivalent of a postinstall hook, executing arbitrary code at compile time.",
		Caveats: []string{
			"build.rs is extremely common in legitimate crates (native bindings, code generation, feature detection) — presence alone is not negative",
			"the signal is analogous to npm's postinstall_present: it raises the attack surface area, not the probability of attack",
			"has_build_script is per-version metadata set by cargo at publish time — cannot be added post-publish",
		},
	},
	"build_script_introduced": {
		Type:              "build_script_introduced",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether a build.rs script appeared in the latest crate version where prior versions lacked one. Longitudinal complement to build_script_present — the cargo analog of postinstall_introduced.",
		Caveats: []string{
			"transitions have legitimate causes — native binding adoption, code-gen migration — so a true positive is an anomaly flag, not a verdict",
			"window is bounded (last N versions by publish time); a build script introduced farther back looks indistinguishable from one that was always there",
		},
	},
	"yanked_release_count": {
		Type:              "yanked_release_count",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Count of yanked/withdrawn versions in the package's version history. Yanking is an irreversible registry-side operation requiring owner credentials (crates.io, PyPI).",
		Caveats: []string{
			"yanking is normal maintenance (buggy releases, security patches) — a nonzero count is expected for active packages",
			"abnormally high counts relative to total versions may indicate cleanup of a compromised publishing spree",
			"yanked versions remain in the index but cannot be freshly resolved — the signal captures historical shape, not current availability",
		},
	},
	"sdist_only_present": {
		Type:              "sdist_only_present",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the latest published PyPI version distributes only source distributions (sdist) with no pre-built wheels. An sdist-only release executes setup.py at install time — PyPI's equivalent of npm's postinstall or cargo's build.rs.",
		Caveats: []string{
			"sdist-only is common for legitimate packages with C extensions or complex build requirements — presence alone is not negative",
			"the attack surface is real: setup.py runs arbitrary Python with full system access during pip install",
			"pure-Python packages that drop wheels force setup.py execution where none was previously needed — the transition is the anomaly, not the absolute state",
		},
	},
	"sdist_only_introduced": {
		Type:              "sdist_only_introduced",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the latest version distributes only sdists where prior versions included pre-built wheels. Longitudinal complement to sdist_only_present — dropping wheels forces setup.py execution on every install, the Python analog of npm's postinstall_introduced.",
		Caveats: []string{
			"transitions have legitimate causes — build system migration, platform-specific packaging changes — so a true positive is an anomaly flag, not a verdict",
			"window is bounded (last N versions by publish time); a transition farther back is indistinguishable from always-sdist",
			"a package that was always sdist-only gets introduced_recently=false, which is the correct stable-state signal",
		},
	},
	"owner_count": {
		Type:              "owner_count",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Count of crate owners (users + teams) from crates.io's /owners endpoint. Bus-factor signal.",
		Caveats: []string{
			"crates.io owner lists are authoritative and append-only within a session — higher forgery resistance than npm's self-declared maintainers list",
			"low count (1 user, no team) is a governance concern independent of the owner's trustworthiness",
			"team membership is opaque — a team of 1 looks like group ownership but isn't",
		},
	},
	"owner_team_present": {
		Type:              "owner_team_present",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether at least one crates.io team (not just individual users) owns the crate. Team ownership is a governance positive.",
		Caveats: []string{
			"team presence is a structural governance signal — it doesn't certify that the team has active members or review processes",
			"a team of 1 is indistinguishable from no team at the API level; the signal can't penetrate team membership",
		},
	},
	"proc_macro_crate": {
		Type:              "proc_macro_crate",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the crate is a procedural macro — code that executes inside rustc at compile time. Proc macros run with full system access during compilation of any downstream crate that uses them.",
		Caveats: []string{
			"proc macros are extremely common in legitimate Rust code (derive macros, attribute macros) — presence alone is not negative",
			"the signal flags a distinct attack surface: a compromised proc-macro crate achieves code execution on every developer's machine that compiles code depending on it, without any runtime execution of the crate itself",
			"detection is from source (Cargo.toml [lib] proc-macro = true); not available without a clone",
		},
	},
	"agent_config_files": {
		Type:              "agent_config_files",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryLowDeclining,
		Description:       "Inventory of AI-agent configuration files present in the repo (CLAUDE.md, AGENTS.md, .cursorrules, .claude/settings.json, .cursor/rules/*.mdc, .aider.conf.yml, .zed/settings.json, .continue/config.json, .windsurfrules). Cross-ecosystem: emitted on any cloned repo regardless of language.",
		Caveats: []string{
			"presence is hygiene-neutral by default — many legitimate projects ship per-repo agent instructions to standardize tool behavior",
			"the signal pairs with agent_config_content_injection: presence makes the file visible to analysts; the content-injection signal flags concerns about contents",
			"absence is a meaningful positive — the project does not direct AI agents at repo level, so any agent that scans this repo operates from its built-in priors only",
			"new since 2026-05 in response to the Trapdoor crypto-stealer campaign (design/threat-landscape/2026-05-24-trapdoor-crypto-stealer.md), which weaponized .cursorrules and CLAUDE.md as zero-width-Unicode prompt-injection carriers",
		},
	},
	"agent_config_content_injection": {
		Type:              "agent_config_content_injection",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Content-injection-surface findings on AI-agent configuration files: invisible Unicode (zero-width family, bidi controls, tag block), markdown HTML comments with imperative-mood prose, parameterized image URLs, lexical injection patterns, and long base-N encoded blobs. Per design/anti-subversion.md the primitives are byte-level / regex structural signals an attacker cannot decouple from a functional payload.",
		Caveats: []string{
			"empty findings is a positive observation — the scanner ran on every detected agent-config file and found no structural injection primitive",
			"lexical_injection has the highest false-positive rate of the primitives; a project whose own topic is prompt-injection research will fire repeatedly. The analyst weights by file role and project topic",
			"markdown_comment heuristic fires on imperative-shape prose (first word is a catalog verb, or two-plus catalog verbs in the body) above a 32-char threshold; it does not catch every prompt-injection comment shape",
			"encoded_blob thresholds are calibrated to skip SHA-256/SHA-512 hashes, Ed25519/RSA signatures, and JWT signatures; the false-negative tradeoff is real for legitimate-looking shorter payloads",
			"Truncated=true on a per-file entry means the file exceeded contentinjection.MaxScanFileBytes; findings reflect the in-cap prefix only",
		},
	},
	"mfa_required": {
		Type:              "mfa_required",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the gem's publisher has enabled mandatory multi-factor authentication for pushes. MFA-required gems cannot be published with a compromised API key alone.",
		Caveats: []string{
			"MFA-required reflects the gem owner's current policy — it does not retroactively certify older versions",
			"rubygems.org enforces MFA at the account level for high-download gems since 2022; this signal captures the per-gem explicit opt-in",
		},
	},
	"native_extension_present": {
		Type:              "native_extension_present",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the latest published gem version includes native extensions (platform != 'ruby'). Native extensions execute arbitrary code at install time via extconf.rb — the gem equivalent of cargo's build.rs or npm's postinstall.",
		Caveats: []string{
			"native extensions are common in legitimate gems (nokogiri, ffi, pg, mysql2) — presence alone is not negative",
			"the signal flags a distinct attack surface: extconf.rb runs with full system access during gem install",
			"platform is per-version metadata set by rubygems at publish time — cannot be changed post-publish",
		},
	},
	"native_extension_introduced": {
		Type:              "native_extension_introduced",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether a native extension appeared in a recent version where prior versions were pure Ruby. Longitudinal complement to native_extension_present — the gem analog of build_script_introduced.",
		Caveats: []string{
			"transitions have legitimate causes — adopting a C extension for performance, wrapping a new system library — so a true positive is an anomaly flag, not a verdict",
			"window is bounded (last N versions by publish time); an extension introduced farther back is indistinguishable from one always present",
			"the BufferZoneCorp campaign weaponized extconf.rb in v0.4.0 after pure-Ruby v0.1.0–v0.3.0 — this signal catches that exact shape",
		},
	},
	"version_publish_burst": {
		Type:              "version_publish_burst",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether multiple versions were published within a short time window (72 hours). Version-pumping is a common supply-chain attack tactic: ship benign versions quickly to build history, then weaponize the latest.",
		Caveats: []string{
			"initial releases of a new gem legitimately publish several versions in rapid succession (0.1.0, 0.1.1, 0.2.0 in a week as the API stabilizes)",
			"the signal is strongest when combined with young account age and low download counts",
			"the 72-hour window matches the BufferZoneCorp campaign cadence (4 versions in 3 days) — longer windows would capture more legitimate rapid-iteration patterns",
		},
	},
	"publisher_account_class": {
		Type:              "publisher_account_class",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Heuristic classification of each extracted publisher login as human / bot / service-account / unknown, plus summary counts. Surfaces bot and service-account publishers as a distinct risk class — credential surfaces on automation accounts are operationally different from human-maintainer accounts (long-lived PATs, often without 2FA, credentials stored in CI configs rather than personal vaults).",
		Caveats: []string{
			"forgery resistance is medium-declining: an attacker can rename their account to bypass any pattern; the signal is heuristic risk-stratification, not a verdict",
			"v1 uses name-pattern matching only — no GitHub type:Bot lookups, no activity-shape analysis, no allowlist of known automation accounts",
			"v1 patterns are hyphen-strict: '-bot' / '-ci' / '-deploy' / '-svc' / '-release' / '-publisher' / '-automation' suffixes plus GitHub's '[bot]' suffix. Logins without a hyphen separator (e.g., 'deploybot', 'npmbot') fall through to 'human' — accepted false-negative tradeoff for low false-positive rate",
			"false positives are possible — a human named with a coincidentally-matching pattern would misclassify. The matched_pattern field makes the classification auditable; analyst layer disambiguates",
			"the signal describes publishers but emits on the package entity (login field links to the corresponding identity:pypi/<login> entity which is minted separately via Path E). Mirrors owner_type's emission convention",
		},
	},
	"latest_attestation_builder": {
		Type:              "latest_attestation_builder",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Publisher identity the latest version's Sigstore provenance attestation binds to: builder_kind, source_repository, workflow, environment, and source_revision (the SHA stamped in the Fulcio cert's source-repo-digest extension). Consolidating contract over data the publication-integrity collectors already extract — provides a stable namespace for sketch 5 (workflow_ref_transitions) and future composites to consume without merging fields from sibling signals.",
		Caveats: []string{
			"forgery resistance is contingent on the attesting workflow being integrity-bounded at attestation time; the TanStack 2026-05-11 compromise rode a legitimate workflow and produced a valid attestation with the project's normal builder identity",
			"PEP 740 on PyPI carries the publisher block (kind/repository/workflow/environment) directly; the npm side surfaces only an attestations URL marker in the inline registry block and would require a follow-up fetch to populate the same shape",
			"workflow is the workflow path (e.g., 'release.yml' or '.github/workflows/release.yml') without the ref/branch suffix — the @ref portion is on a separate Fulcio extension OID not currently extracted",
			"extraction_status reports ok (full publisher block parsed) or no_attestation (Integrity API returned 404); fetch errors record as retryable absence instead of an extraction_status value",
		},
	},
	"commit_publish_cadence_divergence": {
		Type:              "commit_publish_cadence_divergence",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Temporal gap between most-recent push to the source repo and most-recent publish to the registry. Four shapes: synchronized, active-repo-paused-publishes, active-publishes-fallow-repo, and both-fallow. Derived signal — reads sibling collectors' last_commit (or last_push), last_publish, and (when available) version_count emissions via the in-run accumulator. When version_count is visible, the emission carries a prior_version_count field — the disambiguating context that lets a reader distinguish a high-version-count package on a paused cadence (operationally stable) from a low-version-count package on the same cadence (more likely abandoned).",
		Caveats: []string{
			"cadence is observable but not cryptographic — an attacker controlling both source and publish paths can fake either timestamp",
			"the 'synchronized' threshold (|divergence| <= 2 days) and the 'fallow' threshold (60 days) are arbitrary defaults; values close to either edge are weak signal on their own",
			"partial inputs (no commit-side signal, or no last_publish) produce no emission rather than an absence — the collector treats partial data as 'doesn't apply' to the entity, not 'failed'",
			"both-fallow trumps the divergence shapes — a 200-day commit + 201-day publish is reported as both-fallow, not synchronized, because divergence is only meaningful when at least one side is recent",
			"prior_version_count is absent (field omitted, not zeroed) when no version_count sibling signal is in the in-run accumulator — typical for repo-only entities or partial runs",
			"the shape value alone does not distinguish stable-foundational (e.g., 200+ releases over a decade, recent quiet stretch) from abandoned-thin-history (e.g., 3 releases, last touched a year ago); pair shape with prior_version_count for the disambiguating read",
		},
	},
	"git_url_dep_introduced": {
		Type:              "git_url_dep_introduced",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether the latest published version introduces a dependency whose specifier points at a git source (github:/gitlab:/bitbucket: short form, or git+https://, git+ssh://, git://, git+http:// URL forms) where prior versions in the window had no git-URL deps. The transition is the anomaly — consistent presence and consistent absence are both healthy.",
		Caveats: []string{
			"a git-URL dep is not by itself malicious — legitimate uses include prerelease testing against an upstream PR or temporarily pinning to a fork waiting on a merge",
			"the pinned_sha field on each emitted dep entry is non-empty only when the ref is a 40-hex SHA-1; tag-pinned and branch-pinned refs leave it empty (tags are mutable on GitHub by default and branches are mutable by design)",
			"tarball URLs (https:// to .tgz/.tar.gz) are a separate non-registry vector not covered here — this signal is git-fetch-specific",
		},
	},
	"version_unpublish_observed": {
		Type:              "version_unpublish_observed",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Versions present in the registry's publish-event log but absent from the current versions map — the gap that signals a version was published and subsequently unpublished. The signal is direction-agnostic on cause (maintainer cleanup, registry-security takedown, or both at once); cleanup-after-compromise is the case where this signal carries information not derivable from the surviving registry state alone.",
		Caveats: []string{
			"does not distinguish causes — maintainer cleanup and registry takedowns produce the same gap; both can coexist in one package",
			"recency reflects when the now-unpublished version was originally published, not when it was removed — the registry does not expose unpublish timestamps",
			"the unpublished_versions list is capped at 10 most-recent by publish time; list_capped=true indicates more exist",
			"a compromise burst lives inside this signal's unpublished_versions list (those versions are gone from pkg.Versions); version_publish_burst sees only the surviving cadence. Tight clusters of recent unpublishes in the per-version timestamps are the discrimination mechanism",
		},
	},
	"gpg_signature_present": {
		Type:              "gpg_signature_present",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Whether the latest published artifact version has a GPG signature. On Maven Central (.asc file, mandatory); on PyPI (legacy has_sig field, deprecated since 2023 in favor of PEP 740 Sigstore attestations).",
		Caveats: []string{
			"presence confirms a signature exists but does not verify its validity or the signing key's trustworthiness — verification is a Phase B.5 concern",
			"Maven Central mandates GPG signing, so absence is more alarming on Central than on registries where signing is optional",
			"on PyPI, has_sig is a legacy field — new uploads cannot set it (disabled May 2023); presence indicates the artifact was signed before the deprecation, absence on post-2023 uploads is expected behavior, not a red flag",
			"PyPI's successor to GPG signing is PEP 740 Sigstore attestations (GA November 2024) — see the Integrity API endpoint for modern provenance signals",
		},
	},
	"author_drift": {
		Type:              "author_drift",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Count of distinct author strings across recent versions. A change in the authors field between versions may indicate account takeover or maintainer handoff.",
		Caveats: []string{
			"the authors field is self-declared in the gemspec/POM — it can be set to anything by whoever publishes",
			"legitimate author drift occurs on maintainer succession, corporate sponsorship changes, and name updates",
			"forgery resistance is medium-declining because the field is publisher-controlled, but a change IS visible in the immutable version history",
		},
	},
	"missing_artifact_count": {
		Type:              "missing_artifact_count",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Number of versions listed in maven-metadata.xml whose artifact jars are absent (404) on repo1.maven.org. Maven Central does not support formal yanking, but artifacts can be removed or fail to sync — a version listed in metadata but missing its jar is the Maven analog of a yanked release.",
		Caveats: []string{
			"a missing artifact may indicate intentional removal, sync failure, or packaging variant (e.g. -sources instead of plain jar)",
			"the count covers only the most recent cross-version window, not the full version history",
			"derived from the same HEAD requests used for timestamp resolution — zero additional HTTP calls",
		},
	},
	"signature_consistency": {
		Type:              "signature_consistency",
		Group:             profile.SignalGroupPublication,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Whether GPG signatures (.asc) are present consistently across the recent version window on Maven Central. A transition from signed to unsigned (or vice versa) across versions indicates a governance or tooling change worth investigating.",
		Caveats: []string{
			"checks .asc presence via HEAD, not cryptographic verification — a present but invalid signature is counted as signed",
			"Maven Central mandates GPG signing for new uploads, so inconsistency typically indicates older pre-requirement versions or migration artifacts",
			"this is an artifact-level signal (the .jar was signed for the registry), distinct from git tag signing which the git collector handles separately",
		},
	},

	// ================================================================
	// Pull-request queue (pr-analyzer collector) — shape of inbound,
	// still-open contributions. Entries span vitality / governance /
	// hygiene; grouped here by source for maintainability, with the
	// per-entry Group field carrying the real classification.
	//
	// All medium-declining: every value is GitHub-API PR metadata,
	// mutable post-observation (edits, force-push, relabeling) and a
	// point-in-time snapshot of the OPEN queue — not merge history.
	// ================================================================

	"open_pr_count": {
		Type:              "open_pr_count",
		Group:             profile.SignalGroupVitality,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Number of currently-open pull requests on the repository, from full pagination of the listing endpoint.",
		Caveats: []string{
			"a snapshot of pending inbound work at observation time, not merge throughput — a high count may mean healthy contribution flow or an unreviewed backlog",
			"this count is the true total (the listing is fully paginated); the per-PR rate/distribution signals below are computed over a bounded sample of it",
		},
	},
	"pr_author_association_distribution": {
		Type:              "pr_author_association_distribution",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Distribution of GitHub author_association values (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, NONE, …) across the sampled open PRs.",
		Caveats: []string{
			"computed over a bounded sample of the most-recent open PRs (default 30); larger queues are sampled, not fully enumerated, and the truncation is logged",
			"author_association is GitHub's point-in-time classification of the author's relationship to the repo and is connector-defined vocabulary; it shifts as people gain or lose membership",
			"describes who is proposing changes, not who is merging them — open PRs include contributions that may never land",
		},
	},
	"pr_first_time_contributor_share": {
		Type:              "pr_first_time_contributor_share",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Share of sampled open PRs whose author_association marks the author as first-time or unaffiliated (FIRST_TIME_CONTRIBUTOR, FIRST_TIMER, NONE).",
		Caveats: []string{
			"bounded sample (default 30 most-recent open PRs); larger queues are sampled with truncation logged",
			"a high first-timer share is ambiguous — it can mark a welcoming, popular project or an influx of low-quality or hostile PRs; interpret alongside review-gate signals",
			"open-queue snapshot, not merge history",
		},
	},
	"pr_test_touch_rate": {
		Type:              "pr_test_touch_rate",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Share of sampled open PRs that touch test files, by pr-analyzer's path and filename heuristics.",
		Caveats: []string{
			"bounded sample (default 30 most-recent open PRs); truncation logged",
			"heuristic name/path matching (e.g. *_test.go, tests/, spec/) — convention- and language-dependent, with both false positives and misses",
			"measures whether tests are touched, not whether coverage is adequate or the tests are meaningful",
		},
	},
	"pr_dependency_manifest_touch_rate": {
		Type:              "pr_dependency_manifest_touch_rate",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Share of sampled open PRs that modify a dependency manifest or lockfile (go.mod, package.json, Cargo.toml, requirements.txt, …).",
		Caveats: []string{
			"bounded sample (default 30 most-recent open PRs); truncation logged",
			"flags inbound dependency changes for review; does not distinguish additions from upgrades or removals",
			"open-queue snapshot — these changes are proposed, not yet merged",
		},
	},
	"pr_agent_config_touch_rate": {
		Type:              "pr_agent_config_touch_rate",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Share of sampled open PRs that touch AI-agent configuration files (CLAUDE.md, .cursorrules, .claude/, …) — a cross-ecosystem prompt-injection carrier surface.",
		Caveats: []string{
			"bounded sample (default 30 most-recent open PRs); truncation logged",
			"the agent-config catalog is pr-analyzer's own and can drift from signatory's internal/agentconfig taxonomy until the two are reconciled",
			"a touch is not itself malicious — legitimate agent-config maintenance looks identical; pair with content-injection scanning of the changed files",
		},
	},
	"pr_oversized_share": {
		Type:              "pr_oversized_share",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Share of sampled open PRs whose total changed lines (additions + deletions) exceed the large-PR threshold the collector passes to pr-analyzer.",
		Caveats: []string{
			"bounded sample (default 30 most-recent open PRs); truncation logged",
			"the threshold is a heuristic; signatory's collection path has no org config, so a built-in default applies — large PRs are a review-burden signal, not a correctness one",
			"LOC counts are GitHub-reported and exclude binary / generated-file context",
		},
	},
	"pr_language_distribution": {
		Type:              "pr_language_distribution",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Distribution of programming languages detected across the files changed in the sampled open PRs.",
		Caveats: []string{
			"bounded sample (default 30 most-recent open PRs); truncation logged",
			"languages are inferred from file extensions, not content; vendored or generated files inflate counts",
			"reflects the languages of proposed changes in the open queue, not the repository's overall composition",
		},
	},
	"pr_queue_samples": {
		Type:              "pr_queue_samples",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Per-PR drill-down detail behind the open-PR-queue aggregate signals: one record per sampled PR (number, author, association, LOC, tests/manifests/agent-config touches, languages).",
		Caveats: []string{
			"the retained detail carrier for the pr_* aggregates above — not a trend signal; field-level deltas on this array are expected to be noisy and should not be alerted on",
			"bounded sample (default 30 most-recent open PRs); truncation is reflected in the carrier's truncated flag",
			"open-queue snapshot at observation time, mutable via the GitHub API",
		},
	},

	// ================================================================
	// Pull-request changelist defense (pr-scan command) — content-
	// derived findings about the files a SINGLE pull request changes,
	// pinned to the PR head commit. Distinct from the pr-analyzer queue
	// aggregates above (which are shape-derived over the open-PR queue).
	// A pre-merge gate: "is THIS changeset trying to compromise us?"
	// ================================================================

	"pr_content_injection": {
		Type:              "pr_content_injection",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "Hidden or obfuscated instruction-injection primitives (zero-width unicode, bidi controls, tag blocks, markdown-comment imperatives, exfil-shaped image URLs, lexical injection, encoded blobs, confusable scripts) detected in a file a pull request changes.",
		Caveats: []string{
			"scoped to the PR's changed files at the head commit — a clean result does not vouch for the rest of the repo",
			"detects surface patterns; runtime-concatenated or custom-encoded payloads can evade it",
			"imperative prose is expected in agent-config files (CLAUDE.md, etc.), so the markdown-comment primitive is suppressed there to avoid false positives",
		},
	},
	"pr_exfil_host_reference": {
		Type:              "pr_exfil_host_reference",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "Literal reference to an HTTP-capture-as-a-service host (webhook.site, requestbin, oast.*, …) in a file a pull request changes — a strong supply-chain exfiltration signal.",
		Caveats: []string{
			"literal substring match; obfuscated (XOR / base64 / runtime-concatenated) host strings evade it by design",
			"scoped to the PR's changed files at the head commit",
			"a hit is high-signal: published library code has no legitimate reason to POST to a public request collector",
		},
	},
	"pr_agent_config_touched": {
		Type:              "pr_agent_config_touched",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "A pull request modifies one or more AI-agent configuration files (CLAUDE.md, .cursorrules, .claude/, …) — the prompt-injection carrier surface that warrants extra scrutiny of the accompanying content-injection scan.",
		Caveats: []string{
			"path classification only (from the agent-config taxonomy); touching such a file is not itself malicious — legitimate maintenance looks identical",
			"raises severity when paired with a pr_content_injection hit in the same file",
			"scoped to the PR's changed files",
		},
	},
	"pr_ast_concern": {
		Type:              "pr_ast_concern",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "AST analysis of the source files a pull request changes spiked two or more rare-on-benign features (exec calls, persistence-path writes, dynamic eval, …) at the head commit — a weaponized-code signal.",
		Caveats: []string{
			"in-situ concern only (single checkout); the cross-version anomaly detector is not run here — a PR head has no version history to compare against",
			"conservative by design (threshold of two features): favors precision over recall, so single-feature payloads are missed",
			"scoped to the changed source files; covers Go / Python / JS-TS / Rust — other languages are not AST-scanned",
		},
	},
	"pr_risky_path_touched": {
		Type:              "pr_risky_path_touched",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "A pull request modifies one or more paths an organization declared sensitive (risky_paths in a shared pr-analyzer.yaml) — surfaced so a PR touching a dangerous code area is noticed even when its content is benign.",
		Caveats: []string{
			"org policy, opt-in: only emitted when pr-scan is run with --config naming a YAML that declares risky_paths; absent config means no opinion, not 'safe'",
			"path-prefix match only (P == F or P + \"/\" prefixes F, no wildcards), via pr-analyzer's shared codeshape.MatchesRiskyPath — touching the area is the signal, not the content",
			"scoped to the PR's changed files at the head commit; added / modified / removed all count as a touch",
		},
	},
	"pr_anomalous_language": {
		Type:              "pr_anomalous_language",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "A pull request introduces one or more programming languages outside the organization's preferred/allowed set (the languages weighting in a shared pr-analyzer.yaml) — surfaced so a PR that brings in a non-acceptable language is noticed.",
		Caveats: []string{
			"org policy, opt-in: only emitted when pr-scan is run with --config naming a YAML that declares languages.preferred or languages.allowed",
			"programming languages only — markup (Markdown, YAML, JSON, …) is excluded via pr-analyzer's shared codeshape classification, so docs/config-only PRs never trip it",
			"language detection is path/extension based (pr-analyzer's DetectLanguages); renaming a source file's extension evades it by design",
		},
	},
	"pr_dependency_manifest_touched": {
		Type:              "pr_dependency_manifest_touched",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryVeryHigh,
		Description:       "A pull request changes one or more dependency manifests or lockfiles (go.mod, package.json, Cargo.toml/lock, requirements.txt, Gemfile/lock, …) — a supply-chain touchpoint surfaced for review. Informational: it does not by itself raise the scan verdict.",
		Caveats: []string{
			"built-in catalog (shared with pr-analyzer's codeshape.TouchedManifests); not org-customizable and always evaluated",
			"informational by design — manifest changes are high-frequency (every dependency bump), so the signal flags the touchpoint without gating; what changed inside the manifest is a separate, deeper analysis",
			"basename match on the PR's changed files; a manifest under an unrecognized filename is not detected",
		},
	},
	"pr_defense_verdict": {
		Type:              "pr_defense_verdict",
		Group:             profile.SignalGroupHygiene,
		ForgeryResistance: profile.ForgeryHigh,
		Description:       "The overall pre-merge verdict for a pull-request scan — block / warn / clear — derived from the changelist findings, with the reasons and the head SHA scanned.",
		Caveats: []string{
			"a derived rollup of the other pr_* findings, not an independent observation",
			"clear means no configured detector fired on the changed files at the head SHA — not a guarantee of safety, since every detector has known evasions",
			"pinned to a head SHA; a force-push after the scan invalidates it",
		},
	},
	"pr_author": {
		Type:              "pr_author",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "The GitHub author of a scanned pull request — login, author_association, and the canonical identity URI of the author's user entity — recorded on the patch so a scan links to the identity: entity of the human who submitted it.",
		Caveats: []string{
			"emitted only for human authors; bot / GitHub-App authors (user.type == Bot, or a [bot] login) are not minted as identities and carry no pr_author signal",
			"author_association is GitHub's point-in-time classification of the author's relationship to THIS repo, not a global property of the identity",
			"the login is publisher-controlled and can change; the canonical identity URI is the stable join key for linking and for future contributor-burn cascades",
		},
	},
	"author_profile": {
		Type:              "author_profile",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "GitHub account profile of a PR author, recorded on their identity entity: account age, public-repo count, follower count, and account type. A freshly-created (throwaway) account submitting PRs is a strong supply-chain tell.",
		Caveats: []string{
			"recorded on the identity: entity (the per-user home), not the patch — it accumulates across every PR/repo we see from this account",
			"account metadata is publisher-controlled and mutable (followers, repo count drift); the created-at / account-age field is the load-bearing, hardest-to-fake part",
			"only fetched for human authors — bot / GitHub-App authors are never minted as identities",
		},
	},
	"pr_author_codeowner": {
		Type:              "pr_author_codeowner",
		Group:             profile.SignalGroupGovernance,
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Description:       "Whether a PR's author owns (per CODEOWNERS) the paths the PR changes — a maintainer editing their own area vs. an outsider touching a protected one.",
		Caveats: []string{
			"read from CODEOWNERS at the BASE commit, not the PR head — otherwise a PR that adds its own author to CODEOWNERS would read as an owner",
			"only direct @login ownership is detected; team (@org/team) and email owners are not resolved, so a real owner via team membership reads as a non-owner",
			"pattern matching covers the common CODEOWNERS forms (catch-all, dir, extension, exact) but not full gitignore glob semantics; absence of a CODEOWNERS file is not evidence either way",
		},
	},
}
