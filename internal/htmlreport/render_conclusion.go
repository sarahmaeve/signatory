package htmlreport

import (
	"errors"
	"fmt"
	"io"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// PageContext bundles parameters that every non-index renderer needs.
//
// RootPrefix is what to prepend to a root-relative LinkPlan path so
// the URL resolves from the page's directory. For pages at the
// report root (index.html) it's empty; for pages at one level deep
// (`conclusions/foo.html`, `analysts/bar.html`) it's "../". The
// directory writer (forthcoming) computes it once per page-class.
//
// GeneratedAt and Version flow into the footer. Renderers never read
// the wall clock or build-info themselves; the CLI wires the live
// values at the call site.
type PageContext struct {
	RootPrefix  string
	GeneratedAt string
	Version     string
}

// ConclusionPageInput is the payload for RenderConclusionPage.
type ConclusionPageInput struct {
	// Conclusion is the finding to render. Must be non-nil; the
	// renderer rejects nil rather than panic.
	Conclusion *exchange.Conclusion

	// OutputID is the parent analyst output's UUID. Surfaced in the
	// footer for traceability and used to look up RelatedConclusions
	// in the LinkPlan (Related ids share the parent OutputID by
	// construction).
	OutputID string

	// Analyst is the parent analyst's attribution block. Surfaced in
	// the footer.
	Analyst exchange.AgentAttribution

	// AnalystPagePath is the root-relative path to the parent analyst
	// page, used as a back-link in the footer. Empty disables the
	// back-link.
	AnalystPagePath string

	// Plan resolves RelatedConclusions cross-links. May be nil; treated
	// as empty.
	Plan *LinkPlan

	// Page bundles RootPrefix, GeneratedAt, Version.
	Page PageContext
}

// RenderConclusionPage writes one conclusion's drill-down page. Pure;
// every dynamic string is escaped via html.EscapeString.
func RenderConclusionPage(w io.Writer, in ConclusionPageInput) error {
	if in.Conclusion == nil {
		return errors.New("RenderConclusionPage: nil Conclusion")
	}
	c := in.Conclusion
	prefix := in.Page.RootPrefix

	fmt.Fprintln(w, "<!DOCTYPE html>")
	fmt.Fprintln(w, `<html lang="en">`)
	fmt.Fprintln(w, "<head>")
	fmt.Fprintln(w, `<meta charset="utf-8">`)
	fmt.Fprintf(w, "<title>%s — %s</title>\n", esc(c.ID), esc(c.Verdict))
	fmt.Fprintf(w, `<link rel="stylesheet" href="%sassets/style.css">`+"\n", esc(prefix))
	fmt.Fprintln(w, "</head>")
	fmt.Fprintln(w, "<body>")

	// Verdict heading + severity badge.
	fmt.Fprintf(w, "<h1>%s</h1>\n", esc(c.Verdict))
	fmt.Fprintf(w, `<p class="severity-badge %s">Severity: %s</p>`+"\n",
		severityClass(c.Severity.Default), esc(string(c.Severity.Default)))

	// Metadata: id, category, signal type, answers-question.
	fmt.Fprintln(w, `<dl class="metadata">`)
	writeDT(w, "ID", c.ID)
	writeDT(w, "Category", c.Category)
	if c.SignalType != nil && *c.SignalType != "" {
		writeDT(w, "Signal type", *c.SignalType)
	}
	if c.AnswersQuestion != nil && *c.AnswersQuestion != "" {
		writeDT(w, "Answers question", *c.AnswersQuestion)
	}
	fmt.Fprintln(w, "</dl>")

	// Rationale.
	if c.Rationale != "" {
		fmt.Fprintln(w, `<section class="rationale">`)
		fmt.Fprintln(w, "<h2>Rationale</h2>")
		writeParagraphs(w, c.Rationale)
		fmt.Fprintln(w, "</section>")
	}

	// Citations.
	if len(c.Citations) > 0 {
		fmt.Fprintln(w, `<section class="citations">`)
		fmt.Fprintln(w, "<h2>Citations</h2>")
		fmt.Fprintln(w, "<ul>")
		for _, cit := range c.Citations {
			writeCitation(w, cit)
		}
		fmt.Fprintln(w, "</ul>")
		fmt.Fprintln(w, "</section>")
	}

	// Prerequisites.
	if len(c.Prerequisites) > 0 {
		fmt.Fprintln(w, `<section class="prerequisites">`)
		fmt.Fprintln(w, "<h2>Prerequisites</h2>")
		fmt.Fprintln(w, "<ul>")
		for _, p := range c.Prerequisites {
			fmt.Fprintf(w, "<li>%s</li>\n", esc(p))
		}
		fmt.Fprintln(w, "</ul>")
		fmt.Fprintln(w, "</section>")
	}

	// Remediation hints.
	if len(c.RemediationHints) > 0 {
		fmt.Fprintln(w, `<section class="remediation-hints">`)
		fmt.Fprintln(w, "<h2>Remediation Hints</h2>")
		fmt.Fprintln(w, "<ul>")
		for _, r := range c.RemediationHints {
			fmt.Fprintf(w, "<li>%s</li>\n", esc(r))
		}
		fmt.Fprintln(w, "</ul>")
		fmt.Fprintln(w, "</section>")
	}

	// Related conclusions: linked when the plan resolves them, plain
	// escaped text otherwise. Related ids by convention live in the
	// same output as the current conclusion.
	if len(c.RelatedConclusions) > 0 {
		fmt.Fprintln(w, `<section class="related-conclusions">`)
		fmt.Fprintln(w, "<h2>Related Conclusions</h2>")
		fmt.Fprintln(w, "<ul>")
		for _, rid := range c.RelatedConclusions {
			path := in.Plan.ConclusionPagePath(in.OutputID, rid)
			if path == "" {
				fmt.Fprintf(w, "<li><span class=\"unresolved\">%s</span></li>\n", esc(rid))
			} else {
				fmt.Fprintf(w, `<li><a href="%s%s">%s</a></li>`+"\n",
					esc(prefix), esc(path), esc(rid))
			}
		}
		fmt.Fprintln(w, "</ul>")
		fmt.Fprintln(w, "</section>")
	}

	// Supersession chain.
	if len(c.Supersedes) > 0 {
		fmt.Fprintln(w, `<section class="supersedes">`)
		fmt.Fprintln(w, "<h2>Supersedes</h2>")
		fmt.Fprintln(w, "<ul>")
		for _, sup := range c.Supersedes {
			fmt.Fprintf(w, "<li>%s — %s",
				esc(sup.PriorID), esc(string(sup.Kind)))
			if sup.PriorRound > 0 {
				fmt.Fprintf(w, " (prior round %d)", sup.PriorRound)
			}
			fmt.Fprintln(w, "</li>")
		}
		fmt.Fprintln(w, "</ul>")
		fmt.Fprintln(w, "</section>")
	}

	// Footer with attribution and back-links.
	fmt.Fprintln(w, `<footer class="report-footer">`)
	fmt.Fprintln(w, "<dl>")
	writeDT(w, "Conclusion ID", c.ID)
	writeDT(w, "Output ID", in.OutputID)
	writeDT(w, "Analyst", in.Analyst.AnalystID)
	if in.Analyst.Model != "" {
		writeDT(w, "Model", in.Analyst.Model)
	}
	if in.Analyst.InvokedAt != "" {
		writeDT(w, "Invoked at", in.Analyst.InvokedAt)
	}
	if in.Analyst.Round > 0 {
		writeDT(w, "Round", fmt.Sprintf("round %d", in.Analyst.Round))
	}
	fmt.Fprintln(w, "</dl>")
	fmt.Fprintln(w, `<nav class="back-links">`)
	fmt.Fprintf(w, `<a href="%sindex.html">Back to synthesis</a>`+"\n", esc(prefix))
	if in.AnalystPagePath != "" {
		fmt.Fprintf(w, ` &middot; <a href="%s%s">Back to analyst</a>`+"\n",
			esc(prefix), esc(in.AnalystPagePath))
	}
	fmt.Fprintln(w, "</nav>")
	fmt.Fprintf(w, "<p>Generated at %s by signatory %s.</p>\n",
		esc(in.Page.GeneratedAt), esc(in.Page.Version))
	fmt.Fprintln(w, "</footer>")

	fmt.Fprintln(w, "</body>")
	fmt.Fprintln(w, "</html>")
	return nil
}

// writeCitation emits one citation as a <li> with whatever fields are
// populated. Phase A renders citations as informational text only;
// see design §0 for the rationale (Citation has no URL field).
func writeCitation(w io.Writer, c exchange.Citation) {
	fmt.Fprint(w, `<li class="citation">`)
	switch {
	case c.Path != "":
		fmt.Fprintf(w, "<code>%s</code>", esc(c.Path))
		if c.LineStart != nil {
			if c.LineEnd != nil && *c.LineEnd != *c.LineStart {
				fmt.Fprintf(w, " lines %d–%d", *c.LineStart, *c.LineEnd)
			} else {
				fmt.Fprintf(w, " line %d", *c.LineStart)
			}
		}
	case c.Scope != nil:
		fmt.Fprintf(w, "scope %s: <code>%s</code>",
			esc(c.Scope.Kind), esc(c.Scope.Path))
	}
	if c.CommitSHA != nil && *c.CommitSHA != "" {
		fmt.Fprintf(w, ` <span class="commit">@%s</span>`, esc(*c.CommitSHA))
	}
	if c.Quoted != nil && *c.Quoted != "" {
		fmt.Fprintf(w, "<br><blockquote>%s</blockquote>", esc(*c.Quoted))
	}
	fmt.Fprintln(w, "</li>")
}

// severityClass maps a SeverityValue to a CSS class. Unknown severities
// fall back to "severity-unknown" so the renderer never emits an
// empty class attribute.
func severityClass(v exchange.SeverityValue) string {
	switch v {
	case exchange.SeverityCritical,
		exchange.SeverityHigh,
		exchange.SeverityMedium,
		exchange.SeverityLow,
		exchange.SeverityInformational,
		exchange.SeverityPositive:
		return "severity-" + string(v)
	default:
		return "severity-unknown"
	}
}
