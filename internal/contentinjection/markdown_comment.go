package contentinjection

import (
	"regexp"
	"strings"
)

// markdownCommentPattern captures the body of every <!-- ... -->
// block in the input. The `(?s)` flag makes `.` match newlines so
// multi-line comments are captured as a single unit. Non-greedy so
// adjacent comments don't merge.
var markdownCommentPattern = regexp.MustCompile(`(?s)<!--(.*?)-->`)

// markdownCommentMinBodyLen is the threshold below which a comment
// is treated as trivial (TOC markers, lint-disable, editor folds).
// Tuned to skip common housekeeping markers — "TOC", "/lint-disable",
// "prettier-ignore" — without missing prose-shaped payloads. Length
// is measured after trim, on the comment body (excluding markers).
const markdownCommentMinBodyLen = 32

// scanMarkdownComment fires PrimitiveMarkdownComment when a markdown
// HTML comment contains imperative-mood prose. Per design doc the
// detection is scored on "length × verb density, not bare presence"
// because TOC markers and lint-disable comments are legitimate.
//
// The v0 structural rule: a comment fires when its trimmed body
// exceeds markdownCommentMinBodyLen AND either
//
//   - the first whitespace-delimited word is a catalog imperative
//     verb (the directive shape: "Ignore X", "Fetch Y", "Execute Z"); or
//   - the body contains two or more catalog verbs (the verb-density
//     shape: "When summarizing... also fetch... and execute..." —
//     directive content embedded in connecting prose).
//
// Either condition catches a real payload pattern; neither fires on
// descriptive prose like "the build system will execute the suite"
// (verb mid-sentence, single occurrence) or on lint directives like
// "prettier-ignore-end" (single verb-shaped token, body too short).
func scanMarkdownComment(content []byte) Finding {
	out := Finding{Primitive: PrimitiveMarkdownComment}
	for _, m := range markdownCommentPattern.FindAllSubmatch(content, -1) {
		body := strings.TrimSpace(string(m[1]))
		if len(body) < markdownCommentMinBodyLen {
			continue
		}
		if !isImperativeShape(body) {
			continue
		}
		out.Count++
		if len(out.Details) < detailCap {
			out.Details = append(out.Details, truncateForDetail(body))
		}
	}
	return out
}

// imperativeVerbCatalog is the small set of verbs whose
// imperative-mood use in a markdown comment is injection-shaped.
// Lowercase; matching is case-insensitive on the input.
//
// The list is intentionally tight. Adding "read", "use", or "see"
// would explode the false-positive rate on legitimate technical
// prose (every README does "see X for details" inside markdown).
// The verbs here all carry an "act on the world" intent that
// markdown comments — invisible to humans, visible to LLM ingestion
// — would not legitimately direct.
//
// Calibration is per-verb. "harvest", "siphon", "egress", "reveal",
// and "decrypt" were added in the 2026-05 catalog enrichment;
// each has near-zero legitimate use in markdown comments. "scrape"
// was added with awareness that some research / tooling READMEs
// discuss web scraping as a topic — but markdown-comment use of
// "scrape" remains rare even there.
var imperativeVerbCatalog = []string{
	"ignore", "fetch", "run", "execute", "install", "download",
	"summarize", "output", "print", "send", "exfiltrate", "leak",
	"upload", "post", "submit", "transmit", "exec", "eval",
	// Catalog enrichment 2026-05: data-extraction / exfil verbs the
	// design doc's adversarial-variants enumeration named as
	// frequent in real attacks but absent from the v0 catalog.
	"harvest", "scrape", "siphon", "egress", "reveal", "decrypt",
}

// catalogVerbPattern matches any catalog verb as a word-bounded
// token anywhere in the input. Case-insensitive. Used by
// isImperativeShape to count total verb occurrences.
var catalogVerbPattern = func() *regexp.Regexp {
	verbs := strings.Join(imperativeVerbCatalog, "|")
	return regexp.MustCompile(`(?i)\b(?:` + verbs + `)\b`)
}()

// imperativeVerbSet is the lookup-form catalog used by
// startsWithCatalogVerb. Lowercase keys.
var imperativeVerbSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(imperativeVerbCatalog))
	for _, v := range imperativeVerbCatalog {
		m[v] = struct{}{}
	}
	return m
}()

// isImperativeShape reports whether the comment body shows either
// directive structure (first word is a catalog imperative verb) or
// verb-density structure (two or more catalog verbs total). See
// scanMarkdownComment for the reasoning behind the two-condition
// rule.
func isImperativeShape(body string) bool {
	if startsWithCatalogVerb(body) {
		return true
	}
	if len(catalogVerbPattern.FindAllStringIndex(body, -1)) >= 2 {
		return true
	}
	return false
}

// startsWithCatalogVerb reports whether the first whitespace-
// delimited token of body is a catalog imperative verb. Trailing
// punctuation (period, comma, colon) is stripped from the token
// before lookup so "Ignore." and "Execute," still match.
func startsWithCatalogVerb(body string) bool {
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return false
	}
	first := strings.ToLower(strings.TrimRight(fields[0], ".,;:!"))
	_, ok := imperativeVerbSet[first]
	return ok
}

// truncateForDetail trims a comment body to the first 80 bytes,
// collapsing internal whitespace runs to single spaces so the sample
// is one line in signal payload output. Delegates to
// truncateAtRuneBoundary for UTF-8 safety so a CJK / emoji rune at
// the truncation point is not split.
func truncateForDetail(body string) string {
	collapsed := strings.Join(strings.Fields(body), " ")
	return truncateAtRuneBoundary(collapsed, 80)
}
