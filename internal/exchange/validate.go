package exchange

import (
	"errors"
	"fmt"
)

// Validate checks structural invariants on an AnalystOutput and
// returns a joined error describing every problem it finds. Nil means
// the document is valid.
//
// Validation covers required fields, enum values, the Citation
// either-lines-or-scope invariant, and ID uniqueness within the
// document. It does NOT validate cross-document references (such as
// Finding.AnswersQuestion pointing to prompts in a separate handoff
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

	findingIDs := make(map[string]struct{}, len(o.Findings))
	for i, f := range o.Findings {
		path := fmt.Sprintf("findings[%d]", i)
		if f.ID == "" {
			errs = append(errs, fmt.Errorf("%s: id required", path))
		} else if _, dup := findingIDs[f.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate id %q", path, f.ID))
		} else {
			findingIDs[f.ID] = struct{}{}
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

	return errors.Join(errs...)
}

func (a *AgentAttribution) validate(path string) []error {
	var errs []error
	if a.AnalystID == "" {
		errs = append(errs, fmt.Errorf("%s: analyst_id required", path))
	}
	if a.Model == "" {
		errs = append(errs, fmt.Errorf("%s: model required", path))
	}
	if a.InvokedAt == "" {
		errs = append(errs, fmt.Errorf("%s: invoked_at required", path))
	}
	return errs
}

func (f *Finding) validate(path string) []error {
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
		errs = append(errs, fmt.Errorf("%s: severity.default %q invalid", path, f.Severity.Default))
	}
	for i, bc := range f.Severity.ByContext {
		if !bc.Value.Valid() {
			errs = append(errs, fmt.Errorf("%s.severity.by_context[%d]: value %q invalid", path, i, bc.Value))
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
			errs = append(errs, fmt.Errorf("%s.scope: kind %q invalid", path, c.Scope.Kind))
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
		errs = append(errs, fmt.Errorf("%s: confidence %q invalid", path, pa.Confidence))
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
		errs = append(errs, fmt.Errorf("%s.collector_hint.grep_precision %q invalid",
			path, mp.CollectorHint.GrepPrecision))
	}
	if !mp.CollectorHint.ReasoningDepth.Valid() {
		errs = append(errs, fmt.Errorf("%s.collector_hint.reasoning_depth %q invalid",
			path, mp.CollectorHint.ReasoningDepth))
	}
	if !mp.CollectorHint.MissMode.Valid() {
		errs = append(errs, fmt.Errorf("%s.collector_hint.miss_mode %q invalid",
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
		errs = append(errs, fmt.Errorf("%s: kind %q invalid", path, s.Kind))
	}
	return errs
}
