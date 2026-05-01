package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/sarahmaeve/signatory/internal/certs"
)

// errSilentFailure is returned by diagnostic-style commands (so far:
// certs doctor) that have already printed their complete output to
// stdout and need main.go to drive a non-zero exit without a
// duplicate stderr line. Same role as errStatusNotRunning.
var errSilentFailure = errors.New("signatory: silent failure")

// CertsCmd groups the certificate-management verbs. Each subcommand
// solves a concrete leg of the NODE_EXTRA_CA_CERTS reliability
// problem that drove this command into existence:
//
//   - check    — non-interactive preflight for scripts and skills
//   - init     — install the managed CA at a stable path (opt-in
//     persist via --write-profile)
//   - doctor   — verbose diagnostic with full context for humans
//
// Shape follows ServeCmd / PostureCmd: a pure dispatcher struct
// whose fields are the individual subcommand structs.
type CertsCmd struct {
	Check  CertsCheckCmd  `cmd:"" help:"Preflight: verify NODE_EXTRA_CA_CERTS is set and points to a valid CA. Exits 0 on success, non-zero with a remediation hint on failure."`
	Init   CertsInitCmd   `cmd:"" help:"Install the managed CA at a stable path and optionally append a managed export block to your shell profile."`
	Doctor CertsDoctorCmd `cmd:"" help:"Verbose diagnostic. Prints env, resolved paths, mkcert state, and remediation — useful when check fails and you want more context."`
}

// --- check -----------------------------------------------------------------

// CertsCheckCmd is the preflight. Designed for shell pipelines:
//
//	signatory certs check || signatory certs init --write-profile
//
// No flags by design — if you need more surface, use `doctor` (which
// can grow options without complicating the scriptable path). Minimum
// surface area here keeps the check cheap and predictable.
type CertsCheckCmd struct{}

func (cmd *CertsCheckCmd) Run(_ *Globals) error {
	r := certs.Check()
	if r.OK {
		// One-line "everything's fine" confirmation on stdout. Quiet
		// callers can redirect stdout to /dev/null; error output is
		// suppressed because there is no error.
		fmt.Println(r.Message)
		return nil
	}
	// Build a compact two-line error: the failure message on one
	// line, the fix hint on the next (prefixed so the user's eye
	// lands on the remediation). main.go prints the returned error
	// to stderr and exits with exitCodeFor — no extra stderr work
	// needed here.
	if r.Fix != "" {
		return fmt.Errorf("%s\nfix: %s", r.Message, r.Fix)
	}
	return errors.New(r.Message)
}

// --- init ------------------------------------------------------------------

// CertsInitCmd copies mkcert's CA into the signatory-owned cert
// directory so NODE_EXTRA_CA_CERTS can point at a stable path, and
// optionally persists the export in the user's shell profile.
//
// --write-profile is deliberately opt-in: writing to a user's
// dotfiles without consent is rude. The default behavior is to
// print the export line so the user can paste it or re-run with
// the flag.
type CertsInitCmd struct {
	CertDir          string `help:"Directory to install the managed CA into." default:"~/.signatory/certs" type:"path"`
	WriteProfile     bool   `help:"Append a managed export block to the shell profile so NODE_EXTRA_CA_CERTS survives terminal restarts. Opt-in." name:"write-profile"`
	ShellProfilePath string `help:"Shell profile to update when --write-profile is set. Default is ~/.zshrc." default:"~/.zshrc" type:"path" name:"shell-profile-path"`
}

func (cmd *CertsInitCmd) Run(_ *Globals) error {
	out := os.Stderr

	result, err := certs.Init(certs.InitOptions{
		CertDir: cmd.CertDir,
		Stderr:  out,
	})
	if err != nil {
		if errors.Is(err, certs.ErrMkcertNotFound) {
			return fmt.Errorf("mkcert not found on PATH\n" +
				"fix: install mkcert (`brew install mkcert` on macOS), then run `mkcert -install` to add the CA to your system trust store, then re-run this command")
		}
		return fmt.Errorf("install managed CA: %w", err)
	}

	// Optional: persist the env var in the shell profile.
	if cmd.WriteProfile {
		pr, err := certs.WriteProfile(certs.WriteProfileOptions{
			ProfilePath: cmd.ShellProfilePath,
			CAPath:      result.CAPath,
		})
		if err != nil {
			return fmt.Errorf("update shell profile %q: %w", cmd.ShellProfilePath, err)
		}
		switch pr.Action {
		case certs.ProfileCreated:
			fmt.Fprintf(out, "shell profile: created %s with managed block\n", pr.ProfilePath)
		case certs.ProfileAppended:
			fmt.Fprintf(out, "shell profile: appended managed block to %s\n", pr.ProfilePath)
		case certs.ProfileReplaced:
			fmt.Fprintf(out, "shell profile: replaced existing managed block in %s\n", pr.ProfilePath)
		case certs.ProfileUnchanged:
			fmt.Fprintf(out, "shell profile: %s already up to date\n", pr.ProfilePath)
		}
		fmt.Fprintln(out, "restart your terminal (or `source "+pr.ProfilePath+"`) so "+certs.EnvVar+" is exported in new shells")
		return nil
	}

	// --write-profile not set: print the export line to stdout so
	// the user can eval or paste it. Stderr carries the explanation;
	// stdout stays clean for `eval "$(signatory certs init)"` use.
	fmt.Fprintf(out, "\nmanaged CA at %s\n", result.CAPath)
	fmt.Fprintf(out, "add the following line to your shell profile (or re-run with --write-profile):\n\n")
	fmt.Printf("export %s=%q\n", certs.EnvVar, result.CAPath)
	return nil
}

// --- doctor ----------------------------------------------------------------

// CertsDoctorCmd is the verbose diagnostic. Exit code matches
// Check (0 if healthy, non-zero if not) so it can be used as a
// drop-in replacement in scripts that want more context.
type CertsDoctorCmd struct{}

func (cmd *CertsDoctorCmd) Run(_ *Globals) error {
	r := certs.Check()
	fmt.Println("signatory cert diagnostic")
	fmt.Println(strings.Repeat("─", 40))
	fmt.Printf("env %s: %s\n", certs.EnvVar, displayEnv(r.Env))
	fmt.Printf("resolved path: %s\n", displayPath(r.CAPath))

	// mkcert diagnostic — independent of Check's scope, useful when
	// the user's env is unset but we want to confirm mkcert itself
	// is reachable.
	if path, err := exec.LookPath("mkcert"); err == nil {
		fmt.Printf("mkcert: %s\n", path)
		if out, err := exec.Command(path, "-CAROOT").Output(); err == nil { //nolint:gosec // G204: path resolved from LookPath, no user input
			fmt.Printf("mkcert CAROOT: %s\n", strings.TrimSpace(string(out)))
		} else {
			fmt.Printf("mkcert -CAROOT failed: %v\n", err)
		}
	} else {
		fmt.Println("mkcert: NOT FOUND on PATH")
	}

	fmt.Println(strings.Repeat("─", 40))
	if r.OK {
		fmt.Println("status: OK")
		fmt.Println(r.Message)
		return nil
	}
	fmt.Println("status: NOT OK")
	fmt.Println(r.Message)
	if r.Fix != "" {
		fmt.Printf("fix: %s\n", r.Fix)
	}
	// errSilentFailure is recognized by main.go; it drives a non-zero
	// exit without re-echoing anything to stderr (we've already
	// printed the complete diagnostic above). Same pattern as
	// errStatusNotRunning.
	return errSilentFailure
}

// displayEnv renders an env value for human display, making
// empty/unset visually obvious so a quick glance distinguishes
// "variable is empty" from "variable contains a path."
func displayEnv(v string) string {
	if v == "" {
		return "(unset)"
	}
	return v
}

// displayPath similarly normalizes empty output.
func displayPath(p string) string {
	if p == "" {
		return "(n/a — env unset)"
	}
	return p
}
