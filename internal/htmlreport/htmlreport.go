// Package htmlreport renders a synthesis output and its referenced
// analyst findings as a static HTML site.
//
// Phase A scope (per design/potential/expanded-reporting.md §0):
// the synthesis index page only, with a LinkPlan describing where
// referenced findings *will* live once subsequent phases land their
// renderers. Conclusion pages, analyst pages, the directory writer,
// and the CLI flag wiring are deliberately not yet present.
//
// Dependencies: stdlib only. No Markdown rendering, no template
// engine. Renderers are fmt.Fprintf-style and call html.EscapeString
// on every string sourced from a synthesis or analyst output.
//
// Determinism: renderers take generated-at and version stamps as
// parameters. No time.Now() inside; no build-info lookup inside.
// The CLI wires live values at the call site.
package htmlreport

// ConclusionKey identifies a single conclusion across all linked
// analyst outputs. The pair is unique: a bare local id like "F001"
// can collide between security and provenance, so the OutputID
// qualifier is mandatory for cross-page linking.
type ConclusionKey struct {
	OutputID string
	LocalID  string
}

// LinkPlan maps every referenced finding to the relative path of the
// page that *will* render it. Phase A produces the plan and uses it
// to emit anchors from the synthesis index; later phases consume the
// same plan to write the conclusion / analyst pages themselves.
//
// Page paths are relative to the report root (the auto-named
// subdirectory described in §4 of the design doc), e.g.
// "conclusions/abc12345-F001.html".
type LinkPlan struct {
	// ConclusionPages maps a (output-id, local-id) pair to the
	// relative path of its rendered conclusion page. A KeyConclusionRef
	// whose target is missing from the loaded analyst outputs does
	// NOT appear here; it appears in Dangling instead.
	ConclusionPages map[ConclusionKey]string

	// AnalystPages maps an analyst-id (e.g. "signatory-security-v1")
	// to the relative path of its per-analyst page.
	AnalystPages map[string]string

	// AnalystToOutput maps an analyst-id to the output-id whose
	// conclusions live under it. Populated for every linked analyst
	// output. Used to resolve bare local ids in concordance and
	// contradiction entries (which carry analyst refs but no output
	// refs) back to specific conclusion pages.
	AnalystToOutput map[string]string

	// Dangling lists references in the synthesis whose target could
	// not be resolved against any loaded analyst output. Phase A
	// records dangling KeyConclusionRefs only; concordance and
	// contradiction misses render as escaped plain text without an
	// entry here (per design §0).
	Dangling []DanglingRef
}

// DanglingRef captures one unresolvable reference. Reason is a short
// human-readable code ("output not loaded", "local id absent in
// output") used both in the renderer's stub-page banner and in the
// LinkPlan unit tests' assertions.
type DanglingRef struct {
	OutputID string
	LocalID  string
	Reason   string
}

// ConclusionPagePath returns the relative page path for a (output,
// local) pair, or empty string if the pair has no rendered page.
// Convenience helper so renderers don't have to construct the
// ConclusionKey inline at every call site.
func (p *LinkPlan) ConclusionPagePath(outputID, localID string) string {
	if p == nil || p.ConclusionPages == nil {
		return ""
	}
	return p.ConclusionPages[ConclusionKey{OutputID: outputID, LocalID: localID}]
}

// AnalystPagePath returns the relative page path for an analyst id,
// or empty string if no page is planned for that analyst.
func (p *LinkPlan) AnalystPagePath(analystID string) string {
	if p == nil || p.AnalystPages == nil {
		return ""
	}
	return p.AnalystPages[analystID]
}

// ResolveConcordanceID tries to map a bare local id (e.g. "F001")
// from a concordance or contradiction entry to a conclusion page
// path, by walking the entry's analyst refs to find an owning
// output. Returns empty string on no match — callers render the id
// as escaped plain text in that case (see design §0).
func (p *LinkPlan) ResolveConcordanceID(analystIDs []string, localID string) string {
	if p == nil {
		return ""
	}
	for _, aid := range analystIDs {
		outputID, ok := p.AnalystToOutput[aid]
		if !ok {
			continue
		}
		if path := p.ConclusionPagePath(outputID, localID); path != "" {
			return path
		}
	}
	return ""
}
