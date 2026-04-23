package certs

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckResult is the outcome of a preflight check. Callers
// (signatory serve start, signatory certs check) map OK=true to
// exit 0 and OK=false to a non-zero exit plus the printed Message
// and Fix. Fix is always populated on failure — a preflight that
// reports a problem without a remediation would leave the user
// worse off than no preflight at all.
type CheckResult struct {
	OK      bool
	Env     string   // raw NODE_EXTRA_CA_CERTS value as seen by this process
	CAPath  string   // expanded/absolute resolution of Env (empty if Env was empty)
	Code    FailCode
	Message string // one-line status (OK or concrete failure)
	Fix     string // remediation hint; empty when OK
}

// Check validates the current process env. It is non-interactive
// and side-effect-free — safe to call from serve start, from a
// CI job, or from a skill orchestrator.
func Check() CheckResult { return CheckWithEnv(os.Getenv) }

// CheckWithEnv is the seam the tests drive. Production callers use
// Check; tests pass a synthesized env lookup so they don't depend on
// whatever NODE_EXTRA_CA_CERTS is set to in the runner's environment.
func CheckWithEnv(getenv func(string) string) CheckResult {
	raw := strings.TrimSpace(getenv(EnvVar))
	if raw == "" {
		return CheckResult{
			Code:    FailEnvUnset,
			Message: fmt.Sprintf("%s is not set — Claude Code WebFetch cannot verify the pipeline service's TLS cert", EnvVar),
			Fix:     "run `signatory certs init --write-profile` to install the CA and persist the env var, then restart your terminal",
		}
	}

	path, err := expandHome(raw)
	if err != nil {
		// expandHome only fails if the user has no HOME; exceptional.
		return CheckResult{
			Env:     raw,
			CAPath:  raw,
			Code:    FailPathMissing,
			Message: fmt.Sprintf("cannot resolve %s=%q: %v", EnvVar, raw, err),
			Fix:     "set NODE_EXTRA_CA_CERTS to an absolute path (no `~/`) or ensure $HOME is set",
		}
	}

	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		return CheckResult{
			Env:     raw,
			CAPath:  path,
			Code:    FailPathMissing,
			Message: fmt.Sprintf("%s=%s but that file does not exist", EnvVar, path),
			Fix:     "run `signatory certs init` to regenerate the managed CA at this path, or point the env var at a real mkcert rootCA.pem",
		}
	case err != nil:
		return CheckResult{
			Env:     raw,
			CAPath:  path,
			Code:    FailPathInvalid,
			Message: fmt.Sprintf("%s=%s: stat failed: %v", EnvVar, path, err),
			Fix:     "check file permissions on the CA path and the directories leading to it",
		}
	case info.IsDir():
		return CheckResult{
			Env:     raw,
			CAPath:  path,
			Code:    FailPathInvalid,
			Message: fmt.Sprintf("%s=%s is a directory — the env var must point at rootCA.pem, not its parent", EnvVar, path),
			Fix:     fmt.Sprintf("set %s to %s/rootCA.pem (or run `signatory certs init`)", EnvVar, path),
		}
	}

	// File exists; verify it at least superficially looks like a PEM
	// certificate. We don't validate the signature chain or expiry —
	// that's Node's job at handshake time, and a false-positive here
	// (truncated PEM that pem.Decode accepts) is far less painful
	// than a false-negative that fences off a valid CA.
	data, err := os.ReadFile(path) //nolint:gosec // G304: path resolved from user-controlled env, already stat'd, scope is their own CA bundle
	if err != nil {
		return CheckResult{
			Env:     raw,
			CAPath:  path,
			Code:    FailPathInvalid,
			Message: fmt.Sprintf("%s=%s: read failed: %v", EnvVar, path, err),
			Fix:     "ensure the CA file is readable by your user (`chmod 0644 " + path + "`)",
		}
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return CheckResult{
			Env:     raw,
			CAPath:  path,
			Code:    FailPathInvalid,
			Message: fmt.Sprintf("%s=%s is not a PEM-encoded CERTIFICATE block", EnvVar, path),
			Fix:     "regenerate the managed copy with `signatory certs init` — your CA file is truncated or the wrong format",
		}
	}

	return CheckResult{
		OK:      true,
		Env:     raw,
		CAPath:  path,
		Code:    StatusOK,
		Message: fmt.Sprintf("%s=%s (valid CERTIFICATE block)", EnvVar, path),
	}
}

// expandHome resolves a leading `~/` or bare `~` to the user's home
// directory. Returns the input unchanged if it doesn't start with `~`.
// Absolute paths round-trip cleanly.
func expandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if p == "~" {
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), nil
	}
	// `~otheruser/...` is not supported — it's not a shape the
	// managed env export ever produces, and Node wouldn't expand
	// it either. Leave as-is so the caller sees FailPathMissing
	// and gets a clear remediation.
	return p, nil
}
