package buildscript

import (
	"path"
	"strings"
)

// buildScriptBasenames are author-written lifecycle/build inputs,
// matched by lowercased basename. *.m4 is matched by extension
// separately (with the generatedAutotools deny-list applied first).
var buildScriptBasenames = map[string]struct{}{
	"setup.py":     {},
	"build.rs":     {},
	"extconf.rb":   {},
	"configure.ac": {},
	"configure.in": {},
	"makefile.am":  {},
}

// generatedAutotools are autotools/libtool OUTPUTS or vendored macros:
// not author-written, frequently enormous, and legitimately packed with
// eval/base64-shaped content. Scanning them produces only false
// positives, so they are excluded even when their extension (.m4) or
// name would otherwise qualify. The xz payload was a hand-written
// macro, not any of these, so excluding them costs no real coverage.
var generatedAutotools = map[string]struct{}{
	"configure": {}, "config.status": {}, "config.guess": {}, "config.sub": {},
	"config.h.in": {}, "makefile.in": {}, "aclocal.m4": {}, "libtool.m4": {},
	"ltmain.sh": {}, "ltoptions.m4": {}, "ltsugar.m4": {}, "ltversion.m4": {},
	"lt~obsolete.m4": {}, "depcomp": {}, "missing": {}, "install-sh": {},
	"compile": {}, "test-driver": {}, "ar-lib": {},
}

// IsBuildScriptSource reports whether the (posix) archive path is an
// author-written build/install script worth content-scanning. Matching
// is by basename so a wrapping top-dir ("pkg-1.0/") does not matter;
// the generatedAutotools deny-list is applied first.
func IsBuildScriptSource(p string) bool {
	base := strings.ToLower(path.Base(p))
	if _, deny := generatedAutotools[base]; deny {
		return false
	}
	if _, ok := buildScriptBasenames[base]; ok {
		return true
	}
	return strings.HasSuffix(base, ".m4")
}
