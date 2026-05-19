// Package node is the JavaScript/TypeScript source-evolution
// analyzer. It mirrors the golang and python packages' role: turn a
// stream of source files into an astfeature.Counts per version.
//
// Like the python package it uses a hand-written lexer+parser (no
// external dependency — a stale third-party JS parser is itself a
// supply-chain risk in a supply-chain tool). It is a deliberately
// security-relevant subset, NOT a conformant ECMAScript/TypeScript
// grammar: enough to find imports, calls, the scope each call runs in
// (module/import-time vs inside a function), class bases, and string
// arguments. TypeScript type syntax is lexed leniently and ignored by
// the parser — modelling the type system buys no trust signal. Anyone
// tempted to "fix" this into a real parser should read AST.md §4
// first.
package node

import "fmt"

// TokenKind enumerates the lexical categories the construct extractor
// needs. String covers ordinary strings, template literals, AND regex
// literals: all three are opaque to the parser so a call/keyword
// spelled inside one is never mistaken for code.
type TokenKind int

const (
	TokenEOF TokenKind = iota
	TokenName
	TokenNumber
	TokenString
	TokenOp
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
	default:
		return fmt.Sprintf("TokenKind(%d)", int(k))
	}
}

// Token is one lexical unit. Line is 1-based, for parser-position
// reporting (the analyzer never opens files).
type Token struct {
	Kind  TokenKind
	Value string
	Line  int
}

// Lex tokenizes JS/TS source. Intentionally lenient: this is a
// trust-signal extractor, not a JS engine, so it favors forward
// progress over rejecting input a real parser would. JS is
// brace-scoped (no significant indentation), so no INDENT/DEDENT or
// statement-terminator tokens are emitted — the parser tracks scope
// from braces and function/arrow markers directly.
func Lex(src []byte) ([]Token, error) {
	var toks []Token
	line := 1
	i := 0
	n := len(src)

	// prev points at the last emitted token, for the regex-vs-division
	// disambiguation. nil at start-of-input (a leading `/` is a regex).
	var prev *Token
	emit := func(k TokenKind, v string) {
		toks = append(toks, Token{Kind: k, Value: v, Line: line})
		prev = &toks[len(toks)-1]
	}

	for i < n {
		c := src[i]

		switch {
		case c == '\n':
			line++
			i++

		case c == ' ' || c == '\t' || c == '\r':
			i++

		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}

		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			for i < n {
				if src[i] == '\n' {
					line++
				}
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					i += 2
					break
				}
				i++
			}

		case c == '/' && regexAllowed(prev):
			j := scanRegex(src, i)
			emit(TokenString, string(src[i:j]))
			i = j

		case c == '`':
			j, nl := scanTemplate(src, i)
			emit(TokenString, string(src[i:j]))
			line += nl
			i = j

		case c == '\'' || c == '"':
			j := scanString(src, i)
			emit(TokenString, string(src[i:j]))
			i = j

		case isNameStart(c):
			j := i + 1
			for j < n && isNameContinue(src[j]) {
				j++
			}
			emit(TokenName, string(src[i:j]))
			i = j

		case c >= '0' && c <= '9':
			j := i + 1
			for j < n && isNumberContinue(src[j]) {
				j++
			}
			emit(TokenNumber, string(src[i:j]))
			i = j

		default:
			op := scanOperator(src, i)
			emit(TokenOp, op)
			i += len(op)
		}
	}

	toks = append(toks, Token{Kind: TokenEOF, Value: "", Line: line})
	return toks, nil
}

// regexPrefixKeywords are the keywords after which a `/` begins a
// regex literal, not division (e.g. `return /re/`, `typeof /re/`).
var regexPrefixKeywords = map[string]struct{}{
	"return": {}, "typeof": {}, "instanceof": {}, "in": {}, "of": {},
	"new": {}, "delete": {}, "void": {}, "throw": {}, "do": {},
	"else": {}, "yield": {}, "await": {}, "case": {},
}

// regexAllowed reports whether a `/` at the current position begins a
// regex literal rather than a division operator, using the standard
// previous-significant-token heuristic. Erring is bounded: the only
// genuinely ambiguous shapes are rare, and the common forms
// (`x = /re/`, `return /re/`, `f(/re/)`) resolve correctly.
func regexAllowed(prev *Token) bool {
	if prev == nil {
		return true
	}
	switch prev.Kind {
	case TokenName:
		// A keyword that takes an expression after it → regex; any
		// other identifier is a value → division.
		_, kw := regexPrefixKeywords[prev.Value]
		return kw
	case TokenNumber, TokenString:
		return false
	case TokenOp:
		switch prev.Value {
		case ")", "]", "}":
			return false
		default:
			return true
		}
	}
	return false
}

// scanString consumes a '..' or ".." literal and returns the index
// just past it. Lenient: an unterminated literal ends at newline or
// EOF rather than erroring, so the lexer keeps making progress on
// adversarial input.
func scanString(src []byte, i int) int {
	n := len(src)
	q := src[i]
	j := i + 1
	for j < n {
		switch src[j] {
		case '\\':
			j += 2
		case q:
			return j + 1
		case '\n':
			return j // unterminated — stop at newline
		default:
			j++
		}
	}
	return n
}

// scanTemplate consumes a `...` template literal as one opaque token,
// returning the index just past the closing backtick and the number
// of embedded newlines (to keep line numbers accurate). It tracks
// ${ } interpolation brace depth and recurses through nested template
// literals and skips interior strings so a backtick or brace inside
// an interpolation's string/nested-template doesn't end the outer
// literal. Code inside ${} is deliberately NOT tokenized — a
// documented conservative miss (AST.md §4). Lenient: unterminated
// ends at EOF.
func scanTemplate(src []byte, i int) (end, newlines int) {
	n := len(src)
	j := i + 1 // past opening backtick
	for j < n {
		switch {
		case src[j] == '\\':
			j += 2
		case src[j] == '\n':
			newlines++
			j++
		case src[j] == '`':
			return j + 1, newlines
		case src[j] == '$' && j+1 < n && src[j+1] == '{':
			j += 2
			depth := 1
			for j < n && depth > 0 {
				switch src[j] {
				case '\\':
					j += 2
				case '\n':
					newlines++
					j++
				case '{':
					depth++
					j++
				case '}':
					depth--
					j++
				case '`':
					sub, nl := scanTemplate(src, j)
					newlines += nl
					j = sub
				case '\'', '"':
					j = scanString(src, j)
				default:
					j++
				}
			}
		default:
			j++
		}
	}
	return n, newlines
}

// scanRegex consumes a /.../flags regex literal as one opaque token,
// returning the index just past the flags. A '/' inside a [...] char
// class is literal, not the terminator; backslash escapes are
// skipped. Lenient: an unterminated regex ends at newline/EOF.
func scanRegex(src []byte, i int) int {
	n := len(src)
	j := i + 1
	inClass := false
	for j < n {
		switch src[j] {
		case '\\':
			j += 2
			continue
		case '\n':
			return j // unterminated
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				j++ // past closing slash
				for j < n && isFlagByte(src[j]) {
					j++
				}
				return j
			}
		}
		j++
	}
	return n
}

func isFlagByte(c byte) bool {
	return c >= 'a' && c <= 'z'
}

// multiCharOps is the maximal-munch operator table, longest first.
// Only operators that matter to the extractor or that would corrupt
// the stream if split need to be here: ^= (XOR-deobfuscation threat),
// => (function-body / scope marker), ?. and ... (so member/spread
// don't masquerade as '.'), and the compound assigns/comparisons.
// Unknown punctuation falls through to single-char ops.
var multiCharOps = []string{
	">>>=", "===", "!==", "**=", "<<=", ">>=", ">>>", "&&=", "||=", "??=", "...",
	"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "**",
	"+=", "-=", "*=", "/=", "%=", "^=", "&=", "|=", "<<", ">>", "++", "--",
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

// isNameStart includes '$' and '#' (private class members) alongside
// the usual identifier starts. Bytes >= 0x80 are accepted so
// non-ASCII identifiers don't fragment.
func isNameStart(c byte) bool {
	return c == '_' || c == '$' || c == '#' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isNameContinue(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// isNumberContinue is deliberately loose — it just keeps a numeric
// literal (hex, binary, exponent, separators, bigint) from
// fragmenting into a Number followed by stray Names/Ops. Numbers are
// not catalog-relevant; only their non-merging matters.
func isNumberContinue(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		return true
	case c == 'x' || c == 'X' || c == 'o' || c == 'O' || c == 'b' || c == 'B':
		return true
	case c == '.' || c == '_' || c == 'n':
		return true
	}
	return false
}
