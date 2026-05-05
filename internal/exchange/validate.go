package exchange

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// SynthesistAnalystIDPrefix gates the SynthesisSupplement field.
// Only outputs whose attribution.analyst_id starts with this prefix
// may carry a supplement (and must carry one). Deliberately generous
// — matches "signatory-synthesis-v1", "signatory-synthesis-v2",
// "signatory-synthesis-experimental" etc. without the validator
// having to know the current active version. See
// design/m6-synthesis-contract.md §4.
//
// Exported because the M6b evidence assembler uses it to filter prior
// synthesis outputs out of the evidence rollup — single source of
// truth for the prefix across validator and assembler.
const SynthesistAnalystIDPrefix = "signatory-synthesis"

// IsSynthesistRole returns true when analystID identifies a synthesist
// role by prefix. Used by the validator's trust-boundary gate (Option C
// of m6-synthesis-contract) and by downstream consumers that need to
// distinguish synthesis outputs from analyst outputs without parsing
// the analyst_id themselves.
func IsSynthesistRole(analystID string) bool {
	return strings.HasPrefix(analystID, SynthesistAnalystIDPrefix)
}

// signatoryNamespacePrefix is the namespace owned by signatory's own
// analyst pipeline. analyst_ids in this namespace must match the
// canonical form (canonicalSignatoryAnalystIDRe); arbitrary suffixes
// or omitted version tags drift the store and break the
// orchestrator's exact-string verify check.
//
// External analysts (different teams, different conventions) use
// their own namespaces — `external-sec-v1`, `external-prov-v1`, etc.
// The validator does not gate those.
const signatoryNamespacePrefix = "signatory-"

// canonicalSignatoryAnalystIDRe matches the only acceptable shapes
// for analyst_ids inside the signatory- namespace:
//
//	^signatory-(security|provenance|synthesis)-v\d+$
//
// Examples that pass: signatory-security-v1, signatory-provenance-v2,
// signatory-synthesis-v10.
//
// Examples that fail: signatory-provenance (no -v1 suffix — the
// dominant drift form, 17 of 30 occurrences in the dogfood store
// pre-fix), signatory-osv-supplement-v1 (unknown role — stray
// historical ingest), signatory-security-vbeta (non-numeric version),
// signatory-security-v1-extra (trailing junk).
//
// Why the strictness: the orchestrator's pipeline_run.go declares
// expected analyst_ids verbatim ("signatory-security-v1" etc.) and
// the verify step matches by exact string equality. Any drift
// silently misses the verify check, the row exists in the store
// but is invisible to ListOutputsForSession's session filter, and
// the orchestrator loops on missing_analysts. Catching at the
// validator gives the agent a fast CodeSchemaViolation it can
// self-correct from per the handoff's "fix and resubmit"
// instruction — turning a multi-minute re-dispatch loop into a
// single-turn correction.
var canonicalSignatoryAnalystIDRe = regexp.MustCompile(
	`^signatory-(security|provenance|synthesis)-v\d+$`)

// Validate checks structural invariants on an AnalystOutput and
// returns a joined error describing every problem it finds. Nil means
// the document is valid.
//
// Validation covers required fields, enum values, the Citation
// either-lines-or-scope invariant, and ID uniqueness within the
// document. It does NOT validate cross-document references (such as
// Conclusion.AnswersQuestion pointing to prompts in a separate handoff
// document) — those are free-form strings by design.
func (o *AnalystOutput) Validate() error {
	if o == nil {
		return errors.New("nil AnalystOutput")
	}

	var errs []error

	errs = append(errs, o.Attribution.validate("attribution")...)

	if o.Target == "" {
		errs = append(errs, errors.New("target required"))
	}

	conclusionIDs := make(map[string]struct{}, len(o.Conclusions))
	for i, f := range o.Conclusions {
		path := fmt.Sprintf("conclusions[%d]", i)
		if f.ID == "" {
			errs = append(errs, fmt.Errorf("%s: id required", path))
		} else if _, dup := conclusionIDs[f.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate id %q", path, f.ID))
		} else {
			conclusionIDs[f.ID] = struct{}{}
		}
		errs = append(errs, f.validate(path)...)
	}

	observationIDs := make(map[string]struct{}, len(o.Observations))
	for i, obs := range o.Observations {
		path := fmt.Sprintf("observations[%d]", i)
		if obs.ID == "" {
			errs = append(errs, fmt.Errorf("%s: id required", path))
		} else if _, dup := observationIDs[obs.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate id %q", path, obs.ID))
		} else {
			observationIDs[obs.ID] = struct{}{}
		}
		errs = append(errs, obs.validate(path)...)
	}

	for i, pa := range o.PositiveAbsences {
		errs = append(errs, pa.validate(fmt.Sprintf("positive_absences[%d]", i))...)
	}

	if o.MethodologyTrace != nil {
		errs = append(errs, o.MethodologyTrace.validate("methodology_trace")...)
	}

	for i, s := range o.Supersedes {
		errs = append(errs, s.validate(fmt.Sprintf("supersedes[%d]", i))...)
	}

	// Trust-boundary guard: SynthesisSupplement presence is gated to
	// the synthesist role in both directions. Non-synthesist roles
	// may not carry a supplement (that would be Layer-2 proposing a
	// Layer-3 decision); synthesist roles must carry one (a synthesis
	// without a supplement is an empty row). See
	// design/m6-synthesis-contract.md §4.
	switch {
	case o.SynthesisSupplement != nil && !IsSynthesistRole(o.Attribution.AnalystID):
		errs = append(errs, fmt.Errorf(
			"synthesis_supplement only allowed for synthesist role; got analyst_id %q",
			o.Attribution.AnalystID))
	case o.SynthesisSupplement == nil && IsSynthesistRole(o.Attribution.AnalystID):
		errs = append(errs, errors.New("synthesist output requires synthesis_supplement"))
	case o.SynthesisSupplement != nil:
		errs = append(errs, o.SynthesisSupplement.validate("synthesis_supplement")...)
	}

	return errors.Join(errs...)
}

// validate checks required-field rules on the supplement. Only called
// from AnalystOutput.Validate after the role-gating switch has confirmed
// the supplement is allowed here.
func (s *SynthesisSupplement) validate(path string) []error {
	var errs []error
	errs = append(errs, s.ProposedPosture.validate(path+".proposed_posture")...)
	if s.Reasoning == "" {
		errs = append(errs, fmt.Errorf("%s.reasoning required", path))
	}
	if s.Summary == "" {
		errs = append(errs, fmt.Errorf("%s.summary required", path))
	}
	return errs
}

// validate checks required-field rules on the proposed posture.
// Called from SynthesisSupplement.validate; path carries the caller's
// JSON-path context so errors say "synthesis_supplement.proposed_posture.tier"
// rather than bare "tier".
func (p *ProposedPosture) validate(path string) []error {
	var errs []error
	if !ValidProposedPostureTier(p.Tier) {
		errs = append(errs, fmt.Errorf("%s.tier %q invalid; valid values: vetted-frozen, trusted-for-now, unexamined, unknown-provenance, rejected", path, p.Tier))
	}
	if p.RationaleSummary == "" {
		errs = append(errs, fmt.Errorf("%s.rationale_summary required", path))
	}
	if err := ValidateVersionScopeShape(p.VersionScope); err != nil {
		errs = append(errs, fmt.Errorf("%s.version_scope: %w", path, err))
	}
	return errs
}

// Maximum byte length of a version_scope string. Generous — the
// largest legitimate shapes we've observed across Go / npm / PyPI
// sit comfortably under 50 bytes (Go pseudo-versions like
// "v0.0.0-20230101000000-abcdefabcdef" are 40 bytes; calendar-
// versioned PyPI releases are ~12 bytes). 128 gives ~3× headroom
// for ecosystems we haven't met yet. A value larger than this is
// almost certainly prose or a pasted URI, not a version identifier.
const maxVersionScopeBytes = 128

// canonicalURISchemes are the prefixes a well-formed version MUST
// NOT start with. They name RELATIONSHIPS between entities, not
// versions of an entity — the two roles are orthogonal and the
// "copy-paste the whole URI into version_scope" mistake conflates
// them. The list mirrors internal/profile/uri.go's validURISchemes.
// Kept as a standalone slice here to avoid importing internal/profile
// (which would create a cycle: profile already imports exchange
// types in some collector paths).
var canonicalURISchemes = []string{
	"pkg:", "repo:", "identity:", "org:", "patch:",
}

// ValidateVersionScopeShape checks that a posture's version_scope
// has the shape of a version identifier, not something else. It
// does NOT try to enforce a full version grammar — ecosystems vary
// (semver, Go pseudo-versions, calendar versioning, git tags) and
// a strict regex would reject legitimate inputs. Instead, it
// rejects the specific NON-VERSION shapes we've seen confuse
// upstream producers:
//
//   - Canonical URI strings (e.g., "pkg:npm/X@1.2.3") — the whole
//     URI was pasted where only the version belongs. Conflates the
//     "which entity" question with the "which version" question.
//   - URL-shaped strings (e.g., "https://…/v1.2.3") — similar
//     confusion, often from a release-page URL.
//   - Multi-line strings — versions are always single-line.
//   - Over-length strings — the cap is generous but catches the
//     "I pasted the whole rationale" class of mistake.
//
// Empty is valid: a posture can be unversioned ("applies to the
// entity as a whole"). Returns nil for empty input.
//
// Exported for symmetric use by cmd/signatory's PostureSetCmd —
// the same check covers the manual `--version` path and the
// synthesis-ingest path, so malformed versions are rejected
// regardless of which door they came in through.
func ValidateVersionScopeShape(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > maxVersionScopeBytes {
		return fmt.Errorf("exceeds %d-byte cap (got %d bytes); this looks like prose or a pasted URL, not a version identifier",
			maxVersionScopeBytes, len(v))
	}
	if strings.ContainsAny(v, "\n\r") {
		return fmt.Errorf("must be single-line; got embedded newline")
	}
	if strings.Contains(v, "://") {
		return fmt.Errorf("looks like a URL (%q contains \"://\"); pass only the version identifier, not a URL", v)
	}
	for _, scheme := range canonicalURISchemes {
		if strings.HasPrefix(v, scheme) {
			return fmt.Errorf("looks like a canonical URI starting with %q; pass only the version part (e.g., \"v1.2.3\"), not the full URI", scheme)
		}
	}
	return nil
}

func (a *AgentAttribution) validate(path string) []error {
	var errs []error
	if a.AnalystID == "" {
		errs = append(errs, fmt.Errorf("%s: analyst_id required", path))
	}
	// Signatory namespace gate: ids that start with `signatory-` must
	// match the canonical form. Other namespaces are unrestricted.
	// See canonicalSignatoryAnalystIDRe for rationale.
	if a.AnalystID != "" &&
		strings.HasPrefix(a.AnalystID, signatoryNamespacePrefix) &&
		!canonicalSignatoryAnalystIDRe.MatchString(a.AnalystID) {
		errs = append(errs, fmt.Errorf(
			"%s: analyst_id %q is in the signatory- namespace but does not match the "+
				"canonical form `signatory-(security|provenance|synthesis)-v<N>`. "+
				"Common drift to avoid: dropping the -v<N> suffix, omitting the "+
				"signatory- prefix, substituting -analyst for -v<N>. Use the "+
				"analyst_id given at the top of your dispatch prompt verbatim.",
			path, a.AnalystID))
	}
	if a.Model == "" {
		errs = append(errs, fmt.Errorf("%s: model required", path))
	}
	if a.InvokedAt == "" {
		errs = append(errs, fmt.Errorf("%s: invoked_at required", path))
	}
	return errs
}

func (f *Conclusion) validate(path string) []error {
	var errs []error
	if f.Verdict == "" {
		errs = append(errs, fmt.Errorf("%s: verdict required", path))
	}
	if f.Rationale == "" {
		errs = append(errs, fmt.Errorf("%s: rationale required", path))
	}
	if f.Category == "" {
		errs = append(errs, fmt.Errorf("%s: category required", path))
	}
	if !f.Severity.Default.Valid() {
		errs = append(errs, fmt.Errorf("%s: severity.default %q invalid; valid values: critical, high, medium, low, informational, positive", path, f.Severity.Default))
	}
	for i, bc := range f.Severity.ByContext {
		if !bc.Value.Valid() {
			errs = append(errs, fmt.Errorf("%s.severity.by_context[%d]: value %q invalid; valid values: critical, high, medium, low, informational, positive", path, i, bc.Value))
		}
	}
	for i, c := range f.Citations {
		errs = append(errs, c.validate(fmt.Sprintf("%s.citations[%d]", path, i))...)
	}
	for i, s := range f.Supersedes {
		errs = append(errs, s.validate(fmt.Sprintf("%s.supersedes[%d]", path, i))...)
	}
	return errs
}

func (c *Citation) validate(path string) []error {
	var errs []error

	hasLines := c.LineStart != nil
	hasScope := c.Scope != nil

	switch {
	case !hasLines && !hasScope:
		errs = append(errs, fmt.Errorf("%s: must have either line_start or scope", path))
	case hasLines && hasScope:
		errs = append(errs, fmt.Errorf("%s: cannot have both line_start and scope", path))
	case hasLines:
		if c.Path == "" {
			errs = append(errs, fmt.Errorf("%s: path required for line-based citation", path))
		}
		if c.LineEnd != nil && *c.LineEnd < *c.LineStart {
			errs = append(errs, fmt.Errorf("%s: line_end (%d) < line_start (%d)", path, *c.LineEnd, *c.LineStart))
		}
		if *c.LineStart < 1 {
			errs = append(errs, fmt.Errorf("%s: line_start must be >= 1", path))
		}
	case hasScope:
		if !ValidScopeKind(c.Scope.Kind) {
			errs = append(errs, fmt.Errorf("%s.scope: kind %q invalid; valid values: file, dir, tree, workspace, crate", path, c.Scope.Kind))
		}
		if c.Scope.Path == "" {
			errs = append(errs, fmt.Errorf("%s.scope: path required", path))
		}
	}

	return errs
}

func (pa *PositiveAbsence) validate(path string) []error {
	var errs []error
	if pa.PatternChecked == "" {
		errs = append(errs, fmt.Errorf("%s: pattern_checked required", path))
	}
	if pa.Description == "" {
		errs = append(errs, fmt.Errorf("%s: description required", path))
	}
	if !pa.Confidence.Valid() {
		errs = append(errs, fmt.Errorf("%s: confidence %q invalid; valid values: exhaustive, thoroughly_reviewed, spot_checked", path, pa.Confidence))
	}
	for i, c := range pa.Citations {
		errs = append(errs, c.validate(fmt.Sprintf("%s.citations[%d]", path, i))...)
	}
	return errs
}

func (obs *Observation) validate(path string) []error {
	var errs []error
	if obs.Title == "" {
		errs = append(errs, fmt.Errorf("%s: title required", path))
	}
	if obs.Body == "" {
		errs = append(errs, fmt.Errorf("%s: body required", path))
	}
	if obs.Category == "" {
		errs = append(errs, fmt.Errorf("%s: category required", path))
	}
	for i, c := range obs.Citations {
		errs = append(errs, c.validate(fmt.Sprintf("%s.citations[%d]", path, i))...)
	}
	return errs
}

func (mc *MethodologyCatalog) validate(path string) []error {
	var errs []error
	errs = append(errs, mc.Source.validate(path+".source")...)

	patternIDs := make(map[string]struct{}, len(mc.Patterns))
	for i, p := range mc.Patterns {
		pPath := fmt.Sprintf("%s.patterns[%d]", path, i)
		if p.ID == "" {
			errs = append(errs, fmt.Errorf("%s: id required", pPath))
		} else if _, dup := patternIDs[p.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate id %q", pPath, p.ID))
		} else {
			patternIDs[p.ID] = struct{}{}
		}
		errs = append(errs, p.validate(pPath)...)
	}

	// ComposesWith must reference patterns in the same catalog.
	// Checked in a second pass so it sees the full ID set.
	for i, p := range mc.Patterns {
		for j, ref := range p.ComposesWith {
			if _, ok := patternIDs[ref]; !ok {
				errs = append(errs, fmt.Errorf("%s.patterns[%d].composes_with[%d]: %q refers to unknown pattern",
					path, i, j, ref))
			}
		}
	}

	return errs
}

func (mp *MethodologyPattern) validate(path string) []error {
	var errs []error
	if mp.SignalGroup == "" {
		errs = append(errs, fmt.Errorf("%s: signal_group required", path))
	}
	if mp.Description == "" {
		errs = append(errs, fmt.Errorf("%s: description required", path))
	}
	if !mp.CollectorHint.GrepPrecision.Valid() {
		errs = append(errs, fmt.Errorf("%s.collector_hint.grep_precision %q invalid; valid values: high, narrows, useless",
			path, mp.CollectorHint.GrepPrecision))
	}
	if !mp.CollectorHint.ReasoningDepth.Valid() {
		errs = append(errs, fmt.Errorf("%s.collector_hint.reasoning_depth %q invalid; valid values: none, one_hop, multi_hop",
			path, mp.CollectorHint.ReasoningDepth))
	}
	if !mp.CollectorHint.MissMode.Valid() {
		errs = append(errs, fmt.Errorf("%s.collector_hint.miss_mode %q invalid; valid values: balanced, false_positive_heavy, false_negative_heavy",
			path, mp.CollectorHint.MissMode))
	}
	// A pattern with no grep and high precision makes no sense.
	if mp.Pattern == nil && mp.CollectorHint.GrepPrecision == GrepPrecisionHigh {
		errs = append(errs, fmt.Errorf("%s: no pattern string but grep_precision=high (should be useless or narrows)", path))
	}
	return errs
}

func (s *Supersession) validate(path string) []error {
	var errs []error
	if s.PriorID == "" {
		errs = append(errs, fmt.Errorf("%s: prior_id required", path))
	}
	if !s.Kind.Valid() {
		errs = append(errs, fmt.Errorf("%s: kind %q invalid; valid values: corrects, refines, deprecates", path, s.Kind))
	}
	return errs
}
