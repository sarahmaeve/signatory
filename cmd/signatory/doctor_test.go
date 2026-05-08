package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sarahmaeve/signatory/internal/doctor"
)

// TestDoctorCmd_ProseRender_AllOK: every probe ok → "status: OK"
// footer, no fix lines, exit 0 (no error).
func TestDoctorCmd_ProseRender_AllOK(t *testing.T) {
	t.Parallel()

	results := []doctor.Result{
		{Name: "go-runtime", Status: doctor.StatusOK, Message: "go1.25.1"},
		{Name: "git-on-path", Status: doctor.StatusOK, Message: "/usr/bin/git"},
	}
	var buf bytes.Buffer
	cmd := &DoctorCmd{
		Stdout: &buf,
		Runner: func(_ doctor.Options) []doctor.Result { return results },
	}
	err := cmd.Run(&Globals{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"signatory doctor",
		"[ ok ] go-runtime",
		"go1.25.1",
		"[ ok ] git-on-path",
		"/usr/bin/git",
		"status: OK",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "fix:") {
		t.Errorf("ok-only render should not include any fix lines:\n%s", out)
	}
}

// TestDoctorCmd_ProseRender_FailExits: a fail result should drive
// errSilentFailure (so main.go exits non-zero) and the prose
// footer should announce "NOT OK" with counts.
func TestDoctorCmd_ProseRender_FailExits(t *testing.T) {
	t.Parallel()

	results := []doctor.Result{
		{Name: "go-runtime", Status: doctor.StatusOK, Message: "go1.25.1"},
		{Name: "node-extra-ca-certs", Status: doctor.StatusFail, Message: "not set", Fix: "run `signatory certs init`"},
		{Name: "github-token", Status: doctor.StatusWarn, Message: "GITHUB_TOKEN is not set", Fix: "export GITHUB_TOKEN=..."},
	}
	var buf bytes.Buffer
	cmd := &DoctorCmd{
		Stdout: &buf,
		Runner: func(_ doctor.Options) []doctor.Result { return results },
	}
	err := cmd.Run(&Globals{})
	if !errors.Is(err, errSilentFailure) {
		t.Fatalf("err = %v, want errSilentFailure", err)
	}

	out := buf.String()
	for _, want := range []string{
		"[fail] node-extra-ca-certs",
		"fix: run `signatory certs init`",
		"[warn] github-token",
		"fix: export GITHUB_TOKEN=...",
		"status: NOT OK (1 fail, 1 warn)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestDoctorCmd_ProseRender_WarnsOnly: warns alone don't trip the
// non-zero exit (default), and the footer reads "OK with warnings"
// rather than "NOT OK".
func TestDoctorCmd_ProseRender_WarnsOnly(t *testing.T) {
	t.Parallel()

	results := []doctor.Result{
		{Name: "github-token", Status: doctor.StatusWarn, Message: "not set", Fix: "set it"},
	}
	var buf bytes.Buffer
	cmd := &DoctorCmd{
		Stdout: &buf,
		Runner: func(_ doctor.Options) []doctor.Result { return results },
	}
	if err := cmd.Run(&Globals{}); err != nil {
		t.Fatalf("warn-only run should succeed; got %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "status: OK with warnings") {
		t.Errorf("expected 'OK with warnings' footer, got:\n%s", got)
	}
}

// TestDoctorCmd_StrictPromotesWarn: --strict should turn a clean-
// of-fails-but-has-warns run into a non-zero exit. Without strict,
// the same input passes; that asymmetry is the point of the flag.
func TestDoctorCmd_StrictPromotesWarn(t *testing.T) {
	t.Parallel()

	results := []doctor.Result{
		{Name: "github-token", Status: doctor.StatusWarn, Message: "not set", Fix: "set it"},
	}
	cmd := &DoctorCmd{
		Stdout: &bytes.Buffer{},
		Strict: true,
		Runner: func(_ doctor.Options) []doctor.Result { return results },
	}
	err := cmd.Run(&Globals{})
	if !errors.Is(err, errSilentFailure) {
		t.Fatalf("--strict should fail on warn; got err=%v", err)
	}
}

// TestDoctorCmd_JSONReport: --json should emit a parseable
// DoctorReport, with the top-level status reflecting the worst
// probe outcome and Results preserved in input order.
func TestDoctorCmd_JSONReport(t *testing.T) {
	t.Parallel()

	results := []doctor.Result{
		{Name: "go-runtime", Status: doctor.StatusOK, Message: "go1.25.1"},
		{Name: "github-token", Status: doctor.StatusWarn, Message: "not set", Fix: "set it"},
	}
	var buf bytes.Buffer
	cmd := &DoctorCmd{
		Stdout: &buf,
		JSON:   true,
		Runner: func(_ doctor.Options) []doctor.Result { return results },
	}
	if err := cmd.Run(&Globals{}); err != nil {
		t.Fatalf("warn-only run should succeed; got %v", err)
	}

	var report DoctorReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON: %v\noutput:\n%s", err, buf.String())
	}
	if report.Status != "warn" {
		t.Errorf("top-level status = %q, want warn", report.Status)
	}
	if len(report.Results) != 2 || report.Results[0].Name != "go-runtime" {
		t.Errorf("results not preserved in order: %+v", report.Results)
	}
}

// TestDoctorCmd_JSONReport_FailStillEmits: when the run fails, JSON
// must still be written to stdout BEFORE returning the sentinel.
// Scripts piping to jq need parseable output even on non-zero
// exits — same convention `certs check --json` already uses.
func TestDoctorCmd_JSONReport_FailStillEmits(t *testing.T) {
	t.Parallel()

	results := []doctor.Result{
		{Name: "node-extra-ca-certs", Status: doctor.StatusFail, Message: "x", Fix: "y"},
	}
	var buf bytes.Buffer
	cmd := &DoctorCmd{
		Stdout: &buf,
		JSON:   true,
		Runner: func(_ doctor.Options) []doctor.Result { return results },
	}
	err := cmd.Run(&Globals{})
	if !errors.Is(err, errSilentFailure) {
		t.Fatalf("err = %v, want errSilentFailure", err)
	}

	var report DoctorReport
	if jerr := json.Unmarshal(buf.Bytes(), &report); jerr != nil {
		t.Fatalf("JSON not emitted on fail (%v):\n%s", jerr, buf.String())
	}
	if report.Status != "fail" {
		t.Errorf("status = %q, want fail", report.Status)
	}
}

// TestDoctorCmd_OnlyFlagPropagates: --only="a,b" should reach
// doctor.Options.Only as []string{"a","b"}, with whitespace trimmed.
func TestDoctorCmd_OnlyFlagPropagates(t *testing.T) {
	t.Parallel()

	var seen []string
	cmd := &DoctorCmd{
		Only:   "go-runtime , github-token",
		Stdout: &bytes.Buffer{},
		Runner: func(opts doctor.Options) []doctor.Result {
			seen = opts.Only
			return nil
		},
	}
	_ = cmd.Run(&Globals{})

	want := []string{"go-runtime", "github-token"}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("Only propagated as %v, want %v", seen, want)
	}
}

// TestDoctorCmd_PassesGlobalsAndStamps: the command must hand the
// version/commit/buildDate package vars and globals.DBPath through
// to doctor.Run. A regression here (e.g., somebody dropping a
// field during a refactor) would silently break binary-stamped and
// signatory-db reporting.
func TestDoctorCmd_PassesGlobalsAndStamps(t *testing.T) {
	t.Parallel()

	var captured doctor.Options
	cmd := &DoctorCmd{
		Stdout: &bytes.Buffer{},
		Runner: func(opts doctor.Options) []doctor.Result {
			captured = opts
			return nil
		},
	}
	_ = cmd.Run(&Globals{DBPath: "/tmp/sig.db"})

	if captured.Version != version || captured.Commit != commit || captured.BuildDate != buildDate {
		t.Errorf("stamps not propagated: %+v vs version=%q commit=%q built=%q",
			captured, version, commit, buildDate)
	}
	if captured.DBPath != "/tmp/sig.db" {
		t.Errorf("DBPath = %q, want /tmp/sig.db", captured.DBPath)
	}
	if captured.OpenStore == nil {
		t.Error("OpenStore seam was not wired — signatory-db open-check would skip silently")
	}
}

// TestDoctorCmd_OpenStoreSeam_ResolvesAndCloses: the wired
// OpenStore closure should resolve the path, open, ping, then
// close. We can't easily mock store.OpenSQLite, but we can
// confirm the closure errors on a deliberately bad path — that
// proves it's actually invoking store machinery and not a no-op.
func TestDoctorCmd_OpenStoreSeam_ErrorsOnBadPath(t *testing.T) {
	t.Parallel()

	var captured doctor.Options
	cmd := &DoctorCmd{
		Stdout: &bytes.Buffer{},
		Runner: func(opts doctor.Options) []doctor.Result {
			captured = opts
			return nil
		},
	}
	_ = cmd.Run(&Globals{DBPath: "/dev/null/cannot-open"})
	if captured.OpenStore == nil {
		t.Fatal("OpenStore seam not wired")
	}
	err := captured.OpenStore(context.Background(), "/dev/null/cannot-open")
	if err == nil {
		t.Error("expected an error opening /dev/null/cannot-open")
	}
}

// TestSplitOnly covers the comma-list parser edge cases. The CLI
// flag accepts loosely-formatted input (`--only="a, b ,c"`); that
// tolerance lives in splitOnly, so the regression surface is here.
func TestSplitOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want []string
	}{
		{in: "", want: nil},
		{in: "  ", want: nil},
		{in: "a", want: []string{"a"}},
		{in: "a,b,c", want: []string{"a", "b", "c"}},
		{in: " a , b , c ", want: []string{"a", "b", "c"}},
		{in: "a,,b", want: []string{"a", "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := splitOnly(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got)=%d want %d (%v vs %v)", len(got), len(tc.want), got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d]=%q want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
