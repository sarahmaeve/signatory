package pypiwheel

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// fakeFetcher returns fixed bytes (a synthetic wheel) or a fixed error.
type fakeFetcher struct {
	data []byte
	err  error
}

func (f fakeFetcher) Fetch(_ context.Context, _ string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

// buildWheel zips the given path→content map into wheel (zip) bytes.
func buildWheel(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func inRunWithWheelURL(entityID string) *signal.CollectionResult {
	r := &signal.CollectionResult{}
	r.RecordSignal(entityID, "wheel_url", "pypi-registry", time.Now().UTC(), time.Hour,
		map[string]any{
			"url":      "https://files.pythonhosted.org/x/pkg-1.4.2-py3-none-any.whl",
			"version":  "1.4.2",
			"filename": "pkg-1.4.2-py3-none-any.whl",
		})
	return r
}

func signalValue(t *testing.T, r *signal.CollectionResult, typ string) (map[string]any, bool) {
	t.Helper()
	for _, s := range r.Signals() {
		if s.Type == typ {
			var v map[string]any
			require.NoError(t, json.Unmarshal(s.Value, &v))
			return v, true
		}
	}
	return nil, false
}

func hasAbsence(r *signal.CollectionResult, typ string) bool {
	for _, s := range r.Signals() {
		if s.Type == "absence:"+typ {
			return true
		}
	}
	return false
}

func pypiEntity(id string) *profile.Entity {
	return &profile.Entity{ID: id, Ecosystem: "pypi", CanonicalURI: "pkg:pypi/pkg"}
}

func TestCollect_MaliciousPth_Flagged(t *testing.T) {
	t.Parallel()
	// A wheel carrying the campaign's *-setup.pth Bun loader alongside a
	// benign bare-path .pth and ordinary package code.
	wheel := buildWheel(t, map[string]string{
		"langchain_core-setup.pth": `import os, subprocess; ` +
			`subprocess.run(["bun", "run", os.path.join(os.path.dirname(__file__), "_index.js")], check=False)`,
		"pkg/__init__.py":            "x = 1\n",
		"pkg-1.4.2.dist-info/RECORD": "pkg/__init__.py,,\n",
		"add_path.pth":               "../src\n",
	})

	c := NewCollector(CollectorConfig{
		InRun:   inRunWithWheelURL("e-1"),
		Fetcher: fakeFetcher{data: wheel},
	})
	res, err := c.Collect(context.Background(), pypiEntity("e-1"))
	require.NoError(t, err)

	v, ok := signalValue(t, res, "wheel_pth_executable")
	require.True(t, ok, "signal must emit on a successful wheel open")
	assert.EqualValues(t, 2, v["pth_files_scanned"], "two .pth files in the wheel")
	assert.EqualValues(t, 1, v["total_finding_count"], "only the setup.pth is malicious")

	files, _ := v["files_with_findings"].([]any)
	require.Len(t, files, 1)
	entry := files[0].(map[string]any)
	assert.Equal(t, "langchain_core-setup.pth", entry["path"])
	findings := entry["findings"].([]any)
	reasons := findings[0].(map[string]any)["reasons"].([]any)
	assert.Contains(t, reasons, "subprocess")
	assert.Contains(t, reasons, "foreign_runtime")
}

func TestCollect_CleanWheel_EmptyPositive(t *testing.T) {
	t.Parallel()
	// A wheel whose only .pth is a legitimate setuptools nspkg shim.
	wheel := buildWheel(t, map[string]string{
		"pkg-1.4.2-nspkg.pth": `import sys, types, os;` +
			`p = os.path.join(sys._getframe(1).f_locals['sitedir'], *('pkg',));` +
			`importlib = __import__('importlib.util')`,
		"pkg/__init__.py": "x = 1\n",
	})

	c := NewCollector(CollectorConfig{
		InRun:   inRunWithWheelURL("e-2"),
		Fetcher: fakeFetcher{data: wheel},
	})
	res, err := c.Collect(context.Background(), pypiEntity("e-2"))
	require.NoError(t, err)

	v, ok := signalValue(t, res, "wheel_pth_executable")
	require.True(t, ok, "clean wheel still emits — empty findings is the positive observation")
	assert.EqualValues(t, 1, v["pth_files_scanned"])
	assert.EqualValues(t, 0, v["total_finding_count"], "nspkg shim must not flag")
	assert.False(t, hasAbsence(res, "wheel_pth_executable"),
		"a successfully-opened clean wheel is a signal, not an absence")
}

func TestCollect_NoWheelURL_Absence(t *testing.T) {
	t.Parallel()
	c := NewCollector(CollectorConfig{
		InRun:   &signal.CollectionResult{}, // no wheel_url recorded
		Fetcher: fakeFetcher{data: nil},
	})
	res, err := c.Collect(context.Background(), pypiEntity("e-3"))
	require.NoError(t, err)
	assert.True(t, hasAbsence(res, "wheel_pth_executable"),
		"missing wheel_url (e.g. sdist-only package) records an absence")
	_, ok := signalValue(t, res, "wheel_pth_executable")
	assert.False(t, ok)
}

func TestCollect_FetchError_Absence(t *testing.T) {
	t.Parallel()
	c := NewCollector(CollectorConfig{
		InRun:   inRunWithWheelURL("e-4"),
		Fetcher: fakeFetcher{err: errors.New("boom")},
	})
	res, err := c.Collect(context.Background(), pypiEntity("e-4"))
	require.NoError(t, err)
	assert.True(t, hasAbsence(res, "wheel_pth_executable"),
		"a fetch failure records an absence, never a returned error")
}
