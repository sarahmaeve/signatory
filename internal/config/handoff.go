package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// homeDir returns the current user's home directory. os.UserHomeDir
// is the canonical lookup; it consults $HOME on Unix and the usual
// profile env vars on Windows. Returns the empty string when
// resolution fails — callers should treat that as "don't expand".
func homeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// placeholderPattern matches any `{ALL_CAPS_IDENT}` token. The set of
// placeholders recognized by signatory's handoff templates is:
// TARGET_NAME, TARGET_URL, TARGET_PATH, TARGET_ROLE, ECOSYSTEM,
// INTAKE_QUESTION — but the renderer treats the token set as open:
// any `{SOMETHING}` in a template is subject to substitution.
// Unknown placeholders that have no mapping are left untouched so
// the author can see exactly which tokens still need values.
var placeholderPattern = regexp.MustCompile(`\{([A-Z][A-Z0-9_]*)\}`)

// RenderTemplate substitutes `{KEY}` occurrences in raw with values
// from subs. Placeholders not found in subs are left as-is in the
// output and reported in the returned unsubstituted slice so the
// caller can surface them to the user.
//
// Substitution is literal, not recursive: if a value itself contains
// `{…}` syntax, it is written through unchanged.
func RenderTemplate(raw []byte, subs map[string]string) (rendered []byte, unsubstituted []string) {
	seenUnsub := make(map[string]struct{})
	rendered = placeholderPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		key := string(match[1 : len(match)-1])
		if val, ok := subs[key]; ok {
			return []byte(val)
		}
		seenUnsub[key] = struct{}{}
		return match
	})
	unsubstituted = make([]string, 0, len(seenUnsub))
	for k := range seenUnsub {
		unsubstituted = append(unsubstituted, k)
	}
	sort.Strings(unsubstituted)
	return rendered, unsubstituted
}

// TargetKind classifies a user-supplied `<target>` argument so
// inference helpers know whether it's a repo URL or a local path.
type TargetKind int

const (
	// TargetURL is an HTTP/HTTPS URL, e.g., https://github.com/foo/bar.
	TargetURL TargetKind = iota

	// TargetPath is a filesystem path (absolute, home-tilde, or
	// relative-with-separator).
	TargetPath

	// TargetUnknown is anything we can't confidently bucket. Callers
	// should treat this as an error ("specify --url or --path").
	TargetUnknown
)

// urlSchemePattern matches the leading protocol of a URL-form target,
// case-insensitively (RFC 3986 §3.1: scheme is case-insensitive).
// Limited to http/https because that's what the handoff templates
// expect; git:// or ssh:// targets are unusual for signatory's
// fresh-agent handoffs and are rejected until a real use case shows
// up.
var urlSchemePattern = regexp.MustCompile(`(?i)^https?://`)

// ClassifyTarget buckets a raw target argument so the handoff
// renderer can decide which placeholder slot to populate.
//
// Heuristics, in order:
//   - An http:// or https:// scheme → URL.
//   - A leading /, ~, ./, or ../ → filesystem path.
//   - A slash-containing string with NO colon → relative path.
//     (This catches `subdir/project` while rejecting scheme-form
//     URLs like `git://…` and scp-form `git@host:path` that both
//     include a colon.)
//   - Anything else → unknown; caller must disambiguate with flags.
func ClassifyTarget(target string) TargetKind {
	switch {
	case urlSchemePattern.MatchString(target):
		return TargetURL
	case strings.HasPrefix(target, "/"),
		strings.HasPrefix(target, "~"),
		strings.HasPrefix(target, "./"),
		strings.HasPrefix(target, "../"):
		return TargetPath
	case strings.ContainsRune(target, filepath.Separator) && !strings.ContainsRune(target, ':'):
		return TargetPath
	default:
		return TargetUnknown
	}
}

// InferNameFromURL extracts a short human-readable package name from
// a GitHub/GitLab-style URL. "https://github.com/foo/bar" becomes
// "bar"; ".git" suffixes are stripped. Returns the empty string when
// no usable name can be derived OR when the derived name would be
// unsafe to use as a path component (".", "..", contains a separator,
// or contains a NUL byte).
//
// This rejection is intentionally inside the helper rather than at
// each call site: a future consumer that uses the returned name to
// build a filesystem path could otherwise forget the containment
// check. Centralizing the rule means callers can trust the output as
// a safe path-component-shaped string. (Reviewer F2: helpers should
// own their own contract.)
func InferNameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	last := parts[len(parts)-1]
	last = strings.TrimSuffix(last, ".git")
	if !isSafePathComponent(last) {
		return ""
	}
	return last
}

// InferNameFromPath returns the basename of a path as a best-effort
// project name. Returns the empty string when the basename would be
// unsafe to use as a path component (same contract as InferNameFromURL).
func InferNameFromPath(path string) string {
	base := filepath.Base(strings.TrimRight(path, string(filepath.Separator)))
	if !isSafePathComponent(base) {
		return ""
	}
	return base
}

// isSafePathComponent returns true when name is suitable for use as
// a single path component without further validation. Centralizing
// this check makes the rule explicit and easy to extend (e.g., reject
// reserved Windows names, or limit length, if needed later).
func isSafePathComponent(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsRune(name, 0x00) {
		return false
	}
	// Reject any platform's path separator. We use ContainsAny rather
	// than filepath.Separator because the value may have come from a
	// URL parsed on a system different from the one consuming it.
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	return true
}

// HandoffSubstitutions builds the {PLACEHOLDER: value} map a caller
// feeds into RenderTemplate. It applies the documented inference
// rules — classify target, fill TARGET_URL or TARGET_PATH and
// TARGET_NAME from it — then overlays any explicit overrides.
//
// Empty values are NOT set in the returned map. This is deliberate:
// leaving a placeholder absent causes RenderTemplate to keep the
// `{KEY}` token literal AND report it in the unsubstituted list so
// the user sees which values still need filling in. Pre-filling
// empty strings would silently erase placeholders and produce broken
// handoffs.
func HandoffSubstitutions(target string, overrides HandoffOverrides) (map[string]string, error) {
	subs := make(map[string]string)

	switch ClassifyTarget(target) {
	case TargetURL:
		subs["TARGET_URL"] = target
		// InferNameFromURL guarantees a safe path-component or "".
		if name := InferNameFromURL(target); name != "" {
			subs["TARGET_NAME"] = name
		}
	case TargetPath:
		// Expand tilde so the template receives a path the agent can
		// actually navigate. We deliberately do NOT resolve symlinks
		// or check existence — that's the user's responsibility.
		expanded := expandTilde(target)
		subs["TARGET_PATH"] = expanded
		// InferNameFromPath guarantees a safe path-component or "".
		if name := InferNameFromPath(expanded); name != "" {
			subs["TARGET_NAME"] = name
		}
	case TargetUnknown:
		// Accept the raw string; require caller to disambiguate via
		// --url / --path overrides, which run below.
	}

	setIfPresent(subs, "TARGET_NAME", overrides.Name)
	setIfPresent(subs, "TARGET_URL", overrides.URL)
	setIfPresent(subs, "TARGET_PATH", overrides.Path)
	setIfPresent(subs, "TARGET_ROLE", overrides.Role)
	setIfPresent(subs, "ECOSYSTEM", overrides.Ecosystem)
	setIfPresent(subs, "INTAKE_QUESTION", overrides.Intake)

	// TARGET_VERSION is always set (never absent), so the
	// templates can rely on the placeholder being filled.
	// Empty Version → "(HEAD of default branch)" so the analyst
	// reading the handoff sees explicitly that no version was
	// pinned, rather than a literal "{TARGET_VERSION}" leaking
	// into prose. Non-empty Version → the version verbatim.
	if overrides.Version != "" {
		subs["TARGET_VERSION"] = overrides.Version
	} else {
		subs["TARGET_VERSION"] = "(HEAD of default branch)"
	}

	// Name is the universal placeholder — if we couldn't derive one
	// and the user didn't supply one, error out so the render doesn't
	// produce a handoff with `{TARGET_NAME}` as an agent-visible
	// literal.
	if _, ok := subs["TARGET_NAME"]; !ok {
		return nil, fmt.Errorf("TARGET_NAME could not be inferred from %q; pass --name", target)
	}
	return subs, nil
}

// setIfPresent writes key=val into m only when val is non-empty.
// This is the "empty overrides don't clobber inference" rule.
func setIfPresent(m map[string]string, key, val string) {
	if val != "" {
		m[key] = val
	}
}

// HandoffOverrides carries explicit placeholder values from CLI flags.
// Each field overrides the corresponding inferred value; empty fields
// preserve the inference. This struct is flat and field-per-placeholder
// on purpose — a generic map would lose compile-time checking of
// which keys signatory's renderer understands.
type HandoffOverrides struct {
	Name      string
	URL       string
	Path      string
	Role      string
	Ecosystem string
	Intake    string

	// Version is the requested git ref (tag/branch) for URL
	// targets — populated from the @version suffix on the
	// user's input target (e.g., `github.com/X/Y@v1.0.0`).
	// Threads through to the handoff template's TARGET_VERSION
	// placeholder so the analyst agent (and the synthesist's
	// version_scope field) name the version that was actually
	// cloned. Empty signals HEAD-of-default-branch and renders
	// as "(HEAD of default branch)" in the template body.
	Version string
}

// expandTilde converts a leading "~" or "~/" into the caller's home
// directory using $HOME. If $HOME is unset, the input is returned
// unchanged — the resulting template path will be wrong, but the
// caller (typically an agent running in a shell) can still parse it.
func expandTilde(path string) string {
	if path == "~" {
		if home := homeDir(); home != "" {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home := homeDir(); home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
