package exchange

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// frontmatterDelim is the conventional Jekyll/Hugo YAML-frontmatter
// delimiter. We require it on its own line at the start and end of
// the frontmatter block.
const frontmatterDelim = "---"

// MarshalMarkdown renders an AnalystOutput as a markdown document
// with YAML frontmatter. The frontmatter holds the entire structured
// AnalystOutput minus RoundNotes; the body holds RoundNotes verbatim
// as markdown.
//
// This split serves two readers:
//
//   - Machine consumers (the MCP, future signatory instances, the
//     CLI's --json renderer) parse only the frontmatter and ignore
//     the body, getting a complete AnalystOutput minus prose
//     commentary.
//   - Human reviewers reading the file on GitHub or in an editor
//     see structured metadata at the top and prose commentary in
//     the body, where it renders as actual markdown.
//
// Round-trip is lossless: UnmarshalMarkdown(MarshalMarkdown(o))
// produces a struct DeepEqual to the original (modulo
// nil-vs-empty-slice canonicalization, the same caveat that applies
// to JSON round-trip).
func (o *AnalystOutput) MarshalMarkdown() ([]byte, error) {
	if o == nil {
		return nil, errors.New("nil AnalystOutput")
	}

	// Split: frontmatter holds everything except RoundNotes; body
	// holds RoundNotes. We work on a copy so we don't mutate the
	// caller's struct.
	frontmatterStruct := *o
	frontmatterStruct.RoundNotes = ""

	var buf bytes.Buffer
	buf.WriteString(frontmatterDelim)
	buf.WriteByte('\n')

	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&frontmatterStruct); err != nil {
		return nil, fmt.Errorf("marshal frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}

	buf.WriteString(frontmatterDelim)
	buf.WriteByte('\n')

	if o.RoundNotes != "" {
		buf.WriteByte('\n')
		buf.WriteString(o.RoundNotes)
		// Ensure trailing newline for clean concatenation and
		// POSIX-friendliness.
		if !strings.HasSuffix(o.RoundNotes, "\n") {
			buf.WriteByte('\n')
		}
	}

	return buf.Bytes(), nil
}

// UnmarshalMarkdown parses a markdown document with YAML frontmatter
// into an AnalystOutput. The content after the closing frontmatter
// delimiter, stripped of leading/trailing whitespace, becomes
// RoundNotes. Returns an error if the document lacks valid
// frontmatter delimiters or if the YAML doesn't parse.
//
// The returned AnalystOutput is NOT validated — callers that want
// validation should call .Validate() on it. This separation lets
// callers parse-then-fix-then-validate workflows without the parser
// rejecting partially-correct documents.
func UnmarshalMarkdown(data []byte) (*AnalystOutput, error) {
	frontmatter, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, err
	}

	var out AnalystOutput
	if err := yaml.Unmarshal(frontmatter, &out); err != nil {
		return nil, fmt.Errorf("parse frontmatter: %w", err)
	}

	// round_notes should not appear in the frontmatter — the format
	// dedicates the body to it. If the frontmatter contains a value
	// we fail loudly rather than silently discarding the body.
	if out.RoundNotes != "" {
		return nil, errors.New("round_notes appears in frontmatter; should only appear in body")
	}

	out.RoundNotes = strings.TrimSpace(string(body))
	return &out, nil
}

// splitFrontmatter separates YAML frontmatter from the markdown body.
// The frontmatter is bounded by `---` lines; the body is everything
// after the closing delimiter.
//
// Accepted shapes:
//
//	---\n<yaml>\n---\n<body>      (typical)
//	---\n<yaml>\n---              (no body, no trailing newline)
//	---\r\n<yaml>\r\n---\r\n      (CRLF — handled by the line-aware
//	                               parsing below)
func splitFrontmatter(data []byte) (frontmatter, body []byte, err error) {
	// Normalize CRLF to LF for our scanning. yaml.v3 itself
	// handles both, but our line-detection relies on \n.
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))

	// Frontmatter must open with `---\n` at the very start of the
	// document. (We tolerate UTF-8 BOMs as a convenience for
	// Windows-edited files.)
	normalized = bytes.TrimPrefix(normalized, []byte{0xEF, 0xBB, 0xBF})

	openDelim := []byte(frontmatterDelim + "\n")
	if !bytes.HasPrefix(normalized, openDelim) {
		return nil, nil, fmt.Errorf("document does not start with %q frontmatter delimiter", frontmatterDelim)
	}
	rest := normalized[len(openDelim):]

	// Find the closing delimiter. It must be on its own line: a
	// `\n---` sequence followed by either `\n` or end-of-document.
	closingPattern := []byte("\n" + frontmatterDelim)
	idx := bytes.Index(rest, closingPattern)
	for idx >= 0 {
		end := idx + len(closingPattern)
		// Must be at end-of-document or followed by \n.
		if end == len(rest) || rest[end] == '\n' {
			frontmatter = rest[:idx]
			if end < len(rest) {
				body = rest[end+1:] // skip the trailing \n
			}
			return frontmatter, body, nil
		}
		// Otherwise this `---` is part of content (e.g., inside a
		// multi-line YAML literal). Look for the next one.
		next := bytes.Index(rest[end:], closingPattern)
		if next < 0 {
			break
		}
		idx = end + next
	}

	return nil, nil, fmt.Errorf("missing closing %q delimiter", frontmatterDelim)
}
