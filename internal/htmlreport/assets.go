package htmlreport

import (
	"embed"
	"io/fs"
)

// embeddedAssets carries the static files copied verbatim into every
// generated report's assets/ directory. The directory layout under
// embedded_assets/ mirrors the on-disk layout under <subdir>/assets/
// — fs.Sub is used at copy time to strip the prefix.
//
// The directory is named embedded_assets/ (not _assets/) because the
// embed directive skips path components beginning with underscore or
// dot unless an "all:" prefix is supplied, and "all:" pulls in
// dotfiles too. A plain alphabetic dir keeps the rule legible.
//
//go:embed embedded_assets/style.css
var embeddedAssets embed.FS

// AssetsFS returns a filesystem rooted at the assets directory —
// "style.css" sits at the root, not at "embedded_assets/style.css".
// Callers iterate it with fs.WalkDir to copy the tree into a real
// directory at report-write time.
func AssetsFS() fs.FS {
	sub, err := fs.Sub(embeddedAssets, "embedded_assets")
	if err != nil {
		// Unreachable: the subdir exists as a build-time constant.
		// Returning the unsubbed FS here would still compile but
		// would silently break callers; panic surfaces the bug.
		panic("htmlreport: embedded_assets subdir missing: " + err.Error())
	}
	return sub
}

// StyleCSS returns the embedded stylesheet bytes. Convenience for
// the directory writer (which copies it verbatim) and for tests
// that want to byte-compare the on-disk copy against the embed.
func StyleCSS() []byte {
	b, err := fs.ReadFile(AssetsFS(), "style.css")
	if err != nil {
		panic("htmlreport: style.css missing from embed: " + err.Error())
	}
	return b
}
