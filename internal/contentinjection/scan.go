package contentinjection

// ScanOptions tunes Scan / ScanFile behavior. Default zero value is
// equivalent to the pre-options behavior — every primitive runs.
type ScanOptions struct {
	// SuppressPrimitives lists primitives the caller wants skipped
	// entirely. Useful when the caller knows a particular primitive
	// produces noise on the file class being scanned — e.g.
	// PrimitiveMarkdownComment is useless on AI-instruction files
	// (CLAUDE.md, AGENTS.md, .cursorrules, …) because imperative
	// prose IS the expected content there, per
	// design/anti-subversion.md §"Where AI-instruction files fit"
	// §2. Suppressed primitives are not executed (compute saving)
	// and do not appear in ScanResult.Findings even with Count 0.
	SuppressPrimitives []Primitive
}

// Scan runs every primitive over content and returns the aggregated
// result. Equivalent to ScanWithOptions(content, ScanOptions{}).
func Scan(content []byte) ScanResult {
	return ScanWithOptions(content, ScanOptions{})
}

// ScanWithOptions runs the primitives over content with the given
// options. Findings is the list of primitives that fired with
// Count > 0; primitives with no occurrences, and primitives the
// caller suppressed, are both omitted from Findings.
//
// The eight primitives are evaluated in this order:
//
//  1. PrimitiveInvisibleUnicode
//  2. PrimitiveBidiControl
//  3. PrimitiveTagBlock
//  4. PrimitiveMarkdownComment
//  5. PrimitiveMarkdownImage
//  6. PrimitiveLexicalInjection
//  7. PrimitiveEncodedBlob
//  8. PrimitiveConfusableMixedScript
//
// Order is stable across calls; consumers may rely on it for
// deterministic signal-value emission. The order reflects the
// design doc's grouping (rune-scan family first, regex family next,
// length-distribution family next, script-mix detection last) but
// carries no semantic weight beyond presentation.
func ScanWithOptions(content []byte, opts ScanOptions) ScanResult {
	suppressed := make(map[Primitive]struct{}, len(opts.SuppressPrimitives))
	for _, p := range opts.SuppressPrimitives {
		suppressed[p] = struct{}{}
	}
	suppress := func(p Primitive) bool {
		_, ok := suppressed[p]
		return ok
	}

	var result ScanResult

	// Rune-scan family: one pass over content. Suppressed
	// primitives are computed (the cost is shared) but their
	// findings are dropped before append.
	if !suppress(PrimitiveInvisibleUnicode) ||
		!suppress(PrimitiveBidiControl) ||
		!suppress(PrimitiveTagBlock) {
		invisible, bidi, tag := scanRuneFamily(content)
		if !suppress(PrimitiveInvisibleUnicode) && invisible.Count > 0 {
			result.Findings = append(result.Findings, invisible)
		}
		if !suppress(PrimitiveBidiControl) && bidi.Count > 0 {
			result.Findings = append(result.Findings, bidi)
		}
		if !suppress(PrimitiveTagBlock) && tag.Count > 0 {
			result.Findings = append(result.Findings, tag)
		}
	}

	if !suppress(PrimitiveMarkdownComment) {
		if f := scanMarkdownComment(content); f.Count > 0 {
			result.Findings = append(result.Findings, f)
		}
	}
	if !suppress(PrimitiveMarkdownImage) {
		if f := scanMarkdownImage(content); f.Count > 0 {
			result.Findings = append(result.Findings, f)
		}
	}
	if !suppress(PrimitiveLexicalInjection) {
		if f := scanLexicalInjection(content); f.Count > 0 {
			result.Findings = append(result.Findings, f)
		}
	}
	if !suppress(PrimitiveEncodedBlob) {
		if f := scanEncodedBlob(content); f.Count > 0 {
			result.Findings = append(result.Findings, f)
		}
	}
	if !suppress(PrimitiveConfusableMixedScript) {
		if f := scanConfusableMixedScript(content); f.Count > 0 {
			result.Findings = append(result.Findings, f)
		}
	}

	return result
}
