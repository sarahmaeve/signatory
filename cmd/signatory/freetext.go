package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// readFreeText reconciles the common pattern of accepting a
// multi-line value via EITHER an inline --<name> flag OR a
// --<name>-file path (with "-" meaning stdin). Implements
// agent-facing-contract §3.4: file form is primary, flag form is a
// one-liner convenience, both-set is an error.
//
// Rules:
//
//   - If both inline and file are non-empty → return an error naming
//     the conflict so the caller picks one.
//   - If file is "-" → read stdin in full.
//   - If file is a path → read the file; strip the trailing newline
//     a text editor typically leaves; everything else is verbatim.
//   - If inline is set → validate that it contains no newline (the
//     whole point of the file form is multi-line; an inline value
//     with a newline is almost certainly a shell-quoting bug we
//     should surface loudly rather than silently accept).
//   - If neither is set → return ("", nil). The caller decides
//     whether empty is valid in its context.
//
// name is the semantic label (e.g. "rationale") used in error
// messages so the caller sees "rationale" rather than a generic
// "free-text field."
func readFreeText(name, inline, file string) (string, error) {
	if inline != "" && file != "" {
		return "", fmt.Errorf("--%s and --%s-file both set; pass one or the other", name, name)
	}

	if file != "" {
		if file == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return "", fmt.Errorf("read --%s-file from stdin: %w", name, err)
			}
			return stripTrailingNewline(string(data)), nil
		}
		// #nosec G304 — file path comes from the user's own CLI
		// invocation; reading it is the whole point of the flag.
		data, err := os.ReadFile(file) //nolint:gosec // G304: CLI-supplied path is the feature
		if err != nil {
			return "", fmt.Errorf("read --%s-file %q: %w", name, file, err)
		}
		return stripTrailingNewline(string(data)), nil
	}

	if inline != "" {
		if strings.ContainsAny(inline, "\n\r") {
			return "", fmt.Errorf("--%s contains a newline; use --%s-file for multi-line values", name, name)
		}
		return inline, nil
	}

	return "", nil
}

// stripTrailingNewline removes a single trailing \n (and an optional
// preceding \r for CRLF files). Editors typically terminate files
// with a newline the user didn't intend to include in the semantic
// value; stripping it keeps stored rationales/reasons clean.
//
// Multiple trailing newlines are preserved beyond the first — a user
// who genuinely wanted a blank line at the end can write two
// newlines, and we honor that.
func stripTrailingNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}
