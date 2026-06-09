package pypiwheel

import (
	"io"
	"path"
	"strings"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
	"github.com/sarahmaeve/signatory/internal/contentinjection"
)

// bundledScriptMaxBytes bounds the bytes content-scanned from any single
// bundled script. The Hades _index.js loader is small; 4 MiB is a
// generous ceiling. An entry over the cap is recorded in the manifest's
// SkippedScans (never silently dropped), and the over-cap case is
// caveated on the signal.
const bundledScriptMaxBytes = 4 << 20

// nativeLibSampleCap bounds the native_libs inventory sample carried in
// the signal payload. The count is authoritative; the sample is
// illustrative (a wide-platform wheel can ship dozens of .so files).
const nativeLibSampleCap = 50

// foreignScriptExts are non-Python script extensions whose presence in a
// Python wheel is the foreign-runtime-payload carrier shape (the
// campaign bundles _index.js and runs it through Bun). Python source is
// deliberately excluded — it is the source-AST layer's job, and scanning
// every .py with the content-injection primitives would flood.
var foreignScriptExts = map[string]struct{}{
	".js": {}, ".mjs": {}, ".cjs": {}, ".jsx": {},
	".ts": {}, ".tsx": {}, ".sh": {},
}

// isForeignScript reports whether a wheel entry path is a non-Python
// script (by extension). Inventory-level: used for foreign_scripts_total.
func isForeignScript(p string) bool {
	_, ok := foreignScriptExts[strings.ToLower(path.Ext(p))]
	return ok
}

// webAssetDirSegments are served-asset / bundler-output directory names.
// Files under them are shipped to a browser, NOT executed by a Python
// import — so a foreign script there is a web asset, not a runtime
// payload. Excluding them from the CONTENT scan kills the dominant false
// positive (minified React/Vue bundles trip encoded_blob / invisible-
// Unicode / confusable on legitimate i18n + minification) without
// blinding the detector: every branch of the Miasma/Hades campaign lands
// _index.js at an import-reachable root (loaded by the .pth/.so), never
// under static/.
var webAssetDirSegments = map[string]struct{}{
	"static": {}, "_static": {}, "assets": {}, "frontend": {},
	"node_modules": {}, "vendor": {}, "dist": {},
}

// isWebAssetScript reports whether a foreign-script path sits in a
// served-asset tree (any path segment in webAssetDirSegments).
func isWebAssetScript(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if _, ok := webAssetDirSegments[strings.ToLower(seg)]; ok {
			return true
		}
	}
	return false
}

// isPayloadReachableScript reports whether a foreign script is one the
// content scanner should inspect: a non-Python script NOT in a served-
// asset tree — i.e. positioned where a .pth/.so loader could execute it.
func isPayloadReachableScript(p string) bool {
	return isForeignScript(p) && !isWebAssetScript(p)
}

// nativeLibSuffixes are compiled-extension suffixes. Matched by suffix
// (not path.Ext) so ".abi3.so" and ".cpython-312-...-gnu.so" both hit on
// ".so".
var nativeLibSuffixes = []string{".so", ".pyd", ".dylib", ".dll"}

// isNativeLib reports whether a wheel entry path is a compiled native
// extension. The bytes are NOT scanned — a compiled object cannot be
// content-inspected at this layer; it is inventoried so co-location with
// a foreign script (the .abi3.so → _index.js trojanization shape) is
// visible.
func isNativeLib(p string) bool {
	lower := strings.ToLower(p)
	for _, suf := range nativeLibSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

// ScriptInjection is one bundled non-Python script with content-
// injection findings, carried in the wheel_bundled_payload signal.
type ScriptInjection struct {
	Path     string                     `json:"path"`
	Findings []contentinjection.Finding `json:"findings"`
}

// bundledScriptScanner returns a stream.Scanner that runs the content-
// injection primitives over every bundled foreign script, counting how
// many were scanned and retaining only the scripts that produced
// findings. The markdown_comment and markdown_image primitives are
// suppressed: both target HTML/markdown documents and are irrelevant to
// JS payloads (markdown_image is additionally FP-prone on JS string
// literals). The remaining primitives carry the load — lexical injection
// (the fake-prompt-injection header's prose), invisible Unicode / bidi /
// tag-block (near-zero-FP smuggling carriers), and encoded blobs (the
// char-code-array obfuscation the Hades loader uses).
func bundledScriptScanner(out *[]ScriptInjection, scanned *int) stream.Scanner {
	opts := contentinjection.ScanOptions{
		SuppressPrimitives: []contentinjection.Primitive{
			contentinjection.PrimitiveMarkdownComment,
			contentinjection.PrimitiveMarkdownImage,
		},
	}
	return stream.Scanner{
		Name:    "bundled-script",
		MaxSize: bundledScriptMaxBytes,
		Match:   func(e stream.Entry) bool { return isPayloadReachableScript(e.Path) },
		Scan: func(p string, body io.Reader) error {
			b, err := io.ReadAll(body)
			if err != nil {
				return err
			}
			*scanned++
			if r := contentinjection.ScanWithOptions(b, opts); r.HasFindings() {
				*out = append(*out, ScriptInjection{Path: p, Findings: r.Findings})
			}
			return nil
		},
	}
}

// foreignScriptsTotal counts every bundled non-Python script in the
// wheel — including web assets the content scanner skips — so the signal
// reports the full inventory (foreign_scripts_total) alongside the
// payload-reachable subset that was actually scanned. Header-only.
func foreignScriptsTotal(m *stream.Manifest) int {
	if m == nil {
		return 0
	}
	n := 0
	for _, e := range m.Entries {
		if e.Type == stream.EntryFile && isForeignScript(e.Path) {
			n++
		}
	}
	return n
}

// nativeLibsFromManifest collects the native-extension paths from a
// walked wheel's header listing — no content read, header-only.
func nativeLibsFromManifest(m *stream.Manifest) []string {
	if m == nil {
		return nil
	}
	var libs []string
	for _, e := range m.Entries {
		if e.Type == stream.EntryFile && isNativeLib(e.Path) {
			libs = append(libs, e.Path)
		}
	}
	return libs
}

func capStrings(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
