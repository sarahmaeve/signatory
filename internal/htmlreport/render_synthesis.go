package htmlreport

import (
	"errors"
	"fmt"
	"html"
	"io"
	"slices"
	"strings"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// SynthesisIndexInput bundles the parameters for RenderSynthesisIndex.
// All time-and-version stamps are passed in: the renderer never
// reads the wall clock or the build info itself, so unit tests can
// pin both to fixed strings.
type SynthesisIndexInput struct {
	// Synth is the synthesis output. Must carry a non-nil
	// SynthesisSupplement; the renderer rejects non-synthesis inputs
	// rather than panicking on the nil dereference.
	Synth *exchange.AnalystOutput

	// ShortName is the human-friendly identifier for the title
	// ("Trust Assessment: <ShortName>"). Caller-supplied to keep the
	// renderer free of store access.
	ShortName string

	// TargetURL is the resolved external registry URL for Synth.Target
	// (e.g. https://pypi.org/project/dark-matter/ for
	// pkg:pypi/dark-matter). Empty disables the link; target then
	// renders as plain escaped text. The CLI computes this via
	// PURLToRegistryURL.
	TargetURL string

	// RecordedPosture is the entity's currently-recorded posture from
	// signatory's own store, if any. Distinct from
	// Synth.SynthesisSupplement.ProposedPosture, which is what the
	// synthesist proposed in this report. The metadata block surfaces
	// both side-by-side so a reader can spot drift between the
	// proposal and the operator's prior decision.
	RecordedPosture *RecordedPosture

	// Plan resolves every cross-reference (key conclusions,
	// concordance ids, contradiction ids) to a relative page path.
	// May be nil; treated as empty (every reference renders as
	// plain text).
	Plan *LinkPlan

	// GeneratedAt is the timestamp written to the footer. Opaque to
	// the renderer — the CLI typically passes RFC3339, but any
	// pre-formatted string works.
	GeneratedAt string

	// Version is the signatory build/version stamp written to the
	// footer.
	Version string
}

// RecordedPosture is the renderer's view of a profile.Posture. We
// keep it as a small local struct rather than importing the profile
// package directly so the htmlreport package stays a leaf — the CLI
// translates from profile.Posture at the call site.
type RecordedPosture struct {
	Tier      string // "vetted-frozen" / "trusted-for-now" / etc.
	Version   string // empty when unversioned
	SetBy     string
	SetAt     string // pre-formatted by the caller
	Rationale string
}

// RenderSynthesisIndex writes the index.html for a synthesis output to
// w. Pure function — no store access, no clock read, no filesystem
// touch — so test composition is straightforward.
//
// Section order mirrors renderSynthesisMarkdown so a reader familiar
// with the markdown form sees the same shape: title → posture →
// metadata → reasoning → summary → concordance → contradictions →
// key conclusions → gaps → action items → notes → footer. Optional
// sections elide when their source array is empty.
//
// Every dynamic string is escaped via html.EscapeString. There is no
// template engine; escaping is the renderer's responsibility on
// every Fprintf.
func RenderSynthesisIndex(w io.Writer, in SynthesisIndexInput) error {
	if in.Synth == nil || in.Synth.SynthesisSupplement == nil {
		return errors.New("RenderSynthesisIndex: input has no synthesis_supplement")
	}
	out := in.Synth
	s := out.SynthesisSupplement

	fmt.Fprintln(w, "<!DOCTYPE html>")
	fmt.Fprintln(w, `<html lang="en">`)
	fmt.Fprintln(w, "<head>")
	fmt.Fprintln(w, `<meta charset="utf-8">`)
	fmt.Fprintf(w, "<title>Trust Assessment: %s</title>\n", esc(in.ShortName))
	fmt.Fprintln(w, `<link rel="stylesheet" href="assets/style.css">`)
	fmt.Fprintln(w, "</head>")
	fmt.Fprintln(w, "<body>")

	// Top-of-page banner. Signals "this is signatory output" before
	// the title or any specifics — useful both to ground the reader
	// and to stamp every printed/screenshot copy with provenance.
	fmt.Fprintln(w, `<div class="agent-banner" role="banner">SIGNATORY AGENT REPORT</div>`)

	fmt.Fprintf(w, "<h1>Trust Assessment: %s</h1>\n", esc(in.ShortName))

	// Recommended posture call-out (synthesist's proposal).
	tier := s.ProposedPosture.Tier
	fmt.Fprintf(w, `<div class="posture-callout %s">`+"\n", postureClass(tier))
	fmt.Fprintf(w, "<strong>Recommended posture:</strong> %s", esc(tier))
	if s.ProposedPosture.VersionScope != "" {
		fmt.Fprintf(w, ` <span class="version-scope">(%s)</span>`, esc(s.ProposedPosture.VersionScope))
	}
	fmt.Fprint(w, ` <span class="posture-source">(from synthesist)</span>`)
	fmt.Fprintln(w)
	if s.ProposedPosture.RationaleSummary != "" {
		fmt.Fprintf(w, "<p>%s</p>\n", esc(s.ProposedPosture.RationaleSummary))
	}
	fmt.Fprintln(w, "</div>")

	// Current recorded posture call-out (from signatory's store), if
	// any. Visually parallel to the recommended one so drift between
	// proposal and prior operator decision is visible at a glance.
	//
	// Common case: the operator accepted the recommendation with
	// `posture accept --yes`, which copies tier + version_scope +
	// rationale_summary verbatim into the posture row. In that case
	// the rationale and version are already on the recommended call-
	// out above and showing them again is just noise — collapse the
	// recorded block to the heading line plus "Set by NAME at TIME"
	// so the operator sees provenance of the decision without
	// duplication.
	//
	// When the tiers disagree, an extra class drives a clashing
	// border so the disagreement is impossible to miss.
	if rp := in.RecordedPosture; rp != nil {
		matchesRecommendation := recordedMatchesRecommended(rp, &s.ProposedPosture)
		classes := postureClass(rp.Tier) + " recorded"
		if rp.Tier != tier {
			classes += " differs-from-recommended"
		}
		fmt.Fprintf(w, `<div class="posture-callout %s">`+"\n", classes)
		fmt.Fprintf(w, "<strong>Current recorded posture:</strong> %s", esc(rp.Tier))
		// Only surface the version on the recorded callout when it
		// adds information — i.e. when it differs from the
		// recommended scope above.
		if rp.Version != "" && !matchesRecommendation {
			fmt.Fprintf(w, ` <span class="version-scope">(%s)</span>`, esc(rp.Version))
		}
		fmt.Fprint(w, ` <span class="posture-source">(from signatory db)</span>`)
		fmt.Fprintln(w)
		// Only surface the rationale paragraph when it adds
		// information beyond the recommended block.
		if rp.Rationale != "" && !matchesRecommendation {
			fmt.Fprintf(w, "<p>%s</p>\n", esc(rp.Rationale))
		}
		if rp.SetBy != "" || rp.SetAt != "" {
			fmt.Fprint(w, `<p class="posture-meta">`)
			if rp.SetBy != "" {
				fmt.Fprintf(w, "Set by %s", esc(rp.SetBy))
			}
			if rp.SetAt != "" {
				if rp.SetBy != "" {
					fmt.Fprint(w, " ")
				}
				fmt.Fprintf(w, "at %s", esc(rp.SetAt))
			}
			fmt.Fprintln(w, ".</p>")
		}
		fmt.Fprintln(w, "</div>")
	}

	// Metadata block.
	fmt.Fprintln(w, `<dl class="metadata">`)
	writeDT(w, "Synthesist", out.Attribution.AnalystID)
	if out.Attribution.Model != "" {
		writeDT(w, "Model", out.Attribution.Model)
	}
	if out.Attribution.InvokedAt != "" {
		writeDT(w, "Invoked at", out.Attribution.InvokedAt)
	}
	writeTargetDT(w, out.Target, in.TargetURL)
	if out.TargetCommit != "" {
		writeDT(w, "Target commit", out.TargetCommit)
	}
	fmt.Fprintln(w, "</dl>")

	// Reasoning. Local-id mentions (e.g. "F001") inside the prose
	// are linkified to the in-page key-conclusion anchors so a
	// reader skimming the narrative can jump to the cited finding
	// without scrolling.
	fmt.Fprintln(w, `<section class="reasoning">`)
	fmt.Fprintln(w, "<h2>Reasoning</h2>")
	writeReasoningProse(w, s.Reasoning, in.Plan)
	fmt.Fprintln(w, "</section>")

	// Summary.
	fmt.Fprintln(w, `<section class="summary">`)
	fmt.Fprintln(w, "<h2>Summary</h2>")
	writeReasoningProse(w, s.Summary, in.Plan)
	fmt.Fprintln(w, "</section>")

	// Concordance.
	if len(s.ConcordanceStrengths) > 0 {
		fmt.Fprintln(w, `<section class="concordance">`)
		fmt.Fprintln(w, "<h2>Cross-analyst Concordance</h2>")
		for _, c := range s.ConcordanceStrengths {
			writeConcordanceEntry(w, c, in.Plan)
		}
		fmt.Fprintln(w, "</section>")
	}

	// Contradictions.
	if len(s.ContradictionsDetected) > 0 {
		fmt.Fprintln(w, `<section class="contradictions">`)
		fmt.Fprintln(w, "<h2>Contradictions Detected</h2>")
		for _, c := range s.ContradictionsDetected {
			writeContradictionEntry(w, c, in.Plan)
		}
		fmt.Fprintln(w, "</section>")
	}

	// Key conclusions, sorted by ascending weight.
	if len(s.KeyConclusionRefs) > 0 {
		fmt.Fprintln(w, `<section class="key-conclusions">`)
		fmt.Fprintln(w, "<h2>Key Conclusions (ranked by weight on the posture decision)</h2>")
		sorted := slices.Clone(s.KeyConclusionRefs)
		slices.SortStableFunc(sorted, func(a, b exchange.ConclusionRef) int {
			return a.Weight - b.Weight
		})
		fmt.Fprintln(w, "<ol>")
		for _, r := range sorted {
			writeKeyConclusionRef(w, r, in.Plan)
		}
		fmt.Fprintln(w, "</ol>")
		fmt.Fprintln(w, "</section>")
	}

	// Gaps.
	if len(s.Gaps) > 0 {
		fmt.Fprintln(w, `<section class="gaps">`)
		fmt.Fprintln(w, "<h2>Gaps and Limitations</h2>")
		fmt.Fprintln(w, "<ul>")
		for _, g := range s.Gaps {
			fmt.Fprintf(w, "<li>%s</li>\n", esc(g))
		}
		fmt.Fprintln(w, "</ul>")
		fmt.Fprintln(w, "</section>")
	}

	// Action items.
	if len(s.ActionItems) > 0 {
		fmt.Fprintln(w, `<section class="action-items">`)
		fmt.Fprintln(w, "<h2>Action Items</h2>")
		fmt.Fprintln(w, "<ol>")
		for _, a := range s.ActionItems {
			fmt.Fprintf(w, "<li>%s</li>\n", esc(a))
		}
		fmt.Fprintln(w, "</ol>")
		fmt.Fprintln(w, "</section>")
	}

	// Notes.
	if s.Notes != "" {
		fmt.Fprintln(w, `<section class="notes">`)
		fmt.Fprintln(w, "<h2>Notes</h2>")
		writeParagraphs(w, s.Notes)
		fmt.Fprintln(w, "</section>")
	}

	// Footer.
	fmt.Fprintln(w, `<footer class="report-footer">`)
	fmt.Fprintf(w, "<p>Generated at %s by signatory %s.</p>\n",
		esc(in.GeneratedAt), esc(in.Version))
	fmt.Fprintln(w, "</footer>")

	fmt.Fprintln(w, "</body>")
	fmt.Fprintln(w, "</html>")
	return nil
}

// esc is the universal escape used on every dynamic string. Wrapping
// html.EscapeString in a one-letter local makes Fprintf call sites
// readable; the indirection is intentional.
func esc(s string) string { return html.EscapeString(s) }

// writeDT emits one <dt>/<dd> pair. Both label and value are escaped.
func writeDT(w io.Writer, label, value string) {
	fmt.Fprintf(w, "<dt>%s</dt><dd>%s</dd>\n", esc(label), esc(value))
}

// writeParagraphs splits prose on blank lines and emits each chunk
// as <p>…</p>. Empty input writes nothing. No Markdown is parsed —
// inline syntax renders as literal escaped text in v0.1.
func writeParagraphs(w io.Writer, prose string) {
	prose = strings.TrimSpace(prose)
	if prose == "" {
		return
	}
	for para := range strings.SplitSeq(prose, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		fmt.Fprintf(w, "<p>%s</p>\n", esc(para))
	}
}

// writeConcordanceEntry emits one concordance entry with linked or
// plain-text local-id refs depending on Plan resolution.
func writeConcordanceEntry(w io.Writer, c exchange.ConcordanceEntry, plan *LinkPlan) {
	fmt.Fprintln(w, `<div class="concordance-entry">`)
	fmt.Fprintf(w, "<h3>Agreement: %s", esc(c.Topic))
	if c.Confidence != "" {
		fmt.Fprintf(w, " <span class=\"confidence\">(%s confidence)</span>", esc(c.Confidence))
	}
	fmt.Fprintln(w, "</h3>")
	if c.Description != "" {
		fmt.Fprintf(w, "<p>%s</p>\n", esc(c.Description))
	}
	if len(c.AnalystRefs) > 0 {
		fmt.Fprintf(w, "<p><strong>Analysts:</strong> %s</p>\n",
			esc(strings.Join(c.AnalystRefs, ", ")))
	}
	if len(c.ConclusionIDs) > 0 {
		fmt.Fprint(w, "<p><strong>Refs:</strong> ")
		writeLocalIDList(w, c.ConclusionIDs, c.AnalystRefs, plan)
		fmt.Fprintln(w, "</p>")
	}
	fmt.Fprintln(w, "</div>")
}

// writeContradictionEntry emits one contradiction entry. Both sides'
// local-id refs run through the same resolver as concordance.
func writeContradictionEntry(w io.Writer, c exchange.ContradictionEntry, plan *LinkPlan) {
	fmt.Fprintln(w, `<div class="contradiction-entry">`)
	fmt.Fprintf(w, "<h3>Contradiction: %s</h3>\n", esc(c.Topic))
	if c.Description != "" {
		fmt.Fprintf(w, "<p>%s</p>\n", esc(c.Description))
	}
	if c.SupportingAnalystA != "" || len(c.ConclusionIDsA) > 0 {
		fmt.Fprintf(w, "<p><strong>%s:</strong> ", esc(c.SupportingAnalystA))
		writeLocalIDList(w, c.ConclusionIDsA, []string{c.SupportingAnalystA}, plan)
		fmt.Fprintln(w, "</p>")
	}
	if c.SupportingAnalystB != "" || len(c.ConclusionIDsB) > 0 {
		fmt.Fprintf(w, "<p><strong>%s:</strong> ", esc(c.SupportingAnalystB))
		writeLocalIDList(w, c.ConclusionIDsB, []string{c.SupportingAnalystB}, plan)
		fmt.Fprintln(w, "</p>")
	}
	if c.ResolutionPreference != "" {
		fmt.Fprintf(w, "<p><strong>Resolution:</strong> %s</p>\n",
			esc(c.ResolutionPreference))
	}
	fmt.Fprintln(w, "</div>")
}

// writeLocalIDList emits a comma-separated list of local ids, each
// rendered as an <a> if Plan can resolve it through the given analyst
// scope or as escaped plain text if it can't.
func writeLocalIDList(w io.Writer, localIDs, analystScope []string, plan *LinkPlan) {
	for i, id := range localIDs {
		if i > 0 {
			fmt.Fprint(w, ", ")
		}
		path := plan.ResolveConcordanceID(analystScope, id)
		if path == "" {
			fmt.Fprint(w, esc(id))
		} else {
			fmt.Fprintf(w, `<a href="%s">%s</a>`, esc(path), esc(id))
		}
	}
}

// writeKeyConclusionRef emits one <li> for a KeyConclusionRefs entry.
// Resolves the (output-id, local-id) pair against the plan; falls
// back to escaped plain text if the pair is missing (dangling).
//
// Each <li> carries id="kc-<output-short>-<local>" so prose mentions
// of the same id earlier in the page can intra-page-link to it. Note
// the prose linkifier also accepts a bare-local-id form for
// convenience; the qualified id avoids collision when two analysts
// emit the same local id.
func writeKeyConclusionRef(w io.Writer, r exchange.ConclusionRef, plan *LinkPlan) {
	anchorID := keyConclusionAnchor(r.OutputID, r.ConclusionLocalID)
	fmt.Fprintf(w, `<li id="%s" class="key-conclusion-ref">`, esc(anchorID))
	path := plan.ConclusionPagePath(r.OutputID, r.ConclusionLocalID)
	label := fmt.Sprintf("%s (output %s)", r.ConclusionLocalID, shortOutputID(r.OutputID))
	if path == "" {
		fmt.Fprintf(w, `<span class="dangling">%s</span>`, esc(label))
	} else {
		fmt.Fprintf(w, `<a href="%s">%s</a>`, esc(path), esc(label))
	}
	if r.ForgeryResistance != "" {
		fmt.Fprintf(w, ` <span class="forgery-resistance">forgery resistance: %s</span>`,
			esc(r.ForgeryResistance))
	}
	if r.RelevanceNote != "" {
		fmt.Fprintf(w, " %s", esc(r.RelevanceNote))
	}
	fmt.Fprintln(w, "</li>")
}

// keyConclusionAnchor returns the <li id="..."> string for a key-
// conclusion entry. Used both for the rendered id attribute and for
// resolving prose links in writeReasoningProse.
func keyConclusionAnchor(outputID, localID string) string {
	return "kc-" + shortOutputID(outputID) + "-" + localID
}

// writeTargetDT renders the target metadata row. When url is non-
// empty, the target URI becomes a clickable anchor pointing at the
// resolved external registry URL; otherwise it renders as plain
// escaped text via writeDT.
func writeTargetDT(w io.Writer, target, url string) {
	if url == "" {
		writeDT(w, "Target", target)
		return
	}
	fmt.Fprintf(w, "<dt>%s</dt><dd><a href=\"%s\">%s</a></dd>\n",
		esc("Target"), esc(url), esc(target))
}

// writeReasoningProse splits prose on blank lines and emits each
// paragraph as <p>...</p> with HTML escaping AND a pass that turns
// any token matching a known local-id from plan into an intra-page
// anchor link to its key-conclusion entry. Tokens are matched on
// word boundaries so a local-id like "F001" doesn't accidentally
// link inside an unrelated word; ambiguous bare local-ids (the same
// "F001" from two different analysts) are resolved arbitrarily —
// the operator can drill in either way to land on the right page.
func writeReasoningProse(w io.Writer, prose string, plan *LinkPlan) {
	prose = strings.TrimSpace(prose)
	if prose == "" {
		return
	}
	links := buildLocalIDLinks(plan)
	for para := range strings.SplitSeq(prose, "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		escaped := esc(para)
		linked := linkifyLocalIDs(escaped, links)
		fmt.Fprintf(w, "<p>%s</p>\n", linked)
	}
}

// localIDLink pairs a local id with the anchor href the prose
// linkifier should emit for it.
type localIDLink struct {
	LocalID string
	Href    string
}

// buildLocalIDLinks collects, from the LinkPlan, the set of
// local-ids that have a key-conclusion anchor in the index. Returns
// the links sorted by descending LocalID length so the substitution
// pass can match longer ids first (prevents "F1" from clobbering
// "F10" when both are present).
func buildLocalIDLinks(plan *LinkPlan) []localIDLink {
	if plan == nil {
		return nil
	}
	// Dedup by local id; if two outputs emit the same local id we
	// keep the first one we see. The renderer's anchors are
	// qualified by output, but prose is not — so an ambiguous bare
	// reference gets a reasonable default rather than no link.
	seen := map[string]string{}
	var out []localIDLink
	for key := range plan.ConclusionPages {
		if _, ok := seen[key.LocalID]; ok {
			continue
		}
		href := "#" + keyConclusionAnchor(key.OutputID, key.LocalID)
		seen[key.LocalID] = href
		out = append(out, localIDLink{LocalID: key.LocalID, Href: href})
	}
	// Stable order: longest first; ties broken alphabetically so
	// runs are deterministic for tests.
	slices.SortFunc(out, func(a, b localIDLink) int {
		if len(a.LocalID) != len(b.LocalID) {
			return len(b.LocalID) - len(a.LocalID)
		}
		return strings.Compare(a.LocalID, b.LocalID)
	})
	return out
}

// linkifyLocalIDs walks already-escaped prose and wraps each
// occurrence of a known local id (matched on ASCII word boundaries)
// in an anchor tag. The input is HTML-escaped, so wrapping it in
// <a>…</a> doesn't produce double-escaping or break the surrounding
// tag soup.
func linkifyLocalIDs(escapedProse string, links []localIDLink) string {
	if len(links) == 0 {
		return escapedProse
	}
	out := escapedProse
	for _, link := range links {
		out = replaceWordBoundary(out, link.LocalID,
			`<a href="`+link.Href+`">`+link.LocalID+`</a>`)
	}
	return out
}

// replaceWordBoundary replaces every occurrence of needle in haystack
// with replacement, but only when the match is bounded by ASCII
// non-word characters (or start/end of string). The "word"
// definition matches `[A-Za-z0-9_-]` — which is what valid
// local-ids look like in practice and which avoids accidentally
// linking F001 inside `myF001var`.
func replaceWordBoundary(haystack, needle, replacement string) string {
	if needle == "" {
		return haystack
	}
	var b strings.Builder
	i := 0
	for i < len(haystack) {
		j := strings.Index(haystack[i:], needle)
		if j < 0 {
			b.WriteString(haystack[i:])
			break
		}
		start := i + j
		end := start + len(needle)
		// Word-boundary check: the character before start and the
		// character after end must NOT be word-class. Edges of the
		// string count as boundaries.
		if !isWordChar(byteOrZero(haystack, start-1)) && !isWordChar(byteOrZero(haystack, end)) {
			b.WriteString(haystack[i:start])
			b.WriteString(replacement)
			i = end
		} else {
			b.WriteString(haystack[i : start+1])
			i = start + 1
		}
	}
	return b.String()
}

func byteOrZero(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '-'
}

// recordedMatchesRecommended reports whether the recorded posture's
// tier, version, and rationale all equal the synthesist's proposal.
// True is the "operator accepted with --yes" case: the posture row
// is a verbatim copy of the proposal, so the recorded call-out's
// optional fields would duplicate what's already on the recommended
// call-out and the renderer collapses them.
func recordedMatchesRecommended(rp *RecordedPosture, prop *exchange.ProposedPosture) bool {
	if rp == nil || prop == nil {
		return false
	}
	return rp.Tier == prop.Tier &&
		rp.Version == prop.VersionScope &&
		rp.Rationale == prop.RationaleSummary
}

// postureClass maps a tier string to a CSS class. Unknown tiers fall
// back to the generic "posture-unknown" so the renderer never emits
// an empty class attribute.
func postureClass(tier string) string {
	switch tier {
	case exchange.ProposedTierVettedFrozen,
		exchange.ProposedTierTrustedForNow,
		exchange.ProposedTierUnexamined,
		exchange.ProposedTierUnknownProvenance,
		exchange.ProposedTierRejected:
		return "posture-" + tier
	default:
		return "posture-unknown"
	}
}

// shortOutputID returns the first 8 characters of an output UUID for
// compact display. Mirrors the existing markdown renderer's helper.
func shortOutputID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
