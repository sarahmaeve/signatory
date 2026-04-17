package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
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
		return nil, fmt.Errorf("git %v: %w", args, err)
	}

	full := make([]string, 0, len(args)+2)
	full = append(full, "-C", repoPath)
	full = append(full, args...)

	//nolint:gosec // G204: argv-form exec of a string-literal binary ("git"); all positional args are caller-controlled but never shell-interpreted
	cmd := exec.CommandContext(ctx, "git", full...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %v: %w: %s",
			args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}
