package contentinjection

// Primitive identifies one structural injection-surface detector.
// String-typed for stable serialization in signal payloads and for
// human readability in dogfood-metrics output.
type Primitive string

const (
	// PrimitiveInvisibleUnicode — zero-width characters used to split
	// tokens or smuggle hidden instructions past a visual review.
	// Detection: rune scan against U+200B–U+200D, U+2060–U+2064,
	// U+FEFF (mid-file only; leading BOM tolerated).
	PrimitiveInvisibleUnicode Primitive = "invisible_unicode"

	// PrimitiveBidiControl — bidirectional formatting controls that
	// cause visual / logical reordering of text. The Trojan-Source
	// 2021 class of attack. Detection: rune scan against
	// U+202A–U+202E, U+2066–U+2069.
	PrimitiveBidiControl Primitive = "bidi_control"

	// PrimitiveTagBlock — Unicode tag characters used as a
	// near-invisible side channel for LLM prompt injection.
	// Detection: rune scan against U+E0000–U+E007F.
	PrimitiveTagBlock Primitive = "tag_block"

	// PrimitiveMarkdownComment — HTML / markdown comments containing
	// imperative-mood prose. The injection vector hides from visual
	// readers (comments don't render) while remaining visible to LLM
	// ingestion. Detection: regex over <!-- ... --> blocks with
	// imperative-verb-density heuristic.
	PrimitiveMarkdownComment Primitive = "markdown_comment"

	// PrimitiveMarkdownImage — markdown image syntax pointing at
	// parameterized or exfil-shaped URLs. The CamoLeak-specific
	// signature. Detection: regex over ![...](URL) with URL
	// classification (query string, length, host class).
	PrimitiveMarkdownImage Primitive = "markdown_image"

	// PrimitiveLexicalInjection — known prompt-injection phrases in
	// non-code prose. Cheap; noisy on technical writing about AI.
	// Detection: regex over a small phrase catalog.
	PrimitiveLexicalInjection Primitive = "lexical_injection"

	// PrimitiveEncodedBlob — long base-N encoded runs in prose. The
	// CamoLeak exfil format and a generic obfuscation primitive.
	// Detection: length-distribution heuristic on base16 / base32 /
	// base64 runs.
	PrimitiveEncodedBlob Primitive = "encoded_blob"
)

// Finding is one positive observation from a primitive scan. Count
// is the total number of occurrences; Details carries primitive-
// specific evidence (codepoint set, regex match samples, URLs) up
// to a per-finding cap.
//
// A primitive that fires with Count == 0 is NOT emitted — the
// ScanResult.Findings slice is the union of positive findings only.
type Finding struct {
	Primitive Primitive `json:"primitive"`
	Count     int       `json:"count"`
	Details   []string  `json:"details,omitempty"`
}

// ScanResult aggregates findings across primitives for one input.
// Findings is the (possibly empty) list of primitives that fired.
// Truncated is set when input exceeded ScanFile's size cap; the
// findings reflect the in-cap prefix only.
type ScanResult struct {
	Findings  []Finding `json:"findings"`
	Truncated bool      `json:"truncated,omitempty"`
}

// HasFindings reports whether at least one primitive produced a
// positive finding.
func (r ScanResult) HasFindings() bool { return len(r.Findings) > 0 }

// detailCap bounds the number of evidence entries each Finding may
// carry. Bounded to prevent a payload designed to flood the analyst
// surface from causing an unbounded signal payload. The Count is the
// authoritative occurrence number; Details is illustrative.
const detailCap = 16
