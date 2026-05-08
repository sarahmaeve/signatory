package doctor

import (
	"errors"
	"testing"
)

// TestRun_AllProbesByDefault verifies that with an empty Only filter,
// Run executes every registered probe and returns one Result per
// probe in registry order. The exact set of names is asserted to
// catch silent registry drift — adding a probe should be a
// deliberate test update, not an invisible expansion.
func TestRun_AllProbesByDefault(t *testing.T) {
	t.Parallel()

	results := Run(stableOpts())

	got := make([]string, len(results))
	for i, r := range results {
		got[i] = r.Name
	}
	want := []string{
		"go-runtime",
		"git-on-path",
		"binary-stamped",
		"github-token",
		"home-signatory-dir",
		"node-extra-ca-certs",
		"mkcert-on-path",
		"mcp-config-present",
		"mcp-binary-matches",
		"skills-present",
		"signatory-db",
		"pipeline-service",
	}
	if !equalStrings(got, want) {
		t.Fatalf("registry order/membership drift\n got:  %v\n want: %v", got, want)
	}
}

// TestRun_OnlyFilter restricts execution to a subset and asserts
// that registry order is preserved (filtering does not reorder).
func TestRun_OnlyFilter(t *testing.T) {
	t.Parallel()

	opts := stableOpts()
	opts.Only = []string{"github-token", "go-runtime"} // deliberately reversed
	results := Run(opts)

	got := make([]string, len(results))
	for i, r := range results {
		got[i] = r.Name
	}
	want := []string{"go-runtime", "github-token"} // registry order, not Only order
	if !equalStrings(got, want) {
		t.Fatalf("Only filter did not preserve registry order\n got:  %v\n want: %v", got, want)
	}
}

// TestRun_OnlyFilter_UnknownProbeIsSkipped: a probe name that isn't
// in the registry matches nothing — Run does not error, it simply
// returns no Results for that name. Letting unknown filter terms be
// silently ignored keeps the CLI robust against typos in scripts;
// stricter validation can be added at the CLI flag level if the
// surface ever needs it.
func TestRun_OnlyFilter_UnknownProbeIsSkipped(t *testing.T) {
	t.Parallel()

	opts := stableOpts()
	opts.Only = []string{"go-runtime", "no-such-probe"}
	results := Run(opts)

	if len(results) != 1 || results[0].Name != "go-runtime" {
		t.Fatalf("unknown probe leaked into output: %+v", results)
	}
}

// TestHasFail_HasWarn confirms the convenience predicates. They are
// the basis of the CLI's exit-code decision (fail → non-zero;
// warn under --strict → non-zero); regressions in either would
// silently flip exit codes.
func TestHasFail_HasWarn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		results  []Result
		wantFail bool
		wantWarn bool
	}{
		{name: "empty", results: nil, wantFail: false, wantWarn: false},
		{name: "all-ok", results: []Result{{Status: StatusOK}, {Status: StatusOK}}, wantFail: false, wantWarn: false},
		{name: "one-warn", results: []Result{{Status: StatusOK}, {Status: StatusWarn}}, wantFail: false, wantWarn: true},
		{name: "one-fail", results: []Result{{Status: StatusOK}, {Status: StatusFail}}, wantFail: true, wantWarn: false},
		{name: "fail-and-warn", results: []Result{{Status: StatusFail}, {Status: StatusWarn}}, wantFail: true, wantWarn: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := HasFail(tc.results); got != tc.wantFail {
				t.Errorf("HasFail = %v, want %v", got, tc.wantFail)
			}
			if got := HasWarn(tc.results); got != tc.wantWarn {
				t.Errorf("HasWarn = %v, want %v", got, tc.wantWarn)
			}
		})
	}
}

// stableOpts returns Options wired to deterministic seam values, so
// Run-level tests assert structure (registry membership, ordering,
// filtering) without coupling to per-probe content. Per-probe
// behavior is tested in the matching probes_*_test.go files.
func stableOpts() Options {
	return Options{
		Version:   "v1.2.3",
		Commit:    "abc123",
		BuildDate: "2026-05-08T00:00:00Z",
		Getenv: func(key string) string {
			if key == "GITHUB_TOKEN" {
				return "ghp_fake"
			}
			return ""
		},
		LookPath:  func(_ string) (string, error) { return "/usr/bin/" + "stub", nil },
		GoVersion: func() string { return "go1.25.1" },
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// errStub is a sentinel for table-driven probe tests that need to
// assert "this seam returned an error." Defined here so per-probe
// test files share it without redeclaration.
var errStub = errors.New("stub error")
