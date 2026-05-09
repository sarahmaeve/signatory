package certs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrMkcertNotFound is returned by Init when `mkcert` is not on
// PATH. The CLI wraps this with an install hint (`brew install
// mkcert` on macOS) because the remediation is the same every
// time and the user shouldn't have to match error strings to
// figure that out.
var ErrMkcertNotFound = errors.New("mkcert not found on PATH")

// InitOptions drives Init. CertDir defaults to DefaultCertDir
// (~/.signatory/certs) when empty; pass an explicit path via
// --cert-dir to override. Stderr receives progress lines; pass
// io.Discard in tests or quiet callers.
type InitOptions struct {
	CertDir string
	Stderr  io.Writer
}

// InitResult reports what Init did. The Actions slice is a
// human-readable audit trail ("copied CA from …", "created
// directory …") suitable for printing verbatim.
type InitResult struct {
	CertDir string
	CAPath  string
	Actions []string
}

// Init copies mkcert's root CA into a stable, signatory-owned
// location so NODE_EXTRA_CA_CERTS can point at a path that
// doesn't move when mkcert updates or relocates its CAROOT.
//
// Idempotent: safe to re-run. If mkcert has rotated its CA, the
// managed copy is refreshed; if not, Init is effectively a no-op
// (files are rewritten with the same bytes, so mtime drifts but
// the trust chain stays consistent).
//
// Init does NOT generate the localhost server cert — that's
// `mkcert 127.0.0.1 localhost` invoked by `serve start` or a
// separate setup step. This function's sole responsibility is
// making the CA trust file reachable at a stable path, which is
// what NODE_EXTRA_CA_CERTS needs.
func Init(opts InitOptions) (*InitResult, error) {
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	certDir := opts.CertDir
	if certDir == "" {
		certDir = DefaultCertDir
	}
	certDirResolved, err := expandHome(certDir)
	if err != nil {
		return nil, fmt.Errorf("resolve cert dir %q: %w", certDir, err)
	}

	result := &InitResult{
		CertDir: certDirResolved,
		CAPath:  filepath.Join(certDirResolved, CAFileName),
	}

	// Locate mkcert's CA bundle. Tests inject a fixture path via
	// setMkcertCAForTest so they don't require mkcert on PATH.
	sourceCA, err := mkcertCARoot()
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(opts.Stderr, "mkcert CA: %s\n", sourceCA)

	if err := os.MkdirAll(certDirResolved, 0o750); err != nil {
		return nil, fmt.Errorf("create cert dir %q: %w", certDirResolved, err)
	}
	result.Actions = append(result.Actions,
		fmt.Sprintf("ensured cert dir %s", certDirResolved))

	// Copy — unconditionally, every run. Treating this as a
	// refresh rather than a one-shot install means mkcert
	// reinstalls propagate to signatory's managed copy on the
	// next `certs init` without the user having to know that
	// the divergence is what's breaking TLS.
	if err := copyFile(sourceCA, result.CAPath); err != nil {
		return nil, fmt.Errorf("copy CA %s → %s: %w", sourceCA, result.CAPath, err)
	}
	result.Actions = append(result.Actions,
		fmt.Sprintf("copied CA %s → %s", sourceCA, result.CAPath))
	_, _ = fmt.Fprintf(opts.Stderr, "wrote %s\n", result.CAPath)

	return result, nil
}

// copyFile writes src's contents to dst with 0o600 mode. We use
// 0o600 because the file is per-user trust material; anything
// wider is unnecessary and triggers lints. The write happens via
// a temp file + rename for atomicity — a crash mid-copy leaves
// the previous valid CA in place rather than a truncated file
// that would silently break TLS on next handshake.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: path from mkcert discovery, trusted
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil { //nolint:gosec // G703: dst flows from operator --cert-dir flag (resolved via filepath.Join with const filename), not network/user input
		return fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Best-effort cleanup — we already failed, nothing useful
		// to do with a second error.
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp to dst: %w", err)
	}
	return nil
}

// mkcertCARoot discovers the mkcert-managed rootCA.pem path on the
// current machine. Overridden by setMkcertCAForTest in tests so the
// package doesn't require mkcert on the test-runner's PATH.
//
// Package-level var for the same reason killFn exists in
// serve_lifecycle.go: tests need to swap the implementation without
// mutating real OS state, and interfaces are overkill for a single
// hook.
var mkcertCARoot = defaultMkcertCARoot

func defaultMkcertCARoot() (string, error) {
	bin, err := exec.LookPath("mkcert")
	if err != nil {
		return "", ErrMkcertNotFound
	}
	// `mkcert -CAROOT` prints the directory containing the CA
	// files, not the file itself. It's a fast, deterministic
	// invocation — no network, no state change.
	out, err := exec.Command(bin, "-CAROOT").Output() //nolint:gosec // G204: bin resolved from LookPath, no user input
	if err != nil {
		return "", fmt.Errorf("run `mkcert -CAROOT`: %w", err)
	}
	caDir := strings.TrimSpace(string(out))
	if caDir == "" {
		return "", errors.New("mkcert -CAROOT returned empty output")
	}
	caPath := filepath.Join(caDir, "rootCA.pem")
	if _, err := os.Stat(caPath); err != nil {
		return "", fmt.Errorf("mkcert CA not found at %s (run `mkcert -install` first): %w", caPath, err)
	}
	return caPath, nil
}

// setMkcertCAForTest installs fn as the mkcert discovery hook and
// returns a restore function. Tests should always schedule the
// restore via t.Cleanup to prevent cross-test pollution.
//
// Lives alongside production code (not in a _test.go file) because
// it's a small, clearly-named seam and keeping it here avoids
// having to expose mkcertCARoot itself. Production code does not
// call this function.
func setMkcertCAForTest(fn func() (string, error)) func() {
	orig := mkcertCARoot
	mkcertCARoot = fn
	return func() { mkcertCARoot = orig }
}
