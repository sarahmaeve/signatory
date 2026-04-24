package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/sarahmaeve/signatory/internal/gitenv"
)

// runGit executes `git -C <repoPath> <args...>` with the supplied
// context. On success it returns stdout as a byte slice; on
// failure it returns an error wrapping both the exec failure and
// the captured stderr text.
//
// Security notes:
//
//   - The binary name "git" is a string literal, not a variable.
//     gosec's G204 rule fires on variables, not literals.
//   - Arguments are passed as a variadic list (argv form), never
//     concatenated into a shell string. There is no shell
//     interpretation, no glob expansion, no quote handling —
//     each element is one argv slot exactly.
//   - The repoPath is a path supplied by the Collector (which
//     in turn gets it from the caller, validated at the
//     signatory-analyze layer). Passing a user-controlled path
//     to `git -C` is safe: git treats the path as a chdir
//     target, not as a command.
//   - cmd.Env is set to gitenv.SafeEnv(), which strips GIT_DIR,
//     GIT_CONFIG_*, GIT_SSH_COMMAND, and the rest of the
//     documented config-injection / redirection interface.
//     Without this, an ambient GIT_DIR would override the -C
//     flag's scope and cause every signal this collector emits
//     to be collected from the wrong repository — a silent
//     trust-model violation (attribution without grounding).
//     The 2026-04-24 postmortem traced shared-config corruption
//     to exactly this class of vector in the sibling test helpers.
//
// Stderr is captured, not streamed, so tests and callers can
// inspect the exact git error message. On an empty repo or a
// missing branch, git writes a terse one-line error to stderr and
// exits non-zero — that output is preserved verbatim in the
// returned error.
func runGit(ctx context.Context, repoPath string, args ...string) ([]byte, error) {
	// Fail fast on an already-cancelled context. exec.CommandContext
	// only kills on context-done *after* the process starts, so a
	// fast command on a pre-cancelled context would otherwise race
	// to completion before the kill signal arrives. Checking ctx.Err()
	// here makes cancellation deterministic.
	if err := ctx.Err(); err != nil {
		return nil, gitError(args, err, nil)
	}

	full := make([]string, 0, len(args)+2)
	full = append(full, "-C", repoPath)
	full = append(full, args...)

	//nolint:gosec // G204: argv-form exec of a string-literal binary ("git"); all positional args are caller-controlled but never shell-interpreted; env sanitized by gitenv.SafeEnv
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitenv.SafeEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, gitError(args, err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}

// gitError formats runGit's two failure paths (pre-exec context
// cancellation and post-Run subprocess failure) into one consistent
// shape so callers aggregating log lines or string-matching against
// f.Reason in collector failures see the same format regardless of
// which path produced the error:
//
//	"git [args]: <err>"                 when stderr is empty
//	"git [args]: <err>: <trimmed stderr>"  otherwise
//
// The %w verb preserves errors.Is/As identity for the underlying
// cause (context.Canceled, *exec.ExitError, etc.) in both paths.
func gitError(args []string, err error, stderr []byte) error {
	s := bytes.TrimSpace(stderr)
	if len(s) == 0 {
		return fmt.Errorf("git %v: %w", args, err)
	}
	return fmt.Errorf("git %v: %w: %s", args, err, s)
}
