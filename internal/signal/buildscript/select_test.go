package buildscript_test

import (
	"testing"

	"github.com/sarahmaeve/signatory/internal/signal/buildscript"
	"github.com/stretchr/testify/require"
)

func TestIsBuildScriptSource(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Author-written build inputs — scanned.
		{"setup.py", true},
		{"pkg-1.0/setup.py", true},
		{"build.rs", true},
		{"ext/extconf.rb", true},
		{"configure.ac", true},
		{"configure.in", true},
		{"Makefile.am", true},
		{"m4/build-to-host.m4", true},
		{"m4/my_macro.m4", true},

		// Generated / vendored autotools output — NOT scanned (huge and
		// legitimately full of eval/base64-ish content; the xz payload
		// lived in a hand-written macro, not generated configure output).
		{"configure", false},
		{"config.status", false},
		{"Makefile.in", false},
		{"aclocal.m4", false},
		{"m4/libtool.m4", false},
		{"m4/ltsugar.m4", false},
		{"ltmain.sh", false},
		{"config.guess", false},

		// Ordinary source / docs — NOT scanned. build.rs matches by
		// basename, so a regular .rs file must not.
		{"src/lib.rs", false},
		{"pkg/__init__.py", false},
		{"README.md", false},
	}
	for _, c := range cases {
		require.Equalf(t, c.want, buildscript.IsBuildScriptSource(c.path), "path %q", c.path)
	}
}
