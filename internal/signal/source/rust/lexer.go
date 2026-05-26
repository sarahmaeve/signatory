// Package rust is the Rust source-evolution analyzer. It mirrors the
// golang, python, and node packages' role: turn a stream of .rs files
// into an astfeature.Counts per version.
//
// Like the python and node packages it uses a hand-written lexer +
// parser. A stale third-party Rust parser is itself a supply-chain
// risk in a supply-chain tool; this also keeps the analyzer bounded
// to the *security-relevant* subset of the language (calls, imports,
// scope, macro invocations, string args) — NOT a conformant Rust
// grammar. The Rust type system (traits, generics, lifetimes,
// `where`, `dyn`, `impl`, `as`, the turbofish) is lexed leniently
// and the parser ignores it. Modelling types buys no trust signal
// and the angle-bracket disambiguation in nested generics is the
// same tar pit `<T>`-vs-JSX is for TypeScript (AST.md §4). Anyone
// tempted to "fix" this into a real parser should read AST.md §4
// first.
//
// File scope: this package consumes the .rs files
// isRustSourceFile (in blobstream.go) admits — primarily
// `src/**/*.rs` and `build.rs`, with tests/, benches/, examples/,
// target/, and `.d.rs` excluded as not-runtime / not-authored
// source.
package rust

import "fmt"

// TokenKind enumerates the lexical categories the construct
// extractor needs. String covers ordinary strings, raw strings,
// byte strings, byte raw strings, and char/byte char literals:
// every literal kind is opaque to the parser so a catalog token
// spelled inside one is never mistaken for code (AST.md §4
// "opaque tokens" lesson).
//
// Lifetime annotations (`'a`, `'static`) are emitted as TokenName
// with a leading apostrophe in the value. The parser ignores
// lifetimes for trust-signal extraction; this representation keeps
// the lexer stream linear without a separate token category.
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

// Token is one lexical unit. Line is 1-based, used by the parser
// for position reporting only — the analyzer never opens files
// (positions are not surfaced externally; they just help debug
// lexer-loop bugs).
type Token struct {
	Kind  TokenKind
	Value string
	Line  int
}

// maxBlockCommentDepth bounds scanBlockComment's recursion through
// nested `/* /* */ */` constructs. signatory lexes untrusted source
// up to the BlobStreamer's 10 MiB cap; without a bound, a crafted
// file of ~3M `/*` levels recurses one Go frame per level and
// stack-overflows, aborting the whole collection (a DoS and an
// evasion — AST.md §4 "Port robustness_test.go *first*, and bound
// every self-recursive scan"). Over the cap, an inner `/*` is
// treated as ordinary bytes inside the outer comment: the worst
// case is the outer comment swallowing more content than a true
// parser would — a conservative miss, never a false call. 256
// matches node's maxTemplateDepth.
const maxBlockCommentDepth = 256

// maxRawStringHashes bounds the `#` count on a raw-string literal
// (`r#####"..."#####`). The Rust language allows an unlimited
// count; we cap at 256 because no legitimate source uses more,
// and an attacker-supplied file with hundreds of thousands of `#`s
// is either nonsense or evasion-shaped. Over the cap the raw-string
// scan aborts and the `r`/`#` bytes are re-lexed as ordinary tokens
// — a conservative miss (the literal contents may now appear to
// the parser, but that's harmless because the parser ignores
// non-call/non-string tokens for catalog matching).
const maxRawStringHashes = 256

// Lex tokenizes Rust source. Intentionally lenient: this is a
// trust-signal extractor, not a Rust compiler, so it favors forward
// progress over rejecting input a real `rustc` would. Rust is
// brace-scoped with no significant indentation, so no INDENT/DEDENT
// or statement-terminator tokens are emitted — the parser tracks
// scope from braces directly.
//
// Unterminated literals (strings, raw strings, char literals,
// block comments) consume to EOF rather than erroring; AST.md §4:
// "Malformed/adversarial input must yield a best-effort partial
// result, never abort the file."
func Lex(src []byte) ([]Token, error) {
	var toks []Token
	line := 1
	i := 0
	n := len(src)

	emit := func(k TokenKind, v string) {
		toks = append(toks, Token{Kind: k, Value: v, Line: line})
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
			// Line comment (also covers `///` doc and `//!` inner-doc).
			for i < n && src[i] != '\n' {
				i++
			}

		case c == '/' && i+1 < n && src[i+1] == '*':
			// Block comment, nestable in Rust unlike C. Depth-bounded
			// per maxBlockCommentDepth.
			end, nl := scanBlockComment(src, i)
			line += nl
			i = end

		case c == '"':
			j, nl := scanString(src, i)
			emit(TokenString, string(src[i:j]))
			line += nl
			i = j

		case c == 'r' && i+1 < n && (src[i+1] == '"' || src[i+1] == '#'):
			// Raw string: r"..." or r#"..."# (any matched # count).
			// Lenient — if the hash count exceeds maxRawStringHashes,
			// scanRawString returns ok=false and we fall through to
			// the ordinary identifier path so the bytes don't get
			// stuck.
			if j, nl, ok := scanRawString(src, i+1); ok {
				emit(TokenString, string(src[i:j]))
				line += nl
				i = j
				continue
			}
			// Hash overflow — re-lex `r` as an identifier.
			j := i + 1
			for j < n && isNameContinue(src[j]) {
				j++
			}
			emit(TokenName, string(src[i:j]))
			i = j

		case c == 'b' && i+1 < n && src[i+1] == '"':
			// Byte string: b"...". Same scan as a regular string; the
			// `b` prefix joins the opaque-literal blob.
			j, nl := scanString(src, i+1)
			emit(TokenString, string(src[i:j]))
			line += nl
			i = j

		case c == 'b' && i+1 < n && src[i+1] == '\'':
			// Byte char literal: b'X' or b'\n'. Lexically a single-
			// char literal opaque to the parser.
			j, nl := scanCharLiteral(src, i+1)
			emit(TokenString, string(src[i:j]))
			line += nl
			i = j

		case c == 'b' && i+2 < n && src[i+1] == 'r' && (src[i+2] == '"' || src[i+2] == '#'):
			// Byte raw string: br"...", br#"..."#.
			if j, nl, ok := scanRawString(src, i+2); ok {
				emit(TokenString, string(src[i:j]))
				line += nl
				i = j
				continue
			}
			// Hash overflow — re-lex `br` as an identifier.
			j := i + 1
			for j < n && isNameContinue(src[j]) {
				j++
			}
			emit(TokenName, string(src[i:j]))
			i = j

		case c == '\'':
			// Char literal OR lifetime annotation. Disambiguated by
			// look-ahead: 'X' (closing apostrophe within 1-2 bytes,
			// or an escape sequence) → char; 'name (no closing
			// apostrophe, identifier continues) → lifetime.
			tok, j, nl := scanCharOrLifetime(src, i, line)
			emit(tok.Kind, tok.Value)
			line += nl
			i = j

		case isNameStart(c):
			j := i + 1
			for j < n && isNameContinue(src[j]) {
				j++
			}
			emit(TokenName, string(src[i:j]))
			i = j

		case c >= '0' && c <= '9':
			j := scanNumber(src, i)
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

// scanString consumes a "..." literal and returns the index just past
// the closing quote plus the number of embedded newlines. Lenient:
// an unterminated literal ends at EOF rather than erroring.
//
// Rust strings allow embedded newlines, unlike JS — so newline is NOT
// a terminator. Backslash-escape is the only special handling.
func scanString(src []byte, i int) (end, newlines int) {
	n := len(src)
	j := i + 1
	for j < n {
		switch src[j] {
		case '\\':
			if j+1 < n && src[j+1] == '\n' {
				newlines++
			}
			j += 2
		case '\n':
			newlines++
			j++
		case '"':
			return j + 1, newlines
		default:
			j++
		}
	}
	return n, newlines
}

// scanRawString consumes a raw-string literal whose opening starts at
// position i (positioned on the first `"` or `#` after the `r`/`br`
// prefix). Counts the leading `#`s, then matches the same count of
// `#`s after the closing `"`. Returns (end, newlines, ok). ok=false
// when the hash count exceeds maxRawStringHashes — the caller falls
// through to ordinary identifier scanning.
//
// Raw strings have no escape processing — every byte until the
// matching `"<#>{n}` is literal. Newlines are counted for line-number
// accuracy.
func scanRawString(src []byte, i int) (end, newlines int, ok bool) {
	n := len(src)
	hashes := 0
	j := i
	for j < n && src[j] == '#' {
		hashes++
		j++
		if hashes > maxRawStringHashes {
			return i, 0, false
		}
	}
	if j >= n || src[j] != '"' {
		// No opening quote after the hashes — not actually a raw
		// string. ok=false so caller re-lexes.
		return i, 0, false
	}
	j++ // past opening "
	for j < n {
		if src[j] == '\n' {
			newlines++
			j++
			continue
		}
		if src[j] == '"' {
			// Try to match the closing hash run.
			k := j + 1
			matched := 0
			for matched < hashes && k < n && src[k] == '#' {
				matched++
				k++
			}
			if matched == hashes {
				return k, newlines, true
			}
			j++
			continue
		}
		j++
	}
	// Unterminated — consume to EOF, lenient.
	return n, newlines, true
}

// scanBlockComment consumes a /* ... */ block comment starting at i,
// including any nested /* ... */ blocks inside it (Rust's block
// comments nest, unlike C's). Returns the index just past the
// closing `*/` plus the number of embedded newlines. Depth-bounded
// by maxBlockCommentDepth — over the cap, an inner `/*` is ignored
// rather than recursing, so the outer comment swallows more content
// than a true Rust parser would (a conservative miss).
func scanBlockComment(src []byte, i int) (end, newlines int) {
	return scanBlockCommentDepth(src, i, 0)
}

func scanBlockCommentDepth(src []byte, i, depth int) (end, newlines int) {
	n := len(src)
	j := i + 2 // past opening /*
	for j < n {
		switch {
		case src[j] == '\n':
			newlines++
			j++
		case src[j] == '*' && j+1 < n && src[j+1] == '/':
			return j + 2, newlines
		case src[j] == '/' && j+1 < n && src[j+1] == '*':
			if depth >= maxBlockCommentDepth {
				// Recursion cap: treat the nested `/*` as ordinary
				// bytes. Bounded, conservative miss.
				j += 2
				continue
			}
			sub, nl := scanBlockCommentDepth(src, j, depth+1)
			newlines += nl
			j = sub
		default:
			j++
		}
	}
	// Unterminated block comment — lenient, consume to EOF.
	return n, newlines
}

// scanCharLiteral consumes a 'X' or '\X' char-literal-shaped token
// starting at i (positioned on the opening `'`). Returns the index
// just past the closing `'` plus the number of embedded newlines
// (which, for a sane char literal, is zero — but we count for
// adversarial cases where the input is truly malformed).
//
// Used both for plain char literals and (via the b' prefix path) for
// byte char literals.
func scanCharLiteral(src []byte, i int) (end, newlines int) {
	n := len(src)
	j := i + 1
	for j < n {
		switch src[j] {
		case '\\':
			if j+1 < n && src[j+1] == '\n' {
				newlines++
			}
			j += 2
		case '\n':
			// A real char literal can't span newlines; bail
			// leniently rather than swallow the rest of the file.
			newlines++
			return j, newlines
		case '\'':
			return j + 1, newlines
		default:
			j++
		}
	}
	return n, newlines
}

// scanCharOrLifetime disambiguates `'X'` (char literal) from `'name`
// (lifetime annotation) starting at the opening apostrophe.
//
// Disambiguation rule (matches rustc's lexer):
//
//   - `'\X'` or `'\X{...}'` — backslash escape, definitely a char
//     literal. Consume escape and closing apostrophe.
//   - `'X'` where the byte after X is the closing apostrophe — char.
//   - `'X` where the byte after X is NOT an apostrophe and X is a
//     name-continue byte — lifetime. Consume identifier chars.
//   - Otherwise (e.g. `'!` or `'9'`) — char literal (lenient
//     fallback). The byte literal scanner will give up at the next
//     apostrophe or newline.
//
// Emits TokenString for char literals (consistent with strings —
// opaque to the parser) and TokenName for lifetimes (the apostrophe
// is preserved in the value so the parser can filter on it cheaply).
func scanCharOrLifetime(src []byte, i, line int) (tok Token, end, newlines int) {
	n := len(src)
	if i+1 >= n {
		// Bare apostrophe at EOF — treat as a malformed char literal.
		return Token{Kind: TokenString, Value: "'", Line: line}, n, 0
	}
	if src[i+1] == '\\' {
		// Escape: definitely a char literal.
		end, nl := scanCharLiteral(src, i)
		return Token{Kind: TokenString, Value: string(src[i:end]), Line: line}, end, nl
	}
	if i+2 < n && src[i+2] == '\'' {
		// 'X' single-char literal.
		return Token{Kind: TokenString, Value: string(src[i : i+3]), Line: line}, i + 3, 0
	}
	if isNameStart(src[i+1]) {
		// 'name lifetime.
		j := i + 2
		for j < n && isNameContinue(src[j]) {
			j++
		}
		return Token{Kind: TokenName, Value: string(src[i:j]), Line: line}, j, 0
	}
	// Lenient fallback — treat as a malformed char literal scanned
	// until the next apostrophe / newline / EOF.
	end, nl := scanCharLiteral(src, i)
	return Token{Kind: TokenString, Value: string(src[i:end]), Line: line}, end, nl
}

// multiCharOps is the maximal-munch operator table, longest first.
//
// Critical entries:
//   - `^=` — XOR-assign, the load-bearing token for the
//     XORAssignments Counts field (the Trapdoor cargo payload's
//     dominant obfuscation primitive — design/threat-landscape/
//     2026-05-24-trapdoor-crypto-stealer.md).
//   - `::` — path separator (std::env::var, etc.), required to
//     keep `std`/`env`/`var` separable yet recognizable as a
//     single path expression in the parser.
//   - `=>` — match arm, also closure-body separator in some
//     contexts. Same role as in JS/TS.
//   - `->` — function return-type arrow.
//   - `..` and `..=` — range operators; `for i in 0..data.len()`
//     is the canonical XOR-loop shape.
//
// Deliberately omitted: `>>` and `>>=`. Rust's nested-generics
// closing (`Vec<HashMap<K, V>>`) would otherwise be mis-lexed as a
// shift operator; rustc itself handles this at the parser stage
// via fixup. Since the analyzer doesn't extract shifts or generics
// for catalog matching, emitting two separate `>` tokens is the
// safer choice — the parser's nesting tracker isn't fooled, and
// we don't gain anything by recognizing right-shift specifically.
var multiCharOps = []string{
	"..=", "...",
	"<<=",
	"::", "->", "=>",
	"==", "!=", "<=", ">=", "&&", "||",
	"<<",
	"+=", "-=", "*=", "/=", "%=", "^=", "&=", "|=",
	"..",
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

// isNameStart includes underscore alongside the usual letter starts.
// Bytes >= 0x80 are accepted so non-ASCII identifiers don't
// fragment — Rust permits Unicode identifiers under XID_Start /
// XID_Continue, and we follow the same byte-pessimistic admission
// pattern node uses.
func isNameStart(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isNameContinue(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

// scanNumber returns the index just past a numeric literal starting
// at i. Greedy on type suffixes and underscores, but with explicit
// look-ahead at `.` so that a range operator (`..` or `..=`) does
// NOT get sucked into the number — `0..data.len()` must lex as
// (NUM 0)(OP ..)(NAME data)(OP .)(NAME len)(OP ()(OP )) not as one
// fused NUM "0..data". This is the Rust analog of the JS lexer's
// regex-vs-division decision: in Rust, decimal `.` and range `..`
// share a starting byte, and the right disambiguator is the byte
// after.
//
// Rule: a `.` consumed into the number must be followed by either
// a digit (continuing the decimal part) OR an identifier-start
// byte (the type suffix in `1.5f64` is handled separately by the
// `f`/`u`/`i` continues). If the next byte is `.` itself, this is
// a range — stop the number before the dot.
func scanNumber(src []byte, i int) int {
	n := len(src)
	j := i + 1
	for j < n {
		c := src[j]
		if c == '.' {
			// Look ahead: range op (`..`) wins over decimal point.
			if j+1 < n && src[j+1] == '.' {
				return j
			}
			j++
			continue
		}
		if !isNumberContinue(c) {
			return j
		}
		j++
	}
	return j
}

// isNumberContinue is deliberately loose — it just keeps a numeric
// literal (decimal, hex 0x, octal 0o, binary 0b, exponent, type
// suffix like 1u32 / 1.5f64, underscores for readability) from
// fragmenting into a Number followed by stray Names/Ops. The `.`
// case is handled by scanNumber's look-ahead, NOT here, because the
// per-byte signature can't peek at the next byte.
func isNumberContinue(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		return true
	case c == 'x' || c == 'X' || c == 'o' || c == 'O' || c == 'b' || c == 'B':
		return true
	case c == '_':
		return true
	case c == 'u' || c == 'i' || c == 'f':
		// Type suffix on integer/float literals (1u32, 1i64, 1.5f64).
		// Captured greedily into the number token so a downstream
		// parser sees a single TokenNumber.
		return true
	case c == 'e' || c == 'E':
		// Float exponent.
		return true
	}
	return false
}
