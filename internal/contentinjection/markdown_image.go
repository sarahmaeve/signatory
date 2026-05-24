package contentinjection

import "regexp"

// markdownImagePattern captures the URL of every markdown image
// reference in the input. The capture group is the URL between `(`
// and `)`. Alt text is non-capturing.
//
// Pattern: ![alt](url) — alt is `[^\]]*` (anything but `]`), URL is
// `[^)]+` (any non-`)` characters). The intentional simplicity here
// is calibrated for markdown source not yet rendered; the regex does
// not try to handle escaped parens inside URLs, which are rare in
// practice and absent from the CamoLeak-shaped payload class.
var markdownImagePattern = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)

// urlExfilSampleLen bounds the URL sample stored in Details. Long
// enough to identify the host and the parameterized portion; short
// enough that a 4 KB image URL doesn't blow the signal payload.
const urlExfilSampleLen = 200

// markdownImageURLLongThreshold is the URL-length above which a
// markdown image reference fires PrimitiveMarkdownImage on length
// alone. Calibrated above ordinary badge / banner URLs (rarely
// exceed 150 chars even with multiple query parameters) and below
// the size of an actual encoded-payload exfil URL (typically
// 300–2000 chars for one frame of a dictionary-of-pixels payload).
const markdownImageURLLongThreshold = 200

// markdownImageQueryValueLongThreshold is the query-value length
// above which a single parameter value is considered exfil-shaped.
// 64 chars covers a SHA-256 digest or a moderately-long opaque
// token without firing on it; encoded exfil payloads in the
// CamoLeak class run hundreds of chars per value.
const markdownImageQueryValueLongThreshold = 96

// queryValuePattern extracts each `key=value` pair from a URL's
// query-string portion. Value capture is the second group; key the
// first. Permissive on key/value contents because URL escaping is
// up to the source — we measure raw lengths, not parsed semantics.
var queryValuePattern = regexp.MustCompile(`[?&]([^=&]+)=([^&]*)`)

// scanMarkdownImage fires PrimitiveMarkdownImage when a markdown
// image reference's URL exhibits either length-shaped or
// query-shaped exfil structure. Each firing image contributes a
// sample URL (truncated to urlExfilSampleLen) to Details.
//
// False-positive note: long URLs occur legitimately in some
// CDN-hosted artwork and in some auth-flow badges. The analyst is
// expected to weight findings here by file role (a README image
// reference is differently meaningful than a hidden-comment image
// reference).
func scanMarkdownImage(content []byte) Finding {
	out := Finding{Primitive: PrimitiveMarkdownImage}
	for _, m := range markdownImagePattern.FindAllSubmatch(content, -1) {
		url := string(m[1])
		if !isExfilShapedURL(url) {
			continue
		}
		out.Count++
		if len(out.Details) < detailCap {
			out.Details = append(out.Details, truncateURLForDetail(url))
		}
	}
	return out
}

// isExfilShapedURL reports whether url has length-shaped or query-
// shaped exfil structure per the v0 thresholds above.
func isExfilShapedURL(url string) bool {
	if len(url) > markdownImageURLLongThreshold {
		return true
	}
	for _, m := range queryValuePattern.FindAllStringSubmatch(url, -1) {
		if len(m[2]) > markdownImageQueryValueLongThreshold {
			return true
		}
	}
	return false
}

// truncateURLForDetail caps a URL at urlExfilSampleLen with an
// ellipsis suffix so the signal payload stays bounded.
func truncateURLForDetail(url string) string {
	if len(url) <= urlExfilSampleLen {
		return url
	}
	return url[:urlExfilSampleLen] + "..."
}
