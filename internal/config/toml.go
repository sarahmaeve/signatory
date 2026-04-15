// Package config parses signatory's configuration file and resolves
// filesystem paths for template reads and filestore writes.
//
// The config format is a deliberately minimal subset of TOML. The
// surface area is:
//
//   - Top-level `key = value` assignments (no tables, no inline
//     tables, no nested keys).
//   - Values are either a double-quoted string or an array of
//     double-quoted strings.
//   - Arrays may span lines; trailing commas are allowed.
//   - `#` starts a comment that runs to end-of-line (except inside
//     quoted strings).
//
// Signatory deliberately does NOT pull in a full-spec TOML parser.
// Third-party parsers are large, have transitive deps, and ship a
// decoder for features (datetimes, subtables, heterogeneous arrays,
// BOM handling, multiline basic strings, etc.) that signatory's
// config surface does not use. The attack surface of a general TOML
// parser is disproportionate to what we need; a ~200-line hand-written
// parser for a ~2-key schema is small enough to fully audit.
//
// If the config schema ever outgrows this subset (unlikely — the
// surface is path lists), the right response is to extend the grammar
// here, not to pull in a dependency.
package config

import (
	"bytes"
	"fmt"
	"io"
)

// maxConfigBytes is the hard limit on how many bytes decodeTOML will
// read from its source. A legitimate signatory config has two keys
// each holding short path lists; even a deliberately padded config
// would not approach 1 MB. The cap prevents a symlinked /dev/zero or
// crafted multi-gigabyte file from OOM-ing the process.
const maxConfigBytes = 1 << 20 // 1 MB

// rawValue is the parser's output for a single `key = value`
// assignment. Exactly one of String / Array is set; IsArray
// distinguishes `x = "foo"` from `x = ["foo"]`. Line is the 1-based
// source line of the key, retained for validation error messages
// issued after parsing completes.
type rawValue struct {
	String  string
	Array   []string
	IsArray bool
	Line    int
}

// decodeTOML reads the entire input and returns a map from key name
// to parsed value. Syntactic errors are returned with a 1-based line
// number. Duplicate keys are a parse error.
//
// The parser is strict: any character outside the documented grammar
// is rejected. Callers validate key names against their schema
// separately (see Config.parseInto).
func decodeTOML(r io.Reader) (map[string]rawValue, error) {
	// Enforce the size cap before reading anything: if the source has
	// more than maxConfigBytes the LimitReader truncates and the
	// (maxConfigBytes+1) sentinel read lets us detect the overflow.
	limited := io.LimitReader(r, maxConfigBytes+1)
	src, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(src) > maxConfigBytes {
		return nil, fmt.Errorf("config input too large (limit %d bytes)", maxConfigBytes)
	}
	// Normalize line endings so the single-byte newline handling below
	// is consistent regardless of the platform that authored the file.
	// Order matters: replace CRLF first, then any surviving bare CR,
	// so Windows (\r\n), Unix (\n), and old-Mac (\r) files all parse
	// identically.
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	src = bytes.ReplaceAll(src, []byte("\r"), []byte("\n"))

	p := &tomlParser{src: src, line: 1}
	result := make(map[string]rawValue)
	for {
		p.skipToNextStatement()
		if p.atEOF() {
			break
		}
		keyLine := p.line
		key, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		if _, dup := result[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", keyLine, key)
		}
		p.skipHorizontalWhitespace()
		if p.atEOF() || p.peek() != '=' {
			return nil, fmt.Errorf("line %d: expected '=' after key %q", keyLine, key)
		}
		p.advance()
		p.skipHorizontalWhitespace()
		val, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		val.Line = keyLine
		result[key] = val
		p.skipTrailing()
		if p.atEOF() {
			break
		}
		if p.peek() != '\n' {
			return nil, fmt.Errorf("line %d: expected newline after value, got %q", p.line, p.peek())
		}
		p.advance()
	}
	return result, nil
}

// tomlParser is a zero-copy scanner over the normalized input. It
// exposes byte-level peek/advance plus higher-level helpers for the
// tiny grammar we support.
type tomlParser struct {
	src  []byte
	pos  int
	line int // 1-based line of src[pos]
}

func (p *tomlParser) atEOF() bool { return p.pos >= len(p.src) }

func (p *tomlParser) peek() byte {
	if p.atEOF() {
		return 0
	}
	return p.src[p.pos]
}

func (p *tomlParser) advance() byte {
	c := p.src[p.pos]
	p.pos++
	if c == '\n' {
		p.line++
	}
	return c
}

// skipHorizontalWhitespace consumes spaces and tabs on the current
// line. It does NOT cross newlines.
func (p *tomlParser) skipHorizontalWhitespace() {
	for !p.atEOF() {
		c := p.peek()
		if c != ' ' && c != '\t' {
			return
		}
		p.advance()
	}
}

// skipToNextStatement advances past any combination of whitespace,
// blank lines, and whole-line comments. On return, the parser is
// positioned either at EOF or at the first byte of the next
// key-assignment.
func (p *tomlParser) skipToNextStatement() {
	for !p.atEOF() {
		p.skipHorizontalWhitespace()
		if p.atEOF() {
			return
		}
		switch p.peek() {
		case '\n':
			p.advance()
		case '#':
			p.skipCommentBody()
		default:
			return
		}
	}
}

// skipTrailing consumes whitespace and an optional trailing comment
// after a value, leaving the parser positioned at the terminating
// newline (or EOF).
func (p *tomlParser) skipTrailing() {
	p.skipHorizontalWhitespace()
	if !p.atEOF() && p.peek() == '#' {
		p.skipCommentBody()
	}
}

// skipCommentBody consumes a `#` comment up to but not including the
// terminating newline.
func (p *tomlParser) skipCommentBody() {
	for !p.atEOF() && p.peek() != '\n' {
		p.advance()
	}
}

// parseKey reads a bare key from the current position. The grammar
// accepts [A-Za-z0-9_-]+; anything else is a parse error. We do not
// support dotted keys, quoted keys, or Unicode key names because the
// schema never needs them.
func (p *tomlParser) parseKey() (string, error) {
	start := p.pos
	for !p.atEOF() {
		c := p.peek()
		if isKeyChar(c) {
			p.advance()
			continue
		}
		break
	}
	if p.pos == start {
		return "", fmt.Errorf("line %d: expected key name, got %q", p.line, p.peek())
	}
	return string(p.src[start:p.pos]), nil
}

func isKeyChar(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '_' || c == '-'
}

// parseValue branches on the first byte to dispatch to the string or
// array parser. Values of any other shape (integer, float, bool,
// date, inline table) are rejected.
func (p *tomlParser) parseValue() (rawValue, error) {
	if p.atEOF() {
		return rawValue{}, fmt.Errorf("line %d: unexpected end of file where value expected", p.line)
	}
	switch p.peek() {
	case '"':
		s, err := p.parseString()
		if err != nil {
			return rawValue{}, err
		}
		return rawValue{String: s}, nil
	case '[':
		arr, err := p.parseArray()
		if err != nil {
			return rawValue{}, err
		}
		return rawValue{Array: arr, IsArray: true}, nil
	default:
		return rawValue{}, fmt.Errorf("line %d: expected '\"' or '[' at start of value, got %q", p.line, p.peek())
	}
}

// parseString reads a double-quoted basic string with a short
// hand-picked escape vocabulary: \", \\, \n, \t, \r. Unterminated
// strings (EOF or bare newline before the closing quote) and unknown
// escapes are errors. Control characters other than tab are rejected
// to catch accidental binary content; UTF-8 bytes in [0x80..0xFF]
// pass through untouched because filesystem paths legitimately
// contain them.
func (p *tomlParser) parseString() (string, error) {
	if p.peek() != '"' {
		return "", fmt.Errorf("line %d: expected '\"', got %q", p.line, p.peek())
	}
	p.advance() // opening quote
	var buf bytes.Buffer
	for !p.atEOF() {
		c := p.peek()
		switch {
		case c == '"':
			p.advance()
			return buf.String(), nil
		case c == '\n':
			return "", fmt.Errorf("line %d: unterminated string (newline before closing quote)", p.line)
		case c == '\\':
			p.advance()
			if p.atEOF() {
				return "", fmt.Errorf("line %d: backslash at end of file", p.line)
			}
			esc := p.advance()
			switch esc {
			case '"':
				buf.WriteByte('"')
			case '\\':
				buf.WriteByte('\\')
			case 'n':
				buf.WriteByte('\n')
			case 't':
				buf.WriteByte('\t')
			case 'r':
				buf.WriteByte('\r')
			default:
				return "", fmt.Errorf("line %d: unknown escape '\\%c'", p.line, esc)
			}
		case (c < 0x20 && c != '\t') || c == 0x7F:
			// Reject C0 control characters (except tab, which is
			// legitimate in paths) and DEL (0x7F).  DEL is not a
			// printable character and has no place in a filesystem path;
			// silently accepting it would allow adversarial configs to
			// smuggle non-printable bytes past casual inspection.
			return "", fmt.Errorf("line %d: control character %#x inside string", p.line, c)
		default:
			buf.WriteByte(p.advance())
		}
	}
	return "", fmt.Errorf("line %d: unterminated string (reached end of file)", p.line)
}

// parseArray reads [ elem, elem, ... ] where each elem is a quoted
// string. Whitespace, newlines, and comments are tolerated between
// elements — so arrays may be formatted vertically for readability.
// A trailing comma is allowed.
func (p *tomlParser) parseArray() ([]string, error) {
	if p.peek() != '[' {
		return nil, fmt.Errorf("line %d: expected '[', got %q", p.line, p.peek())
	}
	p.advance()
	var items []string
	for {
		p.skipArrayNoise()
		if p.atEOF() {
			return nil, fmt.Errorf("line %d: unterminated array (reached end of file)", p.line)
		}
		if p.peek() == ']' {
			p.advance()
			return items, nil
		}
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		items = append(items, s)
		p.skipArrayNoise()
		if p.atEOF() {
			return nil, fmt.Errorf("line %d: unterminated array after element", p.line)
		}
		switch p.peek() {
		case ',':
			p.advance()
		case ']':
			p.advance()
			return items, nil
		default:
			return nil, fmt.Errorf("line %d: expected ',' or ']' after array element, got %q", p.line, p.peek())
		}
	}
}

// skipArrayNoise consumes whitespace, newlines, and `#` comments
// that are allowed between tokens inside an array literal.
func (p *tomlParser) skipArrayNoise() {
	for !p.atEOF() {
		c := p.peek()
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			p.advance()
		case c == '#':
			p.skipCommentBody()
		default:
			return
		}
	}
}
