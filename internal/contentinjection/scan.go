package contentinjection

// Scan runs every primitive over content and returns the aggregated
// result. Findings is the list of primitives that fired with Count > 0;
// primitives with no occurrences are omitted from Findings.
//
// The seven primitives are evaluated in this order:
//
//  1. PrimitiveInvisibleUnicode
//  2. PrimitiveBidiControl
//  3. PrimitiveTagBlock
//  4. PrimitiveMarkdownComment
//  5. PrimitiveMarkdownImage
//  6. PrimitiveLexicalInjection
//  7. PrimitiveEncodedBlob
//
// Order is stable across calls; consumers may rely on it for
// deterministic signal-value emission. The order reflects the
// design doc's grouping (rune-scan family first, regex family next,
// length-distribution family last) but carries no semantic weight
// beyond presentation.
func Scan(content []byte) ScanResult {
	var result ScanResult

	invisible, bidi, tag := scanRuneFamily(content)
	for _, f := range []Finding{invisible, bidi, tag} {
		if f.Count > 0 {
			result.Findings = append(result.Findings, f)
		}
	}

	for _, f := range []Finding{
		scanMarkdownComment(content),
		scanMarkdownImage(content),
		scanLexicalInjection(content),
		scanEncodedBlob(content),
	} {
		if f.Count > 0 {
			result.Findings = append(result.Findings, f)
		}
	}

	return result
}
