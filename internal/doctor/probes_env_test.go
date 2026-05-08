package doctor

import (
	"strings"
	"testing"
)

// TestProbeGitHubToken: unset / whitespace-only / present. Whitespace
// triggers warn because TrimSpace is the right policy for "is the
// var meaningfully populated" — a profile that exports
// `GITHUB_TOKEN=" "` behaves identically to unset for the API.
func TestProbeGitHubToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		env        map[string]string
		wantStatus Status
	}{
		{name: "unset", env: map[string]string{}, wantStatus: StatusWarn},
		{name: "empty", env: map[string]string{"GITHUB_TOKEN": ""}, wantStatus: StatusWarn},
		{name: "whitespace", env: map[string]string{"GITHUB_TOKEN": "   "}, wantStatus: StatusWarn},
		{name: "set", env: map[string]string{"GITHUB_TOKEN": "ghp_realish"}, wantStatus: StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{
				Getenv: func(k string) string { return tc.env[k] },
			})
			got := probeGitHubToken(r)

			if got.Name != "github-token" {
				t.Errorf("Name = %q, want github-token", got.Name)
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

// TestProbeGitHubToken_DoesNotLeakValue is a security guardrail.
// The probe must never echo the token contents into Message or Fix
// — accidental leakage of a real GITHUB_TOKEN into a doctor report
// shared in a bug ticket would be a credential disclosure.
func TestProbeGitHubToken_DoesNotLeakValue(t *testing.T) {
	t.Parallel()

	const secret = "ghp_supersecret_do_not_leak"
	r := resolveOptions(Options{
		Getenv: func(k string) string {
			if k == "GITHUB_TOKEN" {
				return secret
			}
			return ""
		},
	})
	got := probeGitHubToken(r)

	if strings.Contains(got.Message, secret) || strings.Contains(got.Fix, secret) {
		t.Fatalf("token value leaked into Result: %+v", got)
	}
}

// TestProbeMkcertOnPath: warn when missing (a healthy NODE_EXTRA_CA_
// CERTS setup may not need mkcert any more), ok when present.
func TestProbeMkcertOnPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		lookPath   func(string) (string, error)
		wantStatus Status
	}{
		{
			name:       "found",
			lookPath:   func(_ string) (string, error) { return "/opt/homebrew/bin/mkcert", nil },
			wantStatus: StatusOK,
		},
		{
			name:       "missing",
			lookPath:   func(_ string) (string, error) { return "", errStub },
			wantStatus: StatusWarn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := resolveOptions(Options{LookPath: tc.lookPath})
			got := probeMkcertOnPath(r)

			if got.Name != "mkcert-on-path" {
				t.Errorf("Name = %q, want mkcert-on-path", got.Name)
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
