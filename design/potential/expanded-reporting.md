# Expanded Reporting: `show-synthesis --html`

**Status:** proposed, captured 2026-05-01 from a brainstorming pass on
"how do I read a synthesis without running five follow-up CLI
commands?"

**Scope:** extend `signatory show-synthesis` with an `--html=<dir>`
mode that writes a static, cross-linked HTML site rendering one
synthesis output plus drill-down pages for every finding the
synthesis references. Pure store query; no re-collection, no LLM
dispatch.

**Out of scope:** v0.2 export. This feature does **not** produce a
fully attributed, signing-ready, provenance-bearing artifact. It is a
*reading view* of what is already in the signatory store. If the
store record is incomplete, the HTML reflects the incompleteness; we
do not paper over it.

## 1. Motivation

`signatory show-synthesis <output-id>` today emits markdown to stdout.
The "Key Conclusions" section names referenced findings by
`(local-id, output-id-short)` tuples — references only, no bodies.
Concordance and contradiction sections likewise list `conclusion_ids`
without the underlying conclusions. To read the supporting findings,
the operator runs:

- `signatory show-conclusions --target <uri>` for bodies,
- `signatory show-analyses <uri>` for analyst-output context,
- `signatory summary <uri>` for current-state framing.

That's a five-command read flow for one verdict. The synthesist has
already done the work of curating which findings load-bear on the
proposed tier (`KeyConclusionRefs`) and which findings underpin
agreement / disagreement (`ConcordanceEntry.ConclusionIDs`,
`ContradictionEntry.ConclusionIDsA/B`). Those references are dead
strings in the markdown rendering; in HTML they can be live links to
inlined finding pages. The drill-down is the value.

## 2. Verb shape

```
signatory show-synthesis <output-id>              # markdown to stdout (preserved)
signatory show-synthesis <output-id> --html=DIR   # static HTML site under DIR/<subdir>/
```

`--html` is a mode switch; absent, the command behaves exactly as it
does today (`renderSynthesisMarkdown` to stdout). Present, it
suppresses stdout markdown and writes a directory of files instead.
On success, `--html` mode prints the absolute path to the generated
`index.html` to stdout — that path is the result.

The flag takes a *parent* directory. The command auto-creates a
*subdirectory* inside it and writes the site into the subdirectory.
Naming convention: `<short-name>-<output-id-short>` — e.g.
`atuin-3a8b2c14` — using the entity's `ShortName` (already looked up
in the existing `show-synthesis` code) and the first 8 characters of
the synthesis output UUID. If the short-name lookup fails, fall back
to a sanitized canonical-URI slug (strip `pkg:`, `/`, `@` to `-`).

## 3. Out-of-scope: v0.2 export

The v0.2 *export* feature, when it lands, will additionally:

- Stamp full provenance manifests (analyst attribution chains,
  signal-collection commit hashes, store schema version, signatory
  version).
- Sign the artifact with the operator's identity so a downstream
  reader can verify the bundle wasn't tampered with after generation.
- Be canonical-format and round-trippable: a future signatory version
  must be able to re-ingest its own export.

`--html` deliberately does **none** of those. Confusing the two
features is what this scope line exists to prevent. If you find
yourself reaching for "let's add a signature to the HTML", you've
crossed into v0.2.

## 4. File layout

```
<parent-dir>/
  <short-name>-<output-id-short>/
    index.html                         # the synthesis itself
    conclusions/
      <output-id-short>-<local-id>.html   # one per referenced conclusion
      …
    analysts/
      <analyst-id>-r<round>.html       # one per linked analyst output
      …
    assets/
      style.css                        # shipped template, copied verbatim
```

Conclusion-page slugs use `<output-id-short>-<local-id>` to avoid
collision: security and provenance both happily emit `F001`.

## 5. Cross-linking model

### 5.1 What gets a page

A conclusion gets a page if and only if it is referenced *anywhere*
in the synthesis:

- `SynthesisSupplement.KeyConclusionRefs[].ConclusionLocalID`
- `SynthesisSupplement.ConcordanceStrengths[].ConclusionIDs`
- `SynthesisSupplement.ContradictionsDetected[].ConclusionIDsA`
- `SynthesisSupplement.ContradictionsDetected[].ConclusionIDsB`

Conclusions outside this referenced set do not get pages. The
synthesist is the curator; if a conclusion didn't make the verdict's
narrative, it is not in the report. Operators who want the full
analyst output can reach it through the per-analyst page (5.3) which
lists every conclusion the analyst emitted, with links only for the
ones that have pages.

### 5.2 Per-conclusion page contents

- Verdict (heading) + severity badge (CSS class).
- Category, signal type.
- Rationale (markdown rendered to HTML).
- Citations rendered as `<a href>` links — the URI value already
  carries the external URL.
- Prerequisites + RemediationHints, if present.
- `RelatedConclusions` cross-links (only those that have pages; see
  5.5 for missing-reference handling).
- Supersession chain, if any.
- Footer: full conclusion UUID, full output UUID, analyst attribution,
  back-link to `index.html`, back-link to the per-analyst page.

### 5.3 Per-analyst page contents

One page per analyst output linked to this synthesis (typically
security-v1 + provenance-v1). Useful "especially when the analysts
are orthogonal or disagree" — gives a side-by-side view of each
analyst's worldview without forcing the reader to reconstruct it from
individual conclusion pages.

Contents:

- Analyst attribution (analyst-id, model, prompt-version, invoked-at,
  round).
- Round notes prose.
- Full list of conclusions emitted by this analyst — referenced ones
  are linked, non-referenced ones are listed by verdict + severity
  with no link.
- Positive absences (verbatim — no per-finding page; they're not
  conclusion-shaped).
- Observations (verbatim — same reason).
- Methodology trace section (5.4).
- Back-link to `index.html`.

### 5.4 Methodology trace

`AnalystOutput.MethodologyTrace` carries the "what we *looked* for"
record. Default-on for `--html` v0.1; render inline on the per-analyst
page. Re-evaluate after seeing it on real outputs — if it dwarfs the
rest of the page we'll move it behind a `--include-methodology` flag,
but speculative scope-cutting before seeing the rendered output is
unwarranted.

### 5.5 Missing-reference behavior

If a synthesis cites `F003` from output `abc`, but the store row for
`abc` does not contain `F003` (data corruption, round mismatch,
supersession edge), we render a **stub conclusion page** with:

- Heading: "Missing reference: `<local-id>`".
- A clearly visible warning banner indicating the reference could not
  be resolved against the linked analyst output.
- The output ID being referenced, so the operator can investigate
  with `signatory show-analyses` / `signatory_detail`.
- Back-links to index and the parent analyst page (so the
  navigation graph stays connected).

We do **not** hard-fail. Rationale: the report is a snapshot of what
is in the DB. Incomplete records are a real state — older outputs,
mid-migration rows, partial ingest failures — and the operator's
question is "what does the store currently say about X?" A stub page
answers that honestly; a hard fail forces the operator to fix the
store before they can read the report, which inverts the intended
read-only / diagnostic-friendly use case.

The stub page's banner is the diagnostic surface. If the operator
sees one, they know something to investigate.

### 5.6 The link graph

```
index.html
  ├─ conclusion pages (key + concordance + contradiction refs)
  │     ├─ back-link to index
  │     ├─ back-link to parent analyst page
  │     └─ optional cross-links to RelatedConclusions / Supersedes
  ├─ analyst pages (one per linked AnalystOutput)
  │     ├─ back-link to index
  │     └─ forward-links to each of its referenced conclusion pages
  └─ assets/style.css
```

Every page must have at least a back-link to `index.html`. No
orphans.

## 6. CSS / styling

External `assets/style.css`, shipped as a template. One stylesheet
linked from every page. Severity styling lives entirely in CSS:

```
.severity-critical      { ... }
.severity-high          { ... }
.severity-medium        { ... }
.severity-low           { ... }
.severity-informational { ... }
.severity-positive      { ... }
```

Posture tier styling parallel:

```
.posture-vetted-frozen     { ... }
.posture-trusted-for-now   { ... }
.posture-unexamined        { ... }
.posture-unknown-provenance{ ... }
.posture-rejected          { ... }
```

A "missing reference" banner has a dedicated class so it's
unmistakable visually:

```
.missing-reference-banner { ... }
```

The shipped stylesheet ships as a `_template/` embed (Go 1.24
`embed.FS`), copied verbatim into `<subdir>/assets/style.css` at
generation time. v0.1 is a single stylesheet — no theming, no
configurable templates. A future iteration can add `--style=<path>`
if anyone needs it; we will not introduce that complexity speculatively.

## 7. `index.html` layout

Mirrors the existing `renderSynthesisMarkdown` structure so a reader
familiar with the markdown form sees the same shape:

- Title (`Trust Assessment: <short-name>`).
- Posture call-out (CSS-styled, the headline answer).
- Target metadata block (URI, version-scope, target_commit, repo URL
  as `<a>`).
- Synthesis attribution (analyst-id, model, invoked-at).
- Table of contents.
- Reasoning (full prose, markdown → HTML).
- Summary.
- Concordance strengths section. Each `ConclusionIDs` entry rendered
  as an `<a>` to its conclusion page.
- Contradictions detected. Each `ConclusionIDsA` / `ConclusionIDsB`
  entry rendered as an `<a>`.
- Key conclusions, sorted by ascending Weight. Each entry links to
  its conclusion page.
- Gaps.
- Action items.
- Notes.
- Footer: full synthesis output UUID, generated-at timestamp,
  signatory version stamp (`version`/`commit`/`buildDate` — already
  threaded into the binary at build time).

## 8. Behavior decisions

### 8.1 Refusal conditions

`--html=<parent-dir>` refuses **outright** (no `--force`, no
overwrite) if any of:

- `<parent-dir>` does not exist.
- `<parent-dir>` is not writable by the current user.
- The auto-named subdir already exists inside `<parent-dir>`.

The operator's recovery is to choose a different parent or remove
the existing subdir manually. v0.1 has no `--force`; if it becomes a
real ergonomic problem we add it later.

### 8.2 Output channel on success

Stdout: the absolute path to the generated `index.html`, one line, no
trailing prose. The operator can `open "$(signatory show-synthesis
<id> --html=DIR)"` directly. Stderr stays empty on success; reserved
for diagnostics.

### 8.3 Markdown mode unchanged

`signatory show-synthesis <output-id>` (no `--html`) continues to
write markdown to stdout. The existing `ShowSynthesisCmd.Run` path is
preserved verbatim. `--html` is purely additive; there is no
deprecation.

## 9. Implementation slicing (TDD)

Four test layers, each with its own failing test landing first:

### 9.1 Pure HTML renderers

Pure functions, no store, no FS:

- `renderSynthesisIndex(w io.Writer, synth *exchange.AnalystOutput, plan *LinkPlan) error`
- `renderConclusionPage(w io.Writer, c *exchange.Conclusion, ctx *ConclusionPageContext, plan *LinkPlan) error`
- `renderConclusionStub(w io.Writer, ref DanglingRef, plan *LinkPlan) error`
- `renderAnalystPage(w io.Writer, ao *exchange.AnalystOutput, plan *LinkPlan) error`

Test fixture-driven: feed in known supplements and assert on:

- Anchor presence (every referenced `local-id` has a corresponding
  `href`).
- Severity class correctness (`<… class="severity-high"…>`).
- Back-link reciprocity (synthesis → conclusion → back to synthesis).
- Stub-banner presence on the missing-reference renderer.

### 9.2 Cross-link resolver (pure)

`buildLinkPlan(synth *exchange.AnalystOutput, analystOutputs []*exchange.AnalystOutput) (*LinkPlan, []DanglingRef)`

Given the synthesis supplement plus the set of linked analyst
outputs, produce:

- `LinkPlan`: map from `(output-id, local-id)` → page path; from
  `analyst-id` → page path.
- `[]DanglingRef`: every reference in the synthesis whose target is
  not present in any provided analyst output.

Table-driven tests cover:

- Conclusion in `KeyConclusionRefs` only.
- Conclusion in concordance only.
- Conclusion in both contradiction sides.
- Conclusion referenced multiple times (one page, one entry in plan).
- Dangling reference (synthesis cites `F003` from output `abc`, but
  `abc.Conclusions` does not contain `F003`).
- Missing analyst output entirely (synthesis references output `xyz`
  that wasn't linked).

### 9.3 Directory writer

`writeReportTree(parentDir string, plan *LinkPlan, …) (indexPath string, err error)`

Wraps the renderers + link plan; walks the file tree under a
`t.TempDir()`. Tests:

- Refuse-if-parent-missing: error contains the missing path.
- Refuse-if-parent-unwriteable: error is permissions-shaped.
- Refuse-if-subdir-exists: error names the existing subdir.
- Successful run produces every expected file (`index.html`, each
  conclusion page, each analyst page, `assets/style.css`).
- `assets/style.css` byte-for-byte matches the shipped template.
- Returned `indexPath` is absolute and points at an actual file.

### 9.4 Integration

End-to-end test that:

1. Opens a temp store.
2. Ingests a known synthesis fixture (plus its referenced analyst
   outputs).
3. Runs `ShowSynthesisCmd{OutputID: …, HTMLDir: tmp}` through `Run`.
4. Walks the resulting tree, asserts on structural invariants
   (every conclusion-page link in the index resolves to an existing
   file; no orphan pages; severity classes match the source data).

The integration test does not assert on byte-level HTML — that's the
unit tests' job. It asserts on the *shape* of what was produced.

## 10. Open questions / future work

- **Do we ship multiple stylesheets (light / dark / print)?** v0.1
  ships one. If real use surfaces a need (e.g. printing for a
  security review), revisit.
- **Multi-target index page.** Out of scope for v0.1; the unit is
  one synthesis output. If/when v0.2 adds project-level export, the
  multi-target index becomes a natural extension.
- **JS for collapsible sections / search?** No. v0.1 ships zero JS.
  The site must work with JS disabled. If we ever need search,
  static-site generators (Hugo, Zola) handle this kind of cross-link
  far better than ad-hoc JS would; we'd port to one of those before
  hand-rolling.
- **Linkable identity to other entities.** `Summary.RelatedURIs` —
  related-identity links — is not surfaced by `show-synthesis`
  today and is not added by this proposal. Captured here as a
  natural extension once the related-identities surface stabilizes.

## 11. See also

- [`m6-synthesis-contract.md`](m6-synthesis-contract.md) — the
  synthesis contract the rendering reads from.
- [`agent-facing-contract.md`](agent-facing-contract.md) §5 M7 — the
  `signatory summary` verb that addresses the *current-state* read
  flow this proposal complements.
- [`v0.1-invariants.md`](v0.1-invariants.md) — Invariant 3
  (SQLite-canonical, files-are-views) is what makes "snapshot of the
  store" a sound design center.
- `cmd/signatory/show_synthesis.go` — the existing markdown renderer
  whose pure-function shape (`renderSynthesisMarkdown`) this proposal
  mirrors for the HTML side.
