package doctor

import (
	"fmt"
	"strconv"
	"strings"
)

// probeGoRuntime checks the running Go runtime version. Signatory's
// minimum is Go 1.24 (per CLAUDE.md); 1.25+ is the active target.
//
// We probe runtime.Version() rather than the toolchain that built
// the binary: the question we're trying to answer is "is this
// process running on a Go runtime with the language semantics we
// rely on" (errors.Join, slog, the loopvar fix). For a stamped
// release binary, runtime.Version() and the build toolchain agree;
// for a `go run` during development it's whatever the user has on
// PATH.
//
// Pre-release suffixes ("go1.25rc1") count as that minor version.
// Unparseable strings are warn — we'd rather flag the oddity than
// silently accept a future release naming change as "fine."
func probeGoRuntime(r resolved) Result {
	const (
		minMajor, minMinor   = 1, 24 // hard floor (CLAUDE.md)
		goodMajor, goodMinor = 1, 25 // current target
	)

	v := r.goVersion()
	major, minor, ok := parseGoVersion(v)
	if !ok {
		return Result{
			Name:    "go-runtime",
			Status:  StatusWarn,
			Message: fmt.Sprintf("could not parse Go runtime version %q", v),
			Fix:     "verify your Go install with `go version`; signatory targets Go 1.25+",
		}
	}

	switch {
	case major < minMajor || (major == minMajor && minor < minMinor):
		return Result{
			Name:    "go-runtime",
			Status:  StatusFail,
			Message: fmt.Sprintf("%s is below the minimum supported runtime (Go %d.%d)", v, minMajor, minMinor),
			Fix:     fmt.Sprintf("upgrade to Go %d.%d or newer", goodMajor, goodMinor),
		}
	case major == minMajor && minor == minMinor:
		return Result{
			Name:    "go-runtime",
			Status:  StatusWarn,
			Message: fmt.Sprintf("%s meets the minimum but the active target is Go %d.%d+", v, goodMajor, goodMinor),
			Fix:     fmt.Sprintf("upgrade to Go %d.%d or newer for full feature parity", goodMajor, goodMinor),
		}
	default:
		return Result{
			Name:    "go-runtime",
			Status:  StatusOK,
			Message: v,
		}
	}
}

// parseGoVersion extracts (major, minor) from a runtime.Version()
// style string ("go1.25.1", "go1.25rc1", "go1.30.0"). Returns
// ok=false for anything that doesn't start with "go" followed by
// a major.minor pair — including the "devel +<hash>" form that
// gotip and unstamped builds emit, which we deliberately treat as
// "unknown, please investigate" rather than guessing.
func parseGoVersion(v string) (major, minor int, ok bool) {
	rest, found := strings.CutPrefix(v, "go")
	if !found {
		return 0, 0, false
	}
	// Split on the first '.' for major; for minor, take the run of
	// digits at the start (so "25rc1" → 25, "25.1" → 25). Trailing
	// patch / pre-release content is irrelevant to the band check.
	majorStr, minorRest, found := strings.Cut(rest, ".")
	if !found {
		return 0, 0, false
	}
	major, err := strconv.Atoi(majorStr)
	if err != nil {
		return 0, 0, false
	}
	end := 0
	for end < len(minorRest) && minorRest[end] >= '0' && minorRest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(minorRest[:end])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// probeBinaryStamped reports whether the running binary carries
// real ldflags-stamped version + commit values, vs. the bland
// defaults the package vars hold when `go install` / `go build`
// is used directly. TROUBLESHOOTING calls this out: a `dev` /
// `none` binary is functional but invisible — drift between
// running binary and source tree is silent until something weird
// happens. Warn (not fail): `make install` is the fix, but the
// CLI works without it.
func probeBinaryStamped(r resolved) Result {
	stub := r.version == "" || r.version == "dev" || r.commit == "" || r.commit == "none"
	if stub {
		return Result{
			Name:    "binary-stamped",
			Status:  StatusWarn,
			Message: fmt.Sprintf("binary is unstamped (version=%q commit=%q)", coalesce(r.version, "dev"), coalesce(r.commit, "none")),
			Fix:     "rebuild with `make install` so version + commit + buildDate are stamped via ldflags",
		}
	}
	return Result{
		Name:    "binary-stamped",
		Status:  StatusOK,
		Message: fmt.Sprintf("version=%s commit=%s built=%s", r.version, r.commit, coalesce(r.buildDate, "unknown")),
	}
}

// coalesce returns s unless it's empty, in which case it returns
// fallback. Used for human-display defaults — "dev" / "none" /
// "unknown" are the strings signatory's main.go already shows for
// unstamped builds, and the doctor message reads more naturally if
// the same vocabulary is used.
func coalesce(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// probeGitOnPath confirms `git` is reachable. QUICKSTART lists it as
// a hard prerequisite (collectors clone repos, version-stamp drift
// detection reads HEAD, etc.) so a missing git is fail, not warn.
//
// Echoing the resolved path on success helps a user spot the
// "wrong git winning $PATH" mode (e.g., a Homebrew install
// shadowed by an old Xcode CLT git, or vice versa).
func probeGitOnPath(r resolved) Result {
	path, err := r.lookPath("git")
	if err != nil {
		return Result{
			Name:    "git-on-path",
			Status:  StatusFail,
			Message: "git not found on PATH",
			Fix:     "install git (`brew install git` on macOS, distro package manager on Linux)",
		}
	}
	return Result{
		Name:    "git-on-path",
		Status:  StatusOK,
		Message: path,
	}
}
