// Package doctor implements the breadth-pass diagnostic behind
// `signatory doctor`. Each probe is a small pure function that
// inspects one piece of the local setup and returns a Result; Run
// iterates the registered probes (optionally filtered by Only) and
// returns their outputs in a stable order.
//
// Design notes:
//
//   - All probes are local and offline. No network calls. Doctor is
//     supposed to be fast and deterministic; flake from the network
//     would confound the signal it's meant to give.
//
//   - All external dependencies (env, filesystem, exec lookup) are
//     reachable through Options seams so each probe is unit-testable
//     without touching real OS state. Production callers pass
//     Options{} and rely on the nil-default fallbacks.
//
//   - Probes never panic on a missing seam; nil-default lookup is
//     centralized in resolveOptions so each probe body stays focused
//     on its check rather than on dependency wiring.
package doctor

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/sarahmaeve/signatory/internal/certs"
)

// Status is the outcome class for a single probe. Three values
// rather than two (pass/fail) because supply-chain trust setup has
// real "degraded but functional" states (e.g. GITHUB_TOKEN unset →
// the pipeline still runs, just with empty github signals) that
// deserve a distinct signal from outright failure.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

// Result is one probe's contribution to the doctor report. Fix is
// populated whenever Status != StatusOK; a probe that reports a
// problem without a remediation would leave the user worse off than
// no probe at all (same rule as certs.CheckResult).
type Result struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

// Options carries injection seams plus the filter list. All seams
// are nil-default — production callers pass an empty or near-empty
// Options{} and the defaults kick in via resolveOptions. Tests
// override individual seams.
type Options struct {
	// Only restricts Run to the named probes. Empty means "all".
	// Unknown names are silently skipped — the caller filters by
	// name and an unknown name simply matches nothing.
	Only []string

	// Build stamps. Injected by cmd/signatory/main.go from its
	// package vars (version, commit, buildDate) which are stamped
	// at install time via ldflags. Empty strings are tolerated and
	// surface as the "dev"/"none"/"unknown" defaults that signatory
	// already uses elsewhere.
	Version   string
	Commit    string
	BuildDate string

	// DBPath is the resolved sqlite database path the running
	// signatory process would use (already expanded by
	// store.ResolvePath). Empty means "use the default" — the probe
	// resolves the default itself rather than guessing in Options.
	DBPath string

	// PipelinePort is the port the pipeline-service probe checks.
	// Zero means "use the default (21517)" — same convention as
	// serve_lifecycle.
	PipelinePort int

	// Seams. Nil → resolveOptions plugs in the real implementation.
	Getenv      func(string) string
	LookPath    func(string) (string, error)
	GoVersion   func() string
	Stat        func(string) (os.FileInfo, error)
	UserHomeDir func() (string, error)
	Getwd       func() (string, error)
	ReadFile    func(string) ([]byte, error)
	Executable  func() (string, error)
	ProbePort   func(port int, timeout time.Duration) bool
	OpenStore   func(ctx context.Context, dbPath string) error
	CertsCheck  func() certs.CheckResult
}

// resolved is Options with every seam non-nil. Probes accept a
// resolved (not Options) so they don't each have to repeat nil
// checks. Run is the only place that calls resolveOptions.
type resolved struct {
	version   string
	commit    string
	buildDate string

	dbPath       string
	pipelinePort int

	getenv      func(string) string
	lookPath    func(string) (string, error)
	goVersion   func() string
	stat        func(string) (os.FileInfo, error)
	userHomeDir func() (string, error)
	getwd       func() (string, error)
	readFile    func(string) ([]byte, error)
	executable  func() (string, error)
	probePort   func(port int, timeout time.Duration) bool
	openStore   func(ctx context.Context, dbPath string) error
	certsCheck  func() certs.CheckResult
}

func resolveOptions(o Options) resolved {
	r := resolved{
		version:      o.Version,
		commit:       o.Commit,
		buildDate:    o.BuildDate,
		dbPath:       o.DBPath,
		pipelinePort: o.PipelinePort,
		getenv:       o.Getenv,
		lookPath:     o.LookPath,
		goVersion:    o.GoVersion,
		stat:         o.Stat,
		userHomeDir:  o.UserHomeDir,
		getwd:        o.Getwd,
		readFile:     o.ReadFile,
		executable:   o.Executable,
		probePort:    o.ProbePort,
		openStore:    o.OpenStore,
		certsCheck:   o.CertsCheck,
	}
	if r.pipelinePort == 0 {
		r.pipelinePort = 21517 // matches serve_lifecycle's default
	}
	if r.getenv == nil {
		r.getenv = os.Getenv
	}
	if r.lookPath == nil {
		r.lookPath = exec.LookPath
	}
	if r.goVersion == nil {
		r.goVersion = runtime.Version
	}
	if r.stat == nil {
		r.stat = os.Stat
	}
	if r.userHomeDir == nil {
		r.userHomeDir = os.UserHomeDir
	}
	if r.getwd == nil {
		r.getwd = os.Getwd
	}
	if r.readFile == nil {
		r.readFile = os.ReadFile
	}
	if r.executable == nil {
		r.executable = os.Executable
	}
	if r.probePort == nil {
		r.probePort = defaultProbePort
	}
	if r.certsCheck == nil {
		r.certsCheck = certs.Check
	}
	// openStore intentionally has no default: the doctor command
	// wires it from cmd/signatory/main.go where the store package
	// is already imported. Leaving it nil here means the
	// signatory-db probe degrades to a "skipped" Result rather
	// than panicking — preferable for any caller that uses
	// internal/doctor without supplying that seam.
	return r
}

// defaultProbePort is the production net.DialTimeout-backed port
// probe. Mirrors the probePort helper in serve_lifecycle.go but
// lives here so the doctor package doesn't depend on cmd/signatory.
func defaultProbePort(port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// probe is the registry entry: a stable name plus the function
// that produces the Result. Run iterates the registry, applies
// Only filtering, and returns the outputs in registry order so
// the renderer's output is stable across runs.
type probe struct {
	name string
	run  func(resolved) Result
}

// registry returns the canonical probe list in display order.
// Order is roughly "easiest signal to act on first": runtime →
// env → trust → wiring → store → service. As probes land in
// follow-up steps, append them here.
func registry() []probe {
	return []probe{
		{name: "go-runtime", run: probeGoRuntime},
		{name: "git-on-path", run: probeGitOnPath},
		{name: "binary-stamped", run: probeBinaryStamped},
		{name: "github-token", run: probeGitHubToken},
		{name: "home-signatory-dir", run: probeHomeSignatoryDir},
		{name: "node-extra-ca-certs", run: probeNodeExtraCACerts},
		{name: "mkcert-on-path", run: probeMkcertOnPath},
		{name: "mcp-config-present", run: probeMCPConfigPresent},
		{name: "mcp-binary-matches", run: probeMCPBinaryMatches},
		{name: "skills-present", run: probeSkillsPresent},
		{name: "signatory-db", run: probeSignatoryDB},
		{name: "pipeline-service", run: probePipelineService},
	}
}

// Run executes the probes selected by opts.Only (all when Only is
// empty) in registry order and returns their Results. It never
// returns an error: probe failures are encoded in Result.Status
// rather than as Go errors, so the caller can render a complete
// report even when several probes fail.
func Run(opts Options) []Result {
	r := resolveOptions(opts)
	probes := registry()

	// Only filter: build a set, then keep registry-order traversal
	// so output ordering is stable regardless of Only's order.
	var selected []probe
	if len(opts.Only) == 0 {
		selected = probes
	} else {
		want := make(map[string]struct{}, len(opts.Only))
		for _, n := range opts.Only {
			want[n] = struct{}{}
		}
		for _, p := range probes {
			if _, ok := want[p.name]; ok {
				selected = append(selected, p)
			}
		}
	}

	out := make([]Result, 0, len(selected))
	for _, p := range selected {
		out = append(out, p.run(r))
	}
	return out
}

// HasFail reports whether any Result in rs has Status == StatusFail.
// Convenience for the CLI's exit-code decision; doctor.Run itself
// stays neutral.
func HasFail(rs []Result) bool {
	for _, r := range rs {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

// HasWarn reports whether any Result in rs has Status == StatusWarn.
// Pairs with HasFail to drive the --strict promotion in the CLI.
func HasWarn(rs []Result) bool {
	for _, r := range rs {
		if r.Status == StatusWarn {
			return true
		}
	}
	return false
}
