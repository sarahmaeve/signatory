package htmlreport

import (
	"fmt"
	"io"
)

// ConclusionStubInput is the payload for RenderConclusionStub. Stub
// pages stand in for KeyConclusionRefs whose target was not found in
// the loaded analyst outputs (data corruption, round mismatch,
// supersession edge). They keep the navigation graph connected and
// surface a banner so the operator knows to investigate.
//
// Concordance and contradiction misses are NOT given stub pages —
// they render as plain escaped text inline (see design §0).
type ConclusionStubInput struct {
	// Ref describes what couldn't be resolved.
	Ref DanglingRef

	// AnalystPagePath is the root-relative path to the parent analyst
	// page. Empty disables the analyst back-link (used when the
	// dangling output was never loaded at all, so no analyst page
	// exists for it).
	AnalystPagePath string

	// Page bundles RootPrefix, GeneratedAt, Version.
	Page PageContext
}

// RenderConclusionStub writes a stub page for an unresolvable
// reference. Pure; every dynamic string is escaped via
// html.EscapeString.
func RenderConclusionStub(w io.Writer, in ConclusionStubInput) error {
	prefix := in.Page.RootPrefix

	fmt.Fprintln(w, "<!DOCTYPE html>")
	fmt.Fprintln(w, `<html lang="en">`)
	fmt.Fprintln(w, "<head>")
	fmt.Fprintln(w, `<meta charset="utf-8">`)
	fmt.Fprintf(w, "<title>Missing reference: %s</title>\n", esc(in.Ref.LocalID))
	fmt.Fprintf(w, `<link rel="stylesheet" href="%sassets/style.css">`+"\n", esc(prefix))
	fmt.Fprintln(w, "</head>")
	fmt.Fprintln(w, "<body>")

	fmt.Fprintf(w, "<h1>Missing reference: %s</h1>\n", esc(in.Ref.LocalID))

	// The banner is the diagnostic surface. CSS class makes it
	// unmistakable visually; the inline text gives the operator the
	// command-line follow-up they need.
	fmt.Fprintln(w, `<div class="missing-reference-banner" role="alert">`)
	fmt.Fprintln(w, "<p><strong>This reference could not be resolved against the synthesis's linked analyst outputs.</strong></p>")
	if in.Ref.Reason != "" {
		fmt.Fprintf(w, "<p>Reason: %s.</p>\n", esc(in.Ref.Reason))
	}
	fmt.Fprintln(w, "<dl>")
	writeDT(w, "Local ID", in.Ref.LocalID)
	writeDT(w, "Output ID", in.Ref.OutputID)
	fmt.Fprintln(w, "</dl>")
	fmt.Fprintln(w, "<p>Investigate with <code>signatory show-analyses</code> or <code>signatory_show_conclusions</code>.</p>")
	fmt.Fprintln(w, "</div>")

	// Footer.
	fmt.Fprintln(w, `<footer class="report-footer">`)
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
