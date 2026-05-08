package doctor

import (
	"strings"
	"testing"
)

// TestProbeGoRuntime asserts the version-band logic against
// runtime.Version()-style inputs. CLAUDE.md pins the policy:
// 1.24 = warn (minimum), 1.25+ = ok, anything older = fail.
// Pre-release suffixes ("go1.25rc1") count as that minor version.
// Unparseable strings are warn — we'd rather flag the oddity than
// silently ignore a future release naming change.
func TestProbeGoRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		goVersion  string
		wantStatus Status
	}{
		{name: "current 1.25.1", goVersion: "go1.25.1", wantStatus: StatusOK},
		{name: "future 1.30.0", goVersion: "go1.30.0", wantStatus: StatusOK},
		{name: "minimum 1.24.0", goVersion: "go1.24.0", wantStatus: StatusWarn},
		{name: "minimum 1.24.5", goVersion: "go1.24.5", wantStatus: StatusWarn},
		{name: "below min 1.23.0", goVersion: "go1.23.0", wantStatus: StatusFail},
		{name: "ancient 1.20.0", goVersion: "go1.20.0", wantStatus: StatusFail},
		{name: "rc counts as 1.25", goVersion: "go1.25rc1", wantStatus: StatusOK},
		{name: "unparseable", goVersion: "devel +abc123", wantStatus: StatusWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{
				GoVersion: func() string { return tc.goVersion },
			})
			got := probeGoRuntime(r)

			if got.Name != "go-runtime" {
				t.Errorf("Name = %q, want %q", got.Name, "go-runtime")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (msg=%q)", got.Status, tc.wantStatus, got.Message)
			}
			if got.Status != StatusOK && got.Fix == "" {
				t.Errorf("non-OK Status without Fix: %+v", got)
			}
			if !strings.Contains(got.Message, tc.goVersion) {
				t.Errorf("Message %q does not echo input version %q", got.Message, tc.goVersion)
			}
		})
	}
}

// TestProbeGitOnPath verifies fail when LookPath returns an error,
// ok when it returns a path, and that the resolved path is echoed
// in Message so users can spot a $PATH oddity (e.g., the wrong
// package manager's git winning).
func TestProbeGitOnPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		lookPath   func(string) (string, error)
		wantStatus Status
		wantInMsg  string
	}{
		{
			name:       "found",
			lookPath:   func(_ string) (string, error) { return "/usr/bin/git", nil },
			wantStatus: StatusOK,
			wantInMsg:  "/usr/bin/git",
		},
		{
			name:       "not found",
			lookPath:   func(_ string) (string, error) { return "", errStub },
			wantStatus: StatusFail,
			wantInMsg:  "git",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{LookPath: tc.lookPath})
			got := probeGitOnPath(r)

			if got.Name != "git-on-path" {
				t.Errorf("Name = %q, want git-on-path", got.Name)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if !strings.Contains(got.Message, tc.wantInMsg) {
				t.Errorf("Message = %q, want substring %q", got.Message, tc.wantInMsg)
			}
			if got.Status != StatusOK && got.Fix == "" {
				t.Errorf("non-OK Status without Fix: %+v", got)
			}
		})
	}
}
