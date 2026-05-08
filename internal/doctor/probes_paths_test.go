package doctor

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFileInfo is the minimal os.FileInfo we need for stat seam
// tests. Only Name() and IsDir() are read by the probes; the
// rest are zero-valued. Defining it once here keeps the test
// files in this package free of duplicate fixtures.
type fakeFileInfo struct {
	name string
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return 0 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

// statTable builds a Stat seam from a path → result map. Paths
// not in the map return os.ErrNotExist, which matches what os.Stat
// surfaces for missing files (and is what errors.Is(err, os.ErrNotExist)
// matches against). Probes treat that case distinctly from other
// stat errors, so the table interface needs to express both.
func statTable(table map[string]fakeFileInfo) func(string) (os.FileInfo, error) {
	return func(p string) (os.FileInfo, error) {
		if info, ok := table[p]; ok {
			return info, nil
		}
		return nil, os.ErrNotExist
	}
}

// ---- binary-stamped --------------------------------------------------------

func TestProbeBinaryStamped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		version    string
		commit     string
		buildDate  string
		wantStatus Status
	}{
		{name: "fully stamped", version: "v1.2.3", commit: "abc123", buildDate: "2026-05-08", wantStatus: StatusOK},
		{name: "version is dev", version: "dev", commit: "abc123", wantStatus: StatusWarn},
		{name: "commit is none", version: "v1.2.3", commit: "none", wantStatus: StatusWarn},
		{name: "both unstamped", version: "dev", commit: "none", wantStatus: StatusWarn},
		{name: "version empty", version: "", commit: "abc123", wantStatus: StatusWarn},
		{name: "commit empty", version: "v1.2.3", commit: "", wantStatus: StatusWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{
				Version:   tc.version,
				Commit:    tc.commit,
				BuildDate: tc.buildDate,
			})
			got := probeBinaryStamped(r)

			if got.Name != "binary-stamped" {
				t.Errorf("Name = %q, want binary-stamped", got.Name)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (msg=%q)", got.Status, tc.wantStatus, got.Message)
			}
			if got.Status != StatusOK && got.Fix == "" {
				t.Errorf("non-OK Status without Fix: %+v", got)
			}
		})
	}
}

// ---- home-signatory-dir ----------------------------------------------------

func TestProbeHomeSignatoryDir(t *testing.T) {
	t.Parallel()

	const home = "/Users/test"
	signatoryPath := filepath.Join(home, ".signatory")

	tests := []struct {
		name       string
		homeErr    error
		stat       func(string) (os.FileInfo, error)
		wantStatus Status
		wantInMsg  string
	}{
		{
			name:       "dir exists",
			stat:       statTable(map[string]fakeFileInfo{signatoryPath: {name: ".signatory", dir: true}}),
			wantStatus: StatusOK,
			wantInMsg:  signatoryPath,
		},
		{
			name:       "dir does not exist yet",
			stat:       statTable(map[string]fakeFileInfo{}),
			wantStatus: StatusOK,
			wantInMsg:  "will be created",
		},
		{
			name:       "path is a regular file",
			stat:       statTable(map[string]fakeFileInfo{signatoryPath: {name: ".signatory", dir: false}}),
			wantStatus: StatusFail,
			wantInMsg:  "not a directory",
		},
		{
			name:       "home dir errors",
			homeErr:    errStub,
			stat:       statTable(map[string]fakeFileInfo{}),
			wantStatus: StatusFail,
			wantInMsg:  "user home",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{
				UserHomeDir: func() (string, error) {
					if tc.homeErr != nil {
						return "", tc.homeErr
					}
					return home, nil
				},
				Stat: tc.stat,
			})
			got := probeHomeSignatoryDir(r)

			if got.Name != "home-signatory-dir" {
				t.Errorf("Name = %q, want home-signatory-dir", got.Name)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (msg=%q)", got.Status, tc.wantStatus, got.Message)
			}
			if !strings.Contains(got.Message, tc.wantInMsg) {
				t.Errorf("Message %q does not contain %q", got.Message, tc.wantInMsg)
			}
			if got.Status != StatusOK && got.Fix == "" {
				t.Errorf("non-OK Status without Fix: %+v", got)
			}
		})
	}
}

// ---- mcp-config-present ----------------------------------------------------

func TestProbeMCPConfigPresent(t *testing.T) {
	t.Parallel()

	const cwd = "/work/proj"
	mcpPath := filepath.Join(cwd, ".mcp.json")

	tests := []struct {
		name       string
		cwdErr     error
		stat       func(string) (os.FileInfo, error)
		wantStatus Status
	}{
		{
			name:       "file exists",
			stat:       statTable(map[string]fakeFileInfo{mcpPath: {name: ".mcp.json", dir: false}}),
			wantStatus: StatusOK,
		},
		{
			name:       "missing",
			stat:       statTable(map[string]fakeFileInfo{}),
			wantStatus: StatusWarn,
		},
		{
			name:       "is a directory",
			stat:       statTable(map[string]fakeFileInfo{mcpPath: {name: ".mcp.json", dir: true}}),
			wantStatus: StatusFail,
		},
		{
			name:       "cwd errors",
			cwdErr:     errStub,
			stat:       statTable(map[string]fakeFileInfo{}),
			wantStatus: StatusWarn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{
				Getwd: func() (string, error) {
					if tc.cwdErr != nil {
						return "", tc.cwdErr
					}
					return cwd, nil
				},
				Stat: tc.stat,
			})
			got := probeMCPConfigPresent(r)

			if got.Name != "mcp-config-present" {
				t.Errorf("Name = %q, want mcp-config-present", got.Name)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (msg=%q)", got.Status, tc.wantStatus, got.Message)
			}
			if got.Status != StatusOK && got.Fix == "" {
				t.Errorf("non-OK Status without Fix: %+v", got)
			}
		})
	}
}

// ---- skills-present --------------------------------------------------------

func TestProbeSkillsPresent(t *testing.T) {
	t.Parallel()

	const cwd = "/work/proj"
	analyze := filepath.Join(cwd, ".claude", "skills", "analyze")
	vet := filepath.Join(cwd, ".claude", "skills", "vet-dependency")

	tests := []struct {
		name       string
		stat       func(string) (os.FileInfo, error)
		wantStatus Status
	}{
		{
			name: "both present",
			stat: statTable(map[string]fakeFileInfo{
				analyze: {name: "analyze", dir: true},
				vet:     {name: "vet-dependency", dir: true},
			}),
			wantStatus: StatusOK,
		},
		{
			name: "analyze missing",
			stat: statTable(map[string]fakeFileInfo{
				vet: {name: "vet-dependency", dir: true},
			}),
			wantStatus: StatusWarn,
		},
		{
			name:       "both missing",
			stat:       statTable(map[string]fakeFileInfo{}),
			wantStatus: StatusWarn,
		},
		{
			name: "analyze is a file, not dir",
			stat: statTable(map[string]fakeFileInfo{
				analyze: {name: "analyze", dir: false},
				vet:     {name: "vet-dependency", dir: true},
			}),
			wantStatus: StatusWarn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{
				Getwd: func() (string, error) { return cwd, nil },
				Stat:  tc.stat,
			})
			got := probeSkillsPresent(r)

			if got.Name != "skills-present" {
				t.Errorf("Name = %q, want skills-present", got.Name)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Status != StatusOK && got.Fix == "" {
				t.Errorf("non-OK Status without Fix: %+v", got)
			}
		})
	}
}

// Smoke that the helper used in stat tables actually surfaces the
// os.ErrNotExist sentinel — probes call errors.Is on it, so the
// table fixture lying about the error type would silently mis-test.
func TestStatTable_MissingPathReturnsErrNotExist(t *testing.T) {
	t.Parallel()

	stat := statTable(nil)
	_, err := stat("/nonexistent")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("statTable: missing path returned %v, want os.ErrNotExist", err)
	}
}
