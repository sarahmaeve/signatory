// Package exchange: structured-text parser for agent output.
//
// Agents produce structured markdown (their strongest modality); this
// parser converts it to an AnalystOutput. The structured format is
// documented in design/agent-output-contract.md.
//
// The parser is deliberately lenient on whitespace and ordering but
// strict on required fields: every Conclusion must have Severity,
// Category, and Verdict. Missing required fields produce a clear
// error that names the conclusion and the missing field — the agent
// or orchestrator can fix the text and retry.
//
// Citation parsing handles these forms:
//
//	Citation: path/to/file.py:47-52 "optional quoted text"
//	Citation: path/to/file.py:47 "quoted"
//	Citation: path/to/file.py:47
//	Citation: path/to/file.py       (scope-based: whole file)
package exchange

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ParseStructuredOutput reads structured markdown from r and produces
// an AnalystOutput. The target field is set from the provided argument
// (not parsed from the text) to avoid the agent having to know the
// canonical URI format.
//
// Returns a detailed error on parse failure naming the line, section,
// and what was expected. Structural validation (required fields on
// each Conclusion, valid severity values) happens here — the output
// should pass format-check without further fixup.
func ParseStructuredOutput(r io.Reader, target string) (*AnalystOutput, error) {
	p := &structuredParser{
		target:      target,
		sectionType: "header", // Start in header mode so pre-H1 fields are captured.
		out: &AnalystOutput{
			Target: target,
			// Attribution.InvokedAt is NOT pre-stamped here — the store
			// layer fills it from time.Now() at ingest. Pre-stamping
			// would set a non-empty value, which the v1 schema validator
			// now rejects (model and invoked_at are server-stamped; see
			// AgentAttribution.validate).
			Attribution: AgentAttribution{},
		},
	}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		p.lineNum++
		line := scanner.Text()
		if err := p.processLine(line); err != nil {
			return nil, fmt.Errorf("line %d: %w", p.lineNum, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	// Flush the last section.
	if err := p.flushSection(); err != nil {
		return nil, err
	}
	// Set conclusions to empty slice (not nil) for JSON serialization.
	if p.out.Conclusions == nil {
		p.out.Conclusions = []Conclusion{}
	}
	return p.out, nil
}

// structuredParser holds state across lines.
type structuredParser struct {
	target  string
	out     *AnalystOutput
	lineNum int

	// Current section being accumulated. Starts as "header" so
	// attribution fields before any H1/H2 heading are captured —
	// agents naturally start with Analyst:/Model:/Round: lines
	// without an explicit # Analysis: preamble.
	sectionType string // "header", "conclusion", "absence", "observation", "round-notes"
	sectionID   string
	fields      map[string]string
	bodyKey     string   // which field accumulates multi-line text ("rationale", "body", etc.)
	bodyLines   []string // accumulated body text
	citations   []string // raw citation lines
}

// processLine routes a line to the appropriate handler based on
// heading markers.
func (p *structuredParser) processLine(line string) error {
	trimmed := strings.TrimSpace(line)

	// Top-level H1: # Analysis: ...
	if strings.HasPrefix(trimmed, "# ") && p.sectionType == "" {
		p.sectionType = "header"
		return nil
	}

	// H2 section boundaries.
	if strings.HasPrefix(trimmed, "## ") {
		if err := p.flushSection(); err != nil {
			return err
		}
		heading := strings.TrimPrefix(trimmed, "## ")
		return p.startSection(heading)
	}

	// Inside a section: accumulate field lines or body text.
	return p.accumulateLine(trimmed, line)
}

var conclusionHeadingRe = regexp.MustCompile(`(?i)^Conclusion:\s*(.+)$`)
var absenceHeadingRe = regexp.MustCompile(`(?i)^Absence:\s*(.+)$`)
var observationHeadingRe = regexp.MustCompile(`(?i)^Observation:\s*(.+)$`)
var roundNotesHeadingRe = regexp.MustCompile(`(?i)^Round[ -]?[Nn]otes$`)

func (p *structuredParser) startSection(heading string) error {
	if m := conclusionHeadingRe.FindStringSubmatch(heading); m != nil {
		p.sectionType = "conclusion"
		p.sectionID = strings.TrimSpace(m[1])
		p.fields = map[string]string{}
		p.bodyKey = "rationale"
		p.bodyLines = nil
		p.citations = nil
		return nil
	}
	if m := absenceHeadingRe.FindStringSubmatch(heading); m != nil {
		p.sectionType = "absence"
		p.sectionID = strings.TrimSpace(m[1])
		p.fields = map[string]string{}
		p.bodyKey = "rationale"
		p.bodyLines = nil
		p.citations = nil
		return nil
	}
	if m := observationHeadingRe.FindStringSubmatch(heading); m != nil {
		p.sectionType = "observation"
		p.sectionID = strings.TrimSpace(m[1])
		p.fields = map[string]string{}
		p.bodyKey = "body"
		p.bodyLines = nil
		p.citations = nil
		return nil
	}
	if roundNotesHeadingRe.MatchString(heading) {
		p.sectionType = "round-notes"
		p.bodyLines = nil
		return nil
	}
	// Unknown heading — treat as body text if we're inside a section,
	// or silently skip if between sections.
	return nil
}

// Field-line patterns: "Key: value" at the start of a section.
var fieldLineRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_-]+):\s+(.+)$`)
var citationLineRe = regexp.MustCompile(`(?i)^Citation:\s+(.+)$`)

func (p *structuredParser) accumulateLine(trimmed, raw string) error {
	switch p.sectionType {
	case "header":
		// Parse header-level key: value fields.
		if m := fieldLineRe.FindStringSubmatch(trimmed); m != nil {
			key := strings.ToLower(m[1])
			val := m[2]
			switch key {
			case "target":
				// Prefer the CLI-provided target; ignore if set.
				if p.target == "" {
					p.out.Target = val
				}
			case "analyst", "analyst-id", "analyst_id":
				p.out.Attribution.AnalystID = val
			case "model", "invoked-at", "invoked_at":
				// Silently dropped: model and invoked_at are
				// server-stamped (by OTEL backfill / store ingest
				// respectively) and the v1 schema validator rejects
				// caller-supplied values. We still recognize the
				// header keys here for forward-compat with
				// agent-emitted markdown that hasn't been updated to
				// drop them — accepting and discarding is friendlier
				// than failing the parse on what's now decorative.
			case "prompt-version", "prompt_version":
				p.out.Attribution.PromptVersion = val
			case "round":
				if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
					p.out.Attribution.Round = n
				}
			case "target-commit", "target_commit":
				p.out.TargetCommit = val
			}
		}

	case "conclusion", "absence", "observation":
		// Citation lines are special.
		if m := citationLineRe.FindStringSubmatch(trimmed); m != nil {
			p.citations = append(p.citations, m[1])
			return nil
		}
		// Field lines (before body text starts).
		if len(p.bodyLines) == 0 {
			if m := fieldLineRe.FindStringSubmatch(trimmed); m != nil {
				p.fields[strings.ToLower(m[1])] = m[2]
				return nil
			}
		}
		// Body text (everything after the last field line).
		// Empty lines are preserved in body.
		if trimmed != "" || len(p.bodyLines) > 0 {
			p.bodyLines = append(p.bodyLines, raw)
		}

	case "round-notes":
		p.bodyLines = append(p.bodyLines, raw)
	}
	return nil
}

func (p *structuredParser) flushSection() error {
	switch p.sectionType {
	case "conclusion":
		return p.flushConclusion()
	case "absence":
		return p.flushAbsence()
	case "observation":
		return p.flushObservation()
	case "round-notes":
		p.out.RoundNotes = strings.TrimSpace(strings.Join(p.bodyLines, "\n"))
	}
	p.sectionType = ""
	p.fields = nil
	p.bodyLines = nil
	p.citations = nil
	return nil
}

// sectionIsEmpty returns true when the current section has no
// accumulated content: no field lines, no body text, no citations.
// This happens when an agent emits an ID-only heading followed
// immediately by the real heading, e.g.:
//
//	## Absence: A001
//	## Absence: network-outbound connections or telemetry
//	Confidence: exhaustive
//	...
//
// The first heading creates a section with sectionID="A001" but no
// content. Rather than failing validation on the empty record, the
// flush functions discard it — the real content lands in the next
// section where it belongs.
func (p *structuredParser) sectionIsEmpty() bool {
	return len(p.fields) == 0 && len(p.bodyLines) == 0 && len(p.citations) == 0
}

func (p *structuredParser) flushConclusion() error {
	if p.sectionIsEmpty() {
		return nil // ID-only heading; real content follows.
	}
	id := p.sectionID
	verdict := p.fields["verdict"]
	if verdict == "" {
		return fmt.Errorf("conclusion %s: missing required field Verdict", id)
	}
	sev := p.fields["severity"]
	if sev == "" {
		return fmt.Errorf("conclusion %s: missing required field Severity", id)
	}
	if !SeverityValue(sev).Valid() {
		return fmt.Errorf("conclusion %s: invalid severity %q (want critical/high/medium/low/informational/positive)", id, sev)
	}
	cat := p.fields["category"]
	if cat == "" {
		return fmt.Errorf("conclusion %s: missing required field Category", id)
	}

	c := Conclusion{
		ID:       id,
		Verdict:  verdict,
		Severity: Severity{Default: SeverityValue(sev)},
		Category: cat,
	}

	// Optional fields.
	if v := p.fields["design-intent"]; strings.EqualFold(v, "true") {
		c.DesignIntent = true
	}
	if v := p.fields["signal-type"]; v != "" {
		c.SignalType = &v
	}
	if v := p.fields["signal_type"]; v != "" {
		c.SignalType = &v
	}

	// Rationale from body text.
	c.Rationale = strings.TrimSpace(strings.Join(p.bodyLines, "\n"))

	// Citations.
	for _, raw := range p.citations {
		cit, err := parseCitation(raw)
		if err != nil {
			return fmt.Errorf("conclusion %s: %w", id, err)
		}
		c.Citations = append(c.Citations, cit)
	}

	p.out.Conclusions = append(p.out.Conclusions, c)
	return nil
}

func (p *structuredParser) flushAbsence() error {
	if p.sectionIsEmpty() {
		return nil // ID-only heading; real content follows.
	}
	pattern := p.sectionID
	conf := p.fields["confidence"]
	if conf == "" {
		conf = "spot_checked" // safe default
	}
	if !Confidence(conf).Valid() {
		return fmt.Errorf("absence %q: invalid confidence %q", pattern, conf)
	}
	desc := strings.TrimSpace(strings.Join(p.bodyLines, "\n"))
	if v := p.fields["rationale"]; v != "" && desc == "" {
		desc = v
	}

	a := PositiveAbsence{
		PatternChecked: pattern,
		Description:    desc,
		Confidence:     Confidence(conf),
	}
	for _, raw := range p.citations {
		cit, err := parseCitation(raw)
		if err != nil {
			return fmt.Errorf("absence %q: %w", pattern, err)
		}
		a.Citations = append(a.Citations, cit)
	}
	p.out.PositiveAbsences = append(p.out.PositiveAbsences, a)
	return nil
}

func (p *structuredParser) flushObservation() error {
	if p.sectionIsEmpty() {
		return nil // ID-only heading; real content follows.
	}
	id := p.sectionID
	title := p.fields["title"]
	if title == "" {
		title = id // Use the heading text as the title fallback.
	}
	cat := p.fields["category"]
	if cat == "" {
		cat = "general"
	}
	body := strings.TrimSpace(strings.Join(p.bodyLines, "\n"))

	o := Observation{
		ID:       id,
		Title:    title,
		Body:     body,
		Category: cat,
	}
	if v := p.fields["signal-type"]; v != "" {
		o.SignalType = &v
	}
	if v := p.fields["signal_type"]; v != "" {
		o.SignalType = &v
	}
	for _, raw := range p.citations {
		cit, err := parseCitation(raw)
		if err != nil {
			return fmt.Errorf("observation %s: %w", id, err)
		}
		o.Citations = append(o.Citations, cit)
	}
	p.out.Observations = append(p.out.Observations, o)
	return nil
}

// parseCitation parses citation forms:
//
//	path/to/file.py:47-52 "quoted text"
//	path/to/file.py:47 "quoted"
//	path/to/file.py:47
//	path/to/file.py          (whole-file scope)
var citationRe = regexp.MustCompile(
	`^(\S+?)(?::(\d+)(?:-(\d+))?)?` + // path:start-end
		`(?:\s+"([^"]*)")?$`) // optional "quoted"

func parseCitation(raw string) (Citation, error) {
	raw = strings.TrimSpace(raw)
	m := citationRe.FindStringSubmatch(raw)
	if m == nil {
		// Can't parse — treat as a scope-based citation on the whole string.
		return Citation{
			Scope: &ScopeRef{Kind: "file", Path: raw},
		}, nil
	}

	path := m[1]
	startStr := m[2]
	endStr := m[3]
	quoted := m[4]

	c := Citation{Path: path}

	if startStr != "" {
		n, _ := strconv.Atoi(startStr)
		c.LineStart = &n
		if endStr != "" {
			e, _ := strconv.Atoi(endStr)
			c.LineEnd = &e
		}
	} else {
		// No line number — scope-based citation (whole file).
		c.Scope = &ScopeRef{Kind: "file", Path: path}
		c.Path = "" // Scope takes precedence; clear path to avoid validator complaint.
	}

	if quoted != "" {
		c.Quoted = &quoted
	}

	return c, nil
}
