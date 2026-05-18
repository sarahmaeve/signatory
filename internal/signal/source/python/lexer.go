package python

import "fmt"

// TokenKind enumerates the lexical categories the Python construct
// extractor needs. This is deliberately a security-relevant subset,
// not the full CPython token set: enough to find imports, calls,
// assignments, string args, and import-time (module-scope) code.
type TokenKind int

const (
	TokenEOF TokenKind = iota
	TokenName
	TokenNumber
	TokenString
	TokenOp
	TokenNewline // logical end-of-statement
	TokenIndent
	TokenDedent
)

func (k TokenKind) String() string {
	switch k {
	case TokenEOF:
		return "EOF"
	case TokenName:
		return "NAME"
	case TokenNumber:
		return "NUMBER"
	case TokenString:
		return "STRING"
	case TokenOp:
		return "OP"
	case TokenNewline:
		return "NEWLINE"
	case TokenIndent:
		return "INDENT"
	case TokenDedent:
		return "DEDENT"
	default:
		return fmt.Sprintf("TokenKind(%d)", int(k))
	}
}

// Token is one lexical unit. Line is 1-based; used later for
// parser-position reporting (the analyzer never opens files).
type Token struct {
	Kind  TokenKind
	Value string
	Line  int
}

// Lex tokenizes Python source. It is intentionally lenient: this is a
// trust-signal extractor, not a Python implementation, so it favors
// making forward progress over rejecting input a real interpreter
// would.
//
// It implements the CPython-style indentation algorithm (INDENT /
// DEDENT against an indent stack, tab stops of 8) and physical→logical
// line joining: newlines inside (), [], {} or after a backslash are
// not statement terminators. Blank and comment-only lines emit
// nothing. These are exactly the features the extractor needs to tell
// import-time (module-scope) code from code nested in def/class.
func Lex(src []byte) ([]Token, error) {
	var toks []Token
	line := 1
	i := 0
	n := len(src)

	indents := []int{0}
	parenDepth := 0
	atLineStart := true    // measuring indentation for a logical line
	lineHasTokens := false // emitted a real token on the current logical line

	emit := func(k TokenKind, v string) {
		toks = append(toks, Token{Kind: k, Value: v, Line: line})
	}

	for i < n {
		// --- logical-line start: indentation handling ---
		if atLineStart && parenDepth == 0 {
			col := 0
			j := i
			for j < n {
				switch src[j] {
				case ' ':
					col++
					j++
				case '\t':
					col += 8 - (col % 8)
					j++
				case '\r':
					j++
				default:
					goto measured
				}
			}
		measured:
			// Blank line or comment-only line: consume to and
			// including the newline, emit nothing, stay at line start.
			if j >= n || src[j] == '\n' || src[j] == '#' {
				for j < n && src[j] != '\n' {
					j++
				}
				if j < n { // consume the '\n'
					j++
					line++
				}
				i = j
				continue
			}
			// Real content: reconcile the indent stack.
			top := indents[len(indents)-1]
			if col > top {
				indents = append(indents, col)
				emit(TokenIndent, "")
			} else {
				for col < indents[len(indents)-1] {
					indents = indents[:len(indents)-1]
					emit(TokenDedent, "")
				}
			}
			i = j
			atLineStart = false
		}

		c := src[i]

		switch {
		case c == '\\' && i+1 < n && src[i+1] == '\n':
			// Explicit line continuation: join physical lines.
			i += 2
			line++

		case c == '\n':
			if parenDepth > 0 {
				// Implicit line join inside brackets: not a terminator.
				i++
				line++
				break
			}
			if lineHasTokens {
				emit(TokenNewline, "")
				lineHasTokens = false
			}
			i++
			line++
			atLineStart = true

		case c == ' ' || c == '\t' || c == '\r':
			i++

		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}

		case isStringPrefixAt(src, i):
			j, nlines, err := scanString(src, i)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			emit(TokenString, string(src[i:j]))
			lineHasTokens = true
			line += nlines
			i = j

		case isNameStart(c):
			j := i + 1
			for j < n && isNameContinue(src[j]) {
				j++
			}
			emit(TokenName, string(src[i:j]))
			lineHasTokens = true
			i = j

		case c >= '0' && c <= '9':
			j := i + 1
			for j < n && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			emit(TokenNumber, string(src[i:j]))
			lineHasTokens = true
			i = j

		default:
			op := scanOperator(src, i)
			switch op {
			case "(", "[", "{":
				parenDepth++
			case ")", "]", "}":
				if parenDepth > 0 {
					parenDepth--
				}
			}
			emit(TokenOp, op)
			lineHasTokens = true
			i += len(op)
		}
	}

	// Terminate a trailing logical line that lacked a final newline.
	if lineHasTokens {
		emit(TokenNewline, "")
	}
	// Close any open indentation.
	for len(indents) > 1 {
		indents = indents[:len(indents)-1]
		emit(TokenDedent, "")
	}
	emit(TokenEOF, "")
	return toks, nil
}

// multiCharOps is the maximal-munch operator table, longest first.
// Only operators that matter to the construct extractor or that would
// corrupt the token stream if split (e.g. ^= vs ^ =) need to be here;
// unknown punctuation falls through to single-char ops.
var multiCharOps = []string{
	"**=", "//=", ">>=", "<<=", "...", "!=", "==", ">=", "<=", ":=",
	"->", "//", "**", "<<", ">>", "+=", "-=", "*=", "/=", "%=",
	"^=", "&=", "|=", "@=",
}

func scanOperator(src []byte, i int) string {
	rest := src[i:]
	for _, op := range multiCharOps {
		if len(rest) >= len(op) && string(rest[:len(op)]) == op {
			return op
		}
	}
	return string(src[i])
}

// stringPrefixByte reports whether c is a legal string-prefix letter
// (r/b/f/u, either case). Combinations like rb, fr are validated by
// length in isStringPrefixAt.
func stringPrefixByte(c byte) bool {
	switch c {
	case 'r', 'R', 'b', 'B', 'f', 'F', 'u', 'U':
		return true
	}
	return false
}

// isStringPrefixAt reports whether a string literal starts at i,
// including up to two prefix letters (rb, fr, ...). It must reject
// ordinary identifiers: `range` is a name, `r"x"` is a string.
func isStringPrefixAt(src []byte, i int) bool {
	if i >= len(src) {
		return false
	}
	if src[i] == '\'' || src[i] == '"' {
		return true
	}
	for p := 0; p < 2 && i+p < len(src); p++ {
		if !stringPrefixByte(src[i+p]) {
			return false
		}
		if i+p+1 < len(src) && (src[i+p+1] == '\'' || src[i+p+1] == '"') {
			return true
		}
	}
	return false
}

// scanString consumes a (possibly prefixed, possibly triple-quoted)
// string literal starting at i and returns the index just past it
// plus the number of embedded newlines (so the lexer keeps line
// numbers accurate through triple-quoted blocks). Lenient by design:
// an unterminated literal ends at EOF rather than erroring, because a
// trust extractor must make progress on adversarial / malformed
// input rather than abort the whole file.
func scanString(src []byte, i int) (end, newlines int, err error) {
	n := len(src)
	j := i
	for j < n && stringPrefixByte(src[j]) {
		j++
	}
	if j >= n {
		return n, 0, nil
	}
	q := src[j]
	triple := j+2 < n && src[j+1] == q && src[j+2] == q
	if triple {
		j += 3
		for j < n {
			if src[j] == '\\' {
				j += 2 // skip escaped char (incl. an escaped quote)
				continue
			}
			if j+2 < n && src[j] == q && src[j+1] == q && src[j+2] == q {
				j += 3
				return j, countNewlines(src[i:j]), nil
			}
			j++
		}
		return n, countNewlines(src[i:n]), nil
	}
	j++ // past opening quote
	for j < n {
		switch src[j] {
		case '\\':
			j += 2
		case q:
			j++
			return j, 0, nil
		case '\n':
			// Unterminated single-line string; stop at newline.
			return j, 0, nil
		default:
			j++
		}
	}
	return n, 0, nil
}

func countNewlines(b []byte) int {
	c := 0
	for _, x := range b {
		if x == '\n' {
			c++
		}
	}
	return c
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isNameContinue(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
