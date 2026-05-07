package htmlreport

import (
	"errors"
	"fmt"
	"io"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// AnalystPageInput is the payload for RenderAnalystPage. The page
// gives a side-by-side view of one analyst's worldview — every
// conclusion they emitted (linked when the synthesis cited it, plain
// when not), positive absences, observations, methodology trace.
type AnalystPageInput struct {
	// Output is the analyst output to render. Must be non-nil.
	Output *exchange.AnalystOutput

	// OutputID is the parent analyst output's UUID. Used to resolve
	// per-conclusion link plan entries (which key on output id).
	// Stored separately because exchange.AnalystOutput doesn't carry
	// its own row id.
	OutputID string

	// Plan resolves per-conclusion links. May be nil.
	Plan *LinkPlan

	// Page bundles RootPrefix, GeneratedAt, Version.
	Page PageContext
}

// RenderAnalystPage writes one analyst output's drill-down page. Pure;
// every dynamic string is escaped via html.EscapeString.
func RenderAnalystPage(w io.Writer, in AnalystPageInput) error {
	if in.Output == nil {
		return errors.New("RenderAnalystPage: nil Output")
	}
	o := in.Output
	prefix := in.Page.RootPrefix

	fmt.Fprintln(w, "<!DOCTYPE html>")
	fmt.Fprintln(w, `<html lang="en">`)
	fmt.Fprintln(w, "<head>")
	fmt.Fprintln(w, `<meta charset="utf-8">`)
	fmt.Fprintf(w, "<title>Analyst: %s</title>\n", esc(o.Attribution.AnalystID))
	fmt.Fprintf(w, `<link rel="stylesheet" href="%sassets/style.css">`+"\n", esc(prefix))
	fmt.Fprintln(w, "</head>")
	fmt.Fprintln(w, "<body>")

	fmt.Fprintf(w, "<h1>Analyst: %s</h1>\n", esc(o.Attribution.AnalystID))

	// Attribution block.
	fmt.Fprintln(w, `<dl class="metadata">`)
	writeDT(w, "Analyst", o.Attribution.AnalystID)
	if o.Attribution.Model != "" {
		writeDT(w, "Model", o.Attribution.Model)
	}
	if o.Attribution.PromptVersion != "" {
		writeDT(w, "Prompt version", o.Attribution.PromptVersion)
	}
	if o.Attribution.InvokedAt != "" {
		writeDT(w, "Invoked at", o.Attribution.InvokedAt)
	}
	if o.Attribution.Round > 0 {
		writeDT(w, "Round", fmt.Sprintf("round %d", o.Attribution.Round))
	}
	writeDT(w, "Target", o.Target)
	if o.TargetCommit != "" {
		writeDT(w, "Target commit", o.TargetCommit)
	}
	fmt.Fprintln(w, "</dl>")

	// Round notes.
	if o.RoundNotes != "" {
		fmt.Fprintln(w, `<section class="round-notes">`)
		fmt.Fprintln(w, "<h2>Round Notes</h2>")
		writeParagraphs(w, o.RoundNotes)
		fmt.Fprintln(w, "</section>")
	}

	// Conclusions list.
	if len(o.Conclusions) > 0 {
		fmt.Fprintln(w, `<section class="conclusions">`)
		fmt.Fprintln(w, "<h2>Conclusions</h2>")
		fmt.Fprintln(w, "<ul>")
		for _, c := range o.Conclusions {
			writeAnalystConclusionEntry(w, c, in.OutputID, in.Plan, prefix)
		}
		fmt.Fprintln(w, "</ul>")
		fmt.Fprintln(w, "</section>")
	}

	// Positive absences.
	if len(o.PositiveAbsences) > 0 {
		fmt.Fprintln(w, `<section class="positive-absences">`)
		fmt.Fprintln(w, "<h2>Positive Absences</h2>")
		for _, p := range o.PositiveAbsences {
			writePositiveAbsence(w, p)
		}
		fmt.Fprintln(w, "</section>")
	}

	// Observations.
	if len(o.Observations) > 0 {
		fmt.Fprintln(w, `<section class="observations">`)
		fmt.Fprintln(w, "<h2>Observations</h2>")
		for _, ob := range o.Observations {
			writeObservation(w, ob)
		}
		fmt.Fprintln(w, "</section>")
	}

	// Methodology trace.
	if o.MethodologyTrace != nil {
		fmt.Fprintln(w, `<section class="methodology-trace">`)
		fmt.Fprintln(w, "<h2>Methodology Trace</h2>")
		writeMethodologyTrace(w, o.MethodologyTrace)
		fmt.Fprintln(w, "</section>")
	}

	// Footer.
	fmt.Fprintln(w, `<footer class="report-footer">`)
	fmt.Fprintln(w, `<nav class="back-links">`)
	fmt.Fprintf(w, `<a href="%sindex.html">Back to synthesis</a>`+"\n", esc(prefix))
	fmt.Fprintln(w, "</nav>")
	fmt.Fprintf(w, "<p>Generated at %s by signatory %s.</p>\n",
		esc(in.Page.GeneratedAt), esc(in.Page.Version))
	fmt.Fprintln(w, "</footer>")

	fmt.Fprintln(w, "</body>")
	fmt.Fprintln(w, "</html>")
	return nil
}

// writeAnalystConclusionEntry emits one <li> per conclusion in the
// analyst page's full list. Cited-by-synthesis conclusions render as
// links; uncited ones list verdict + severity without a link.
func writeAnalystConclusionEntry(w io.Writer, c exchange.Conclusion, outputID string, plan *LinkPlan, prefix string) {
	fmt.Fprintf(w, `<li class="conclusion-entry %s">`, severityClass(c.Severity.Default))
	path := plan.ConclusionPagePath(outputID, c.ID)
	label := fmt.Sprintf("%s — %s", c.ID, c.Verdict)
	if path == "" {
		fmt.Fprintf(w, "<span>%s</span>", esc(label))
	} else {
		fmt.Fprintf(w, `<a href="%s%s">%s</a>`, esc(prefix), esc(path), esc(label))
	}
	fmt.Fprintf(w, ` <span class="severity-tag">[%s]</span>`,
		esc(string(c.Severity.Default)))
	fmt.Fprintln(w, "</li>")
}

// writePositiveAbsence emits one positive-absence block.
func writePositiveAbsence(w io.Writer, p exchange.PositiveAbsence) {
	fmt.Fprintln(w, `<div class="positive-absence">`)
	fmt.Fprintf(w, "<h3>%s</h3>\n", esc(p.PatternChecked))
	if p.Description != "" {
		fmt.Fprintf(w, "<p>%s</p>\n", esc(p.Description))
	}
	fmt.Fprintln(w, "<dl>")
	writeDT(w, "Confidence", string(p.Confidence))
	if p.PatternRef != nil && *p.PatternRef != "" {
		writeDT(w, "Pattern ref", *p.PatternRef)
	}
	fmt.Fprintln(w, "</dl>")
	if len(p.Citations) > 0 {
		fmt.Fprintln(w, "<ul>")
		for _, c := range p.Citations {
			writeCitation(w, c)
		}
		fmt.Fprintln(w, "</ul>")
	}
	fmt.Fprintln(w, "</div>")
}

// writeObservation emits one observation block. Body is multi-paragraph
// prose, split on blank lines.
func writeObservation(w io.Writer, o exchange.Observation) {
	fmt.Fprintln(w, `<div class="observation">`)
	fmt.Fprintf(w, "<h3>%s</h3>\n", esc(o.Title))
	fmt.Fprintln(w, "<dl>")
	writeDT(w, "Category", o.Category)
	if o.SignalType != nil && *o.SignalType != "" {
		writeDT(w, "Signal type", *o.SignalType)
	}
	fmt.Fprintln(w, "</dl>")
	writeParagraphs(w, o.Body)
	if len(o.Citations) > 0 {
		fmt.Fprintln(w, "<ul>")
		for _, c := range o.Citations {
			writeCitation(w, c)
		}
		fmt.Fprintln(w, "</ul>")
	}
	fmt.Fprintln(w, "</div>")
}

// writeMethodologyTrace emits the methodology catalog inline. Phase A
// renders every pattern with its collector hint axes and hit-on-target
// flag; if real outputs surface that this dwarfs the rest of the page,
// a future iteration moves it behind a flag (see design §5.4).
func writeMethodologyTrace(w io.Writer, m *exchange.MethodologyCatalog) {
	if m.Notes != "" {
		fmt.Fprintf(w, "<p>%s</p>\n", esc(m.Notes))
	}
	if len(m.Patterns) == 0 {
		return
	}
	fmt.Fprintln(w, "<ul>")
	for _, p := range m.Patterns {
		fmt.Fprint(w, `<li class="methodology-pattern">`)
		fmt.Fprintf(w, "<strong>%s</strong> (%s) — %s",
			esc(p.ID), esc(p.SignalGroup), esc(p.Description))
		// Collector-hint axes.
		fmt.Fprintf(w, ` <span class="collector-hint">[grep: %s; reasoning: %s`,
			esc(string(p.CollectorHint.GrepPrecision)),
			esc(string(p.CollectorHint.ReasoningDepth)))
		if p.CollectorHint.MissMode != "" {
			fmt.Fprintf(w, "; miss: %s", esc(string(p.CollectorHint.MissMode)))
		}
		fmt.Fprint(w, "]</span>")
		// Hit-on-target.
		if p.HitOnTarget != nil {
			if *p.HitOnTarget {
				fmt.Fprint(w, ` <span class="hit">hit</span>`)
			} else {
				fmt.Fprint(w, ` <span class="miss">miss</span>`)
			}
		}
		if p.FalsePositiveNotes != "" {
			fmt.Fprintf(w, "<br><em>fp notes:</em> %s", esc(p.FalsePositiveNotes))
		}
		fmt.Fprintln(w, "</li>")
	}
	fmt.Fprintln(w, "</ul>")
}
