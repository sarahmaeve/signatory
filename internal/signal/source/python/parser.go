package python

import "strings"

// Module is the parsed view of one Python source file, reduced to the
// constructs the trust extractor cares about. It is NOT a full AST:
// only imports, call sites, and the scope each call is in. Anything
// the security catalog does not need is intentionally dropped.
type Module struct {
	// Imports is every imported dotted path. `import os.path as p`
	// records "os.path"; `from base64 import b64decode` records
	// "base64.b64decode"; `from x import a, b as c` records "x.a"
	// and "x.b". Aliases are discarded — the catalog matches on the
	// real module/symbol, not the local binding.
	Imports []string

	// Calls is every call site, with the scope discriminator the
	// PyPI-attack model hinges on (ModuleScope == runs on import).
	Calls []Call

	// XorAssigns counts `^=` augmented assignments — the canonical
	// XOR-deobfuscation loop primitive. Mirrors the Go analyzer's
	// token.XOR_ASSIGN-only scope (binary `^` inside a plain `=` is
	// the same documented gap there).
	XorAssigns int
}

// Call is one call site: the dotted callee (e.g. "os.system",
// "exec", "a.b.c.d") and whether it is lexically at module scope —
// i.e. executes at import time rather than inside a def/class body.
type Call struct {
	Callee      string
	ModuleScope bool
	Line        int

	// FirstArg is the statically-resolved value of the call's first
	// positional argument, or "" when it can't be resolved without
	// execution. Resolves string literals, implicit concatenation,
	// and os.path.join / expanduser / expandvars of resolvable
	// parts. Deliberately conservative — f-strings with
	// interpolation, .format, % and any name/call result are
	// unresolved (same documented static-resolution gap as the Go
	// analyzer). Feeds SensitivePathReads.
	FirstArg string
}

// pyKeywords are names that, standing alone before '(', are not a
// call (e.g. `return(x)`, `assert(x)`). A dotted callee whose sole
// segment is one of these is skipped. Multi-segment callees can't be
// keywords, so they are unaffected.
var pyKeywords = map[string]struct{}{
	"return": {}, "if": {}, "elif": {}, "else": {}, "for": {}, "while": {},
	"with": {}, "try": {}, "except": {}, "finally": {}, "raise": {},
	"yield": {}, "assert": {}, "del": {}, "lambda": {}, "and": {}, "or": {},
	"not": {}, "in": {}, "is": {}, "await": {}, "async": {}, "global": {},
	"nonlocal": {}, "pass": {}, "break": {}, "continue": {}, "import": {},
	"from": {}, "as": {}, "def": {}, "class": {}, "None": {}, "True": {},
	"False": {},
}

// Parse lexes then walks the token stream, recording imports and call
// sites with scope. Lenient like the lexer: malformed input yields a
// best-effort partial Module, never an error abort, because a trust
// extractor must keep producing signal on adversarial source.
func Parse(src []byte) (*Module, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	m := &Module{}

	// defClass[k] is true when indentation level k is a def/class
	// suite. A call is module-scope (runs on import) iff no enclosing
	// level is a def/class — module-level if/for/try still run on
	// import, so raw indentation is the wrong test; this is not.
	var defClass []bool
	pendingDefClass := false
	atStmt := true    // at the first token of a logical statement
	inHeader := false // inside a def/class header line (skip call detection)

	inDefClass := func() bool {
		for _, b := range defClass {
			if b {
				return true
			}
		}
		return false
	}

	i := 0
	for i < len(toks) {
		t := toks[i]
		switch t.Kind {
		case TokenEOF:
			i++

		case TokenNewline:
			atStmt = true
			inHeader = false
			i++

		case TokenIndent:
			defClass = append(defClass, pendingDefClass)
			pendingDefClass = false
			atStmt = true
			inHeader = false
			i++

		case TokenDedent:
			if len(defClass) > 0 {
				defClass = defClass[:len(defClass)-1]
			}
			atStmt = true
			inHeader = false
			i++

		case TokenName:
			if atStmt {
				atStmt = false
				switch t.Value {
				case "import":
					i = parseImport(toks, i+1, m)
					continue
				case "from":
					i = parseFrom(toks, i+1, m)
					continue
				case "def", "class":
					pendingDefClass = true
					inHeader = true
					i++
					continue
				}
			}
			if !inHeader {
				name, next, ok := scanDotted(toks, i)
				// A NAME whose immediately-preceding token is '.' is an
				// attribute of an expression result (foo().eval(),
				// arr[0].compile()) — not the bare builtin. Prefix with
				// '.' so isDynamicEval (bare-only) and the dotted-suffix
				// catalogs treat it as the method call it is.
				if i > 0 && toks[i-1].Kind == TokenOp && toks[i-1].Value == "." {
					name = "." + name
				}
				if ok && isCallOpen(toks, next) && !soleKeyword(name) {
					m.Calls = append(m.Calls, Call{
						Callee:      name,
						ModuleScope: !inDefClass(),
						Line:        t.Line,
						FirstArg:    resolveFirstArg(toks, next),
					})
					i = next // resume at '(' so nested-arg calls are still seen
					continue
				}
			}
			i++

		default:
			if t.Kind == TokenOp && t.Value == "^=" {
				m.XorAssigns++
			}
			atStmt = false
			i++
		}
	}
	return m, nil
}

// scanDotted reads NAME ('.' NAME)* starting at i and returns the
// dotted string, the index just past it, and whether a NAME was
// present.
func scanDotted(toks []Token, i int) (name string, next int, ok bool) {
	if i >= len(toks) || toks[i].Kind != TokenName {
		return "", i, false
	}
	var b strings.Builder
	b.WriteString(toks[i].Value)
	j := i + 1
	for j+1 < len(toks) &&
		toks[j].Kind == TokenOp && toks[j].Value == "." &&
		toks[j+1].Kind == TokenName {
		b.WriteByte('.')
		b.WriteString(toks[j+1].Value)
		j += 2
	}
	return b.String(), j, true
}

func isCallOpen(toks []Token, i int) bool {
	return i < len(toks) && toks[i].Kind == TokenOp && toks[i].Value == "("
}

func soleKeyword(dotted string) bool {
	if strings.Contains(dotted, ".") {
		return false
	}
	_, isKw := pyKeywords[dotted]
	return isKw
}

// parseImport handles `import a.b [as x][, c.d ...]`. i points just
// past the `import` keyword. Returns the index at/after the
// statement's terminator.
func parseImport(toks []Token, i int, m *Module) int {
	for i < len(toks) {
		name, next, ok := scanDotted(toks, i)
		if !ok {
			break
		}
		m.Imports = append(m.Imports, name)
		i = next
		if i < len(toks) && toks[i].Kind == TokenName && toks[i].Value == "as" {
			i += 2 // skip `as alias`
		}
		if i < len(toks) && toks[i].Kind == TokenOp && toks[i].Value == "," {
			i++
			continue
		}
		break
	}
	return skipToStmtEnd(toks, i)
}

// parseFrom handles `from a.b import x [as y], z` and the
// parenthesized / star forms. i points just past the `from` keyword.
func parseFrom(toks []Token, i int, m *Module) int {
	mod, next, ok := scanDotted(toks, i)
	if !ok {
		return skipToStmtEnd(toks, i)
	}
	i = next
	if i < len(toks) && toks[i].Kind == TokenName && toks[i].Value == "import" {
		i++
	}
	for i < len(toks) {
		tk := toks[i]
		if tk.Kind == TokenNewline || tk.Kind == TokenEOF {
			break
		}
		switch {
		case tk.Kind == TokenName && tk.Value == "as":
			i += 2 // skip `as alias`
		case tk.Kind == TokenName:
			m.Imports = append(m.Imports, mod+"."+tk.Value)
			i++
		default: // commas, parens, '*'
			i++
		}
	}
	return skipToStmtEnd(toks, i)
}

// pathJoinCallees / pathPassthroughCallees are the path-builders the
// resolver folds: join concatenates with "/"; passthrough returns its
// single argument unchanged (expanduser keeps '~', expandvars keeps
// $VARS — the sensitive-path patterns match on the literal segments).
var (
	pathJoinCallees = map[string]struct{}{
		"os.path.join": {}, "posixpath.join": {}, "ntpath.join": {},
	}
	pathPassthroughCallees = map[string]struct{}{
		"os.path.expanduser": {}, "os.path.expandvars": {}, "os.fspath": {},
		"posixpath.expanduser": {}, "pathlib.Path": {}, "str": {},
	}
)

// resolveFirstArg returns the statically-resolved first positional
// argument of the call whose '(' is at openIdx, or "" if it can't be
// resolved without executing code.
func resolveFirstArg(toks []Token, openIdx int) string {
	args, _, ok := splitCallArgs(toks[openIdx:])
	if !ok || len(args) == 0 {
		return ""
	}
	s, resolved := resolveExpr(args[0])
	if !resolved {
		return ""
	}
	return s
}

// splitCallArgs takes a token slice whose first token is '(' and
// returns the top-level comma-separated argument token slices, the
// index (relative to the input) of the matching ')', and whether the
// call was well-formed (balanced). Nested brackets are tracked so
// commas inside them don't split arguments.
func splitCallArgs(toks []Token) (args [][]Token, closeIdx int, ok bool) {
	if len(toks) == 0 || toks[0].Kind != TokenOp || toks[0].Value != "(" {
		return nil, 0, false
	}
	depth := 0
	var cur []Token
	for idx, tk := range toks {
		if tk.Kind == TokenOp {
			switch tk.Value {
			case "(", "[", "{":
				depth++
				if depth == 1 {
					continue // skip the outer '('
				}
			case ")", "]", "}":
				depth--
				if depth == 0 {
					if len(cur) > 0 || len(args) > 0 {
						args = append(args, cur)
					}
					return args, idx, true
				}
			case ",":
				if depth == 1 {
					args = append(args, cur)
					cur = nil
					continue
				}
			}
		}
		if depth >= 1 {
			cur = append(cur, tk)
		}
	}
	return args, 0, false // unterminated
}

// resolveExpr statically evaluates one argument expression to a
// string. Handles string-literal sequences (implicit concatenation)
// and a small set of path-builder calls; everything else is
// unresolved by design.
func resolveExpr(toks []Token) (string, bool) {
	if len(toks) == 0 {
		return "", false
	}

	allStrings := true
	for _, tk := range toks {
		if tk.Kind != TokenString {
			allStrings = false
			break
		}
	}
	if allStrings {
		var b strings.Builder
		for _, tk := range toks {
			s, ok := unquotePyString(tk.Value)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		}
		return b.String(), true
	}

	name, after, ok := scanDotted(toks, 0)
	if !ok || after >= len(toks) || toks[after].Kind != TokenOp || toks[after].Value != "(" {
		return "", false
	}
	callArgs, closeIdx, wellFormed := splitCallArgs(toks[after:])
	if !wellFormed || after+closeIdx != len(toks)-1 {
		// The call must span the whole expression; trailing tokens
		// (e.g. `join(...) + x`) make it non-static.
		return "", false
	}
	if _, isJoin := pathJoinCallees[name]; isJoin {
		parts := make([]string, 0, len(callArgs))
		for _, a := range callArgs {
			s, ok := resolveExpr(a)
			if !ok {
				return "", false
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, "/"), true
	}
	if _, isPass := pathPassthroughCallees[name]; isPass && len(callArgs) == 1 {
		return resolveExpr(callArgs[0])
	}
	return "", false
}

// unquotePyString strips the optional prefix and surrounding quotes
// from a STRING token's raw source. An f-string with a `{`
// interpolation is treated as unresolved (its value depends on
// runtime state). Escape sequences are intentionally left raw — the
// sensitive-path patterns match on path segments, not on bytes that
// would be escape-encoded.
func unquotePyString(v string) (string, bool) {
	i := 0
	for i < len(v) && stringPrefixByte(v[i]) {
		i++
	}
	prefix := strings.ToLower(v[:i])
	body := v[i:]

	var inner string
	switch {
	case len(body) >= 6 && (strings.HasPrefix(body, `"""`) || strings.HasPrefix(body, "'''")):
		inner = body[3 : len(body)-3]
	case len(body) >= 2 && (body[0] == '"' || body[0] == '\''):
		inner = body[1 : len(body)-1]
	default:
		return "", false
	}
	if strings.Contains(prefix, "f") && strings.Contains(inner, "{") {
		return "", false
	}
	return inner, true
}

// skipToStmtEnd advances to the next NEWLINE (or EOF) and stops
// there, leaving that terminator for the main loop to consume so it
// re-arms statement-start detection for the following line.
func skipToStmtEnd(toks []Token, i int) int {
	for i < len(toks) {
		switch toks[i].Kind {
		case TokenNewline, TokenEOF:
			return i
		}
		i++
	}
	return i
}
