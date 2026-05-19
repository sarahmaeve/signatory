package node

import "strings"

// Module is the parsed view of one JS/TS source file, reduced to the
// constructs the trust extractor cares about. NOT a full AST: imports,
// call sites with scope, the XOR-deobfuscation primitive count.
// Anything the security catalog does not need is dropped.
type Module struct {
	// Imports is every module specifier seen via `import ... from
	// 'm'`, `import 'm'`, `require('m')`, or dynamic `import('m')`.
	// Recorded for completeness; catalog matching is on call callees,
	// not imports (mirrors the python analyzer).
	Imports []string

	// Calls is every call site, with the scope discriminator the
	// supply-chain model hinges on (ModuleScope == runs at
	// import/require time).
	Calls []Call

	// XorAssigns counts `^=` augmented assignments — the canonical
	// XOR-deobfuscation loop primitive. Parity with the Go/Python
	// analyzers (binary `^` in a plain `=` is the same documented
	// gap there).
	XorAssigns int
}

// Call is one call site: the (alias-resolved) dotted callee, whether
// it is lexically at module scope, its line, and the statically
// resolved first argument.
type Call struct {
	// Callee is the dotted call target after import/require alias
	// resolution: `cp.exec` becomes `child_process.exec` when
	// `cp` was bound by `require('child_process')`. `new Function(`
	// records `Function`. Optional chaining (`a?.b()`) is flattened
	// to `a.b`.
	Callee      string
	ModuleScope bool
	Line        int

	// FirstArg is the statically-resolved first positional argument,
	// or "" when it can't be resolved without execution. Resolves
	// string literals, `+` concatenation of literals, and
	// path.join/path.resolve of resolvable parts. Template literals
	// with interpolation, names, and call results are unresolved by
	// design (documented conservative gap). Feeds SensitivePathReads.
	FirstArg string

	// SecondArg is the statically-resolved second positional argument
	// (same resolver/limits as FirstArg), or "". Needed because the
	// dominant npm payload-decode primitive — Buffer.from(data,
	// 'base64') — encodes the "this is a decode" intent in its
	// SECOND argument, not the first. Feeds Base64DecodeCalls for the
	// Buffer.from case.
	SecondArg string
}

// jsNonCallKeywords are names that, standing alone before '(', are a
// control-flow head, not a call (`if (x)`, `for (...)`, `switch (x)`,
// `function (...)`). A sole-segment callee that is one of these is
// skipped; multi-segment callees can't be keywords.
var jsNonCallKeywords = map[string]struct{}{
	"if": {}, "for": {}, "while": {}, "switch": {}, "catch": {},
	"with": {}, "return": {}, "function": {}, "typeof": {}, "await": {},
	"throw": {}, "in": {}, "of": {}, "do": {}, "else": {}, "yield": {},
	"void": {}, "delete": {}, "case": {}, "instanceof": {},
}

// Adversarial-input work bounds — same rationale as the python
// analyzer: signatory parses untrusted source up to the 10 MiB
// BlobStreamer cap, so a crafted deep nest must degrade to a
// conservative miss, never O(n^2) or a stack overflow.
const (
	maxArgScanTokens = 256
	maxResolveDepth  = 64
)

// scopeFrame is one `{ }` nesting level. isFunc marks a function /
// arrow / method body (its calls do NOT run at import time); isClass
// marks a class body (so a `) {` directly inside it is a method body).
type scopeFrame struct {
	isFunc  bool
	isClass bool
}

// Parse lexes then walks the token stream. Lenient like the lexer:
// malformed input yields a best-effort partial Module, never an error
// abort, because a trust extractor must keep producing signal on
// adversarial source.
func Parse(src []byte) (*Module, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	m := &Module{}
	p := &parser{toks: toks, m: m, alias: map[string]string{}}
	p.run()
	return m, nil
}

type parser struct {
	toks  []Token
	m     *Module
	alias map[string]string // local name → module-qualified prefix

	frames       []scopeFrame
	pendingFunc  bool // next '{' is a function/arrow body
	pendingClass bool // next '{' is a class body
}

func (p *parser) inFunc() bool {
	for _, f := range p.frames {
		if f.isFunc {
			return true
		}
	}
	return false
}

func (p *parser) innermostIsClass() bool {
	return len(p.frames) > 0 && p.frames[len(p.frames)-1].isClass
}

func (p *parser) run() {
	i := 0
	for i < len(p.toks) {
		t := p.toks[i]

		if t.Kind == TokenOp {
			switch t.Value {
			case "^=":
				p.m.XorAssigns++
				i++
				continue
			case "=>":
				p.pendingFunc = true
				i++
				continue
			case "{":
				p.pushBrace(i)
				i++
				continue
			case "}":
				if len(p.frames) > 0 {
					p.frames = p.frames[:len(p.frames)-1]
				}
				i++
				continue
			}
			i++
			continue
		}

		if t.Kind != TokenName {
			i++
			continue
		}

		// Names not preceded by '.' / '?.' can introduce a binding
		// statement or be a keyword influencing scope.
		precededByDot := i > 0 && p.toks[i-1].Kind == TokenOp &&
			(p.toks[i-1].Value == "." || p.toks[i-1].Value == "?.")

		if !precededByDot {
			switch t.Value {
			case "function":
				p.pendingFunc = true
				i++
				continue
			case "class":
				p.pendingClass = true
				i++
				continue
			case "import":
				if next, ok := p.parseImport(i); ok {
					i = next
					continue
				}
			case "const", "let", "var":
				if next, ok := p.parseBinding(i + 1); ok {
					i = next
					continue
				}
			case "require":
				if next, ok := p.parseRequireExpr(i, ""); ok {
					i = next
					continue
				}
			case "new":
				if next, ok := p.parseNew(i); ok {
					i = next
					continue
				}
			}
		}

		// General call detection: DOTTED '(' .
		name, next, ok := scanDotted(p.toks, i)
		if ok && isCallOpen(p.toks, next) && !soleNonCall(name) {
			// Declaration, not a call: `NAME ( params ) {` is a
			// function/method/getter definition (the body brace
			// follows the param list directly). A real call's ')' is
			// followed by ';' / '.' / ')' / ',' etc., never '{' — an
			// arrow-body '{' lives *inside* the call's parens. Skip
			// the name (don't record a call) and mark the upcoming
			// body as a function scope so its inner calls aren't
			// counted as import-time. Mirrors the python analyzer's
			// def-header handling.
			if _, closeIdx, balanced := splitCallArgs(p.toks[next:]); balanced {
				if after := next + closeIdx + 1; after < len(p.toks) &&
					p.toks[after].Kind == TokenOp && p.toks[after].Value == "{" {
					p.pendingFunc = true
					i++
					continue
				}
			}
			// A NAME reached via '.'/'?.' is a method on an expression
			// result (db.query(q).eval(), buf.toString()) — not a bare
			// builtin and not an import alias. Prefix '.' so the
			// bare-only dynamic-eval check and the dotted-suffix
			// catalogs treat it as the method call it is, never as
			// eval/exec/etc. (mirrors the python analyzer).
			callee := "." + name
			if !precededByDot {
				callee = p.resolveAlias(name)
			}
			p.m.Calls = append(p.m.Calls, Call{
				Callee:      callee,
				ModuleScope: !p.inFunc(),
				Line:        t.Line,
				FirstArg:    resolveFirstArg(p.toks, next),
				SecondArg:   resolveArgN(p.toks, next, 1),
			})
			i = next
			continue
		}
		i++
	}
}

// pushBrace decides what kind of scope frame a '{' at index i opens.
// Order matters: a class body opener wins over the "method body"
// heuristic, and an explicit function/arrow marker wins over a plain
// block.
func (p *parser) pushBrace(i int) {
	switch {
	case p.pendingFunc:
		p.frames = append(p.frames, scopeFrame{isFunc: true})
		p.pendingFunc = false
		p.pendingClass = false
	case p.pendingClass:
		p.frames = append(p.frames, scopeFrame{isClass: true})
		p.pendingClass = false
	case p.innermostIsClass() && i > 0 && p.toks[i-1].Kind == TokenOp && p.toks[i-1].Value == ")":
		// `member(...) {` directly inside a class body — a method,
		// getter/setter, or constructor body.
		p.frames = append(p.frames, scopeFrame{isFunc: true})
	default:
		p.frames = append(p.frames, scopeFrame{})
	}
}

// parseImport handles the ESM import forms and records bindings into
// the alias map. i points at the `import` keyword. Returns the index
// just past the specifier and true when this was a static import;
// false (and no consumption) for dynamic `import(` so the caller can
// treat it as an expression.
func (p *parser) parseImport(i int) (int, bool) {
	// Dynamic import( ... ) — not a static import statement.
	if isCallOpen(p.toks, i+1) {
		if spec, _, ok := stringArg(p.toks, i+1); ok {
			p.m.Imports = append(p.m.Imports, spec)
		}
		return i + 1, false
	}

	// Find `from 'mod'` and the local bindings between import..from.
	j := i + 1
	var locals []Token
	spec := ""
	for j < len(p.toks) {
		tk := p.toks[j]
		if tk.Kind == TokenName && tk.Value == "from" {
			if j+1 < len(p.toks) && p.toks[j+1].Kind == TokenString {
				s, _ := unquoteJS(p.toks[j+1].Value)
				spec = normalizeSpec(s)
				j += 2
			}
			break
		}
		if tk.Kind == TokenString { // `import 'side-effect'`
			s, _ := unquoteJS(tk.Value)
			spec = normalizeSpec(s)
			j++
			break
		}
		if tk.Kind == TokenOp && tk.Value == ";" {
			break
		}
		locals = append(locals, tk)
		j++
	}
	if spec != "" {
		p.m.Imports = append(p.m.Imports, spec)
		p.bindImportLocals(locals, spec)
	}
	return j, true
}

// bindImportLocals maps the names between `import` and `from` to
// module-qualified prefixes. Handles: default (`x`), namespace
// (`* as ns`), and named (`{ a, b as c }`) — mixed forms included.
func (p *parser) bindImportLocals(locals []Token, spec string) {
	k := 0
	for k < len(locals) {
		tk := locals[k]
		switch {
		case tk.Kind == TokenOp && tk.Value == "{":
			// Named group until '}'.
			k++
			for k < len(locals) && (locals[k].Kind != TokenOp || locals[k].Value != "}") {
				if locals[k].Kind == TokenName {
					orig := locals[k].Value
					local := orig
					if k+2 < len(locals) && locals[k+1].Kind == TokenName &&
						locals[k+1].Value == "as" && locals[k+2].Kind == TokenName {
						local = locals[k+2].Value
						k += 2
					}
					p.alias[local] = spec + "." + orig
				}
				k++
			}
		case tk.Kind == TokenName && tk.Value == "as":
			// `* as ns`
			if k+1 < len(locals) && locals[k+1].Kind == TokenName {
				p.alias[locals[k+1].Value] = spec
				k++
			}
		case tk.Kind == TokenName:
			// Default import binding.
			p.alias[tk.Value] = spec
		}
		k++
	}
}

// parseBinding handles `const/let/var <target> = require('m')` and the
// destructured form. i points just past the declaration keyword.
// Returns (next, true) when it consumed a require binding; (i, false)
// otherwise so the normal walk continues (e.g. `const x = 1`).
func (p *parser) parseBinding(i int) (int, bool) {
	if i >= len(p.toks) {
		return i, false
	}
	// Destructured: `{ a, b } = require('m')`
	if p.toks[i].Kind == TokenOp && p.toks[i].Value == "{" {
		close := matchBrace(p.toks, i)
		if close < 0 {
			return i, false
		}
		eq := close + 1
		spec, end, ok := requireAfterEq(p.toks, eq)
		if !ok {
			return i, false
		}
		for k := i + 1; k < close; k++ {
			if p.toks[k].Kind == TokenName && p.toks[k].Value != "as" {
				p.alias[p.toks[k].Value] = spec + "." + p.toks[k].Value
			}
		}
		p.m.Imports = append(p.m.Imports, spec)
		return end, true
	}
	// Simple: `NAME = require('m')`
	if p.toks[i].Kind == TokenName {
		name := p.toks[i].Value
		spec, end, ok := requireAfterEq(p.toks, i+1)
		if !ok {
			return i, false
		}
		p.alias[name] = spec
		p.m.Imports = append(p.m.Imports, spec)
		return end, true
	}
	return i, false
}

// requireAfterEq matches `= require('m')` starting at index eq and
// returns the module spec and the index just past the `)`.
func requireAfterEq(toks []Token, eq int) (spec string, end int, ok bool) {
	if eq >= len(toks) || toks[eq].Kind != TokenOp || toks[eq].Value != "=" {
		return "", 0, false
	}
	j := eq + 1
	// Tolerate `await` before require/import.
	if j < len(toks) && toks[j].Kind == TokenName && toks[j].Value == "await" {
		j++
	}
	if j >= len(toks) || toks[j].Kind != TokenName ||
		(toks[j].Value != "require" && toks[j].Value != "import") {
		return "", 0, false
	}
	s, close, ok := stringArg(toks, j+1)
	if !ok {
		return "", 0, false
	}
	// stringArg already normalized the spec.
	return s, close + 1, true
}

// parseRequireExpr handles a `require('m')` expression: records the
// import, and if it is chained (`require('m').method(...)`) records
// the resulting <m>.<method...> call so the catalog can match the
// dominant inline-dropper shape. i points at `require`. lead is an
// optional already-resolved receiver prefix (unused today; kept for
// symmetry).
func (p *parser) parseRequireExpr(i int, _ string) (int, bool) {
	spec, close, ok := stringArg(p.toks, i+1)
	if !ok {
		return i, false
	}
	p.m.Imports = append(p.m.Imports, spec)
	j := close + 1
	// Chained: `.name (.name)* (`
	if j < len(p.toks) && p.toks[j].Kind == TokenOp &&
		(p.toks[j].Value == "." || p.toks[j].Value == "?.") {
		rest, next, dok := scanDotted(p.toks, j+1)
		if dok && isCallOpen(p.toks, next) {
			p.m.Calls = append(p.m.Calls, Call{
				Callee:      spec + "." + rest,
				ModuleScope: !p.inFunc(),
				Line:        p.toks[i].Line,
				FirstArg:    resolveFirstArg(p.toks, next),
				SecondArg:   resolveArgN(p.toks, next, 1),
			})
			return next, true
		}
	}
	return j, true
}

// parseNew handles `new <dotted>(...)` — records the constructor as a
// call with the dotted name as callee (so `new Function(` → Function,
// the dynamic-code primitive). i points at `new`.
func (p *parser) parseNew(i int) (int, bool) {
	name, next, ok := scanDotted(p.toks, i+1)
	if !ok || !isCallOpen(p.toks, next) {
		return i, false
	}
	p.m.Calls = append(p.m.Calls, Call{
		Callee:      p.resolveAlias(name),
		ModuleScope: !p.inFunc(),
		Line:        p.toks[i].Line,
		FirstArg:    resolveFirstArg(p.toks, next),
		SecondArg:   resolveArgN(p.toks, next, 1),
	})
	return next, true
}

// resolveAlias rewrites the leading segment of a dotted callee through
// the import/require alias map. The replacement may itself be dotted
// (`execSync` → `child_process.execSync`).
func (p *parser) resolveAlias(dotted string) string {
	head := dotted
	tail := ""
	if dot := strings.IndexByte(dotted, '.'); dot >= 0 {
		head = dotted[:dot]
		tail = dotted[dot:]
	}
	if repl, ok := p.alias[head]; ok {
		return repl + tail
	}
	return dotted
}

// scanDotted reads NAME (('.'|'?.') NAME)* and returns the
// dot-joined string, the index just past it, and whether a NAME was
// present.
func scanDotted(toks []Token, i int) (name string, next int, ok bool) {
	if i >= len(toks) || toks[i].Kind != TokenName {
		return "", i, false
	}
	var b strings.Builder
	b.WriteString(toks[i].Value)
	j := i + 1
	for j+1 < len(toks) &&
		toks[j].Kind == TokenOp && (toks[j].Value == "." || toks[j].Value == "?.") &&
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

func soleNonCall(dotted string) bool {
	if strings.Contains(dotted, ".") {
		return false
	}
	_, isKw := jsNonCallKeywords[dotted]
	return isKw
}

// matchBrace returns the index of the '}' matching the '{' at i, or
// -1 if unbalanced within the adversarial scan bound.
func matchBrace(toks []Token, i int) int {
	depth := 0
	for j := i; j < len(toks) && j-i < maxArgScanTokens; j++ {
		if toks[j].Kind != TokenOp {
			continue
		}
		switch toks[j].Value {
		case "{":
			depth++
		case "}":
			depth--
			if depth == 0 {
				return j
			}
		}
	}
	return -1
}

// stringArg expects toks[i] == '(' followed by a single string
// literal then ')'. Returns the unquoted spec and the index of the
// ')'. Used for require/import specifiers.
func stringArg(toks []Token, i int) (spec string, closeIdx int, ok bool) {
	if !isCallOpen(toks, i) {
		return "", 0, false
	}
	if i+2 < len(toks) && toks[i+1].Kind == TokenString &&
		toks[i+2].Kind == TokenOp && toks[i+2].Value == ")" {
		if s, sok := unquoteJS(toks[i+1].Value); sok {
			return normalizeSpec(s), i + 2, true
		}
	}
	return "", 0, false
}

// resolveFirstArg returns the statically-resolved first positional
// argument of the call whose '(' is at openIdx, or "".
func resolveFirstArg(toks []Token, openIdx int) string {
	return resolveArgN(toks, openIdx, 0)
}

// resolveArgN returns the statically-resolved n-th (0-based)
// positional argument of the call whose '(' is at openIdx, or "".
// Same resolver and adversarial bounds as resolveFirstArg.
func resolveArgN(toks []Token, openIdx, n int) string {
	args, _, ok := splitCallArgs(toks[openIdx:])
	if !ok || n >= len(args) {
		return ""
	}
	s, resolved := resolveExpr(args[n], 0)
	if !resolved {
		return ""
	}
	return s
}

// splitCallArgs takes a slice whose first token is '(' and returns
// the top-level comma-separated argument token slices, the index of
// the matching ')', and whether it was balanced within the
// adversarial bound. Nested (), [], {} are tracked so commas inside
// them don't split arguments.
func splitCallArgs(toks []Token) (args [][]Token, closeIdx int, ok bool) {
	if len(toks) == 0 || toks[0].Kind != TokenOp || toks[0].Value != "(" {
		return nil, 0, false
	}
	depth := 0
	var cur []Token
	for idx, tk := range toks {
		if idx >= maxArgScanTokens {
			return nil, 0, false
		}
		if tk.Kind == TokenOp {
			switch tk.Value {
			case "(", "[", "{":
				depth++
				if depth == 1 {
					continue
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
	return args, 0, false
}

// pathJoinCallees fold to a '/'-joined path when every argument
// resolves; mirrors the python analyzer's os.path.join handling.
var pathJoinCallees = map[string]struct{}{
	"path.join": {}, "path.resolve": {}, "join": {}, "resolve": {},
}

// resolveExpr statically evaluates one argument expression to a
// string: a single string literal, `+` concatenation of resolvable
// parts, or a path.join/resolve of resolvable parts. Everything else
// (names, call results, template interpolation) is unresolved by
// design.
func resolveExpr(toks []Token, depth int) (string, bool) {
	if depth > maxResolveDepth || len(toks) == 0 {
		return "", false
	}

	// `+` concatenation of resolvable operands (also the single-
	// literal case: one operand, no '+').
	parts := splitTopLevel(toks, "+")
	if len(parts) > 1 {
		var b strings.Builder
		for _, part := range parts {
			s, ok := resolveExpr(part, depth+1)
			if !ok {
				return "", false
			}
			b.WriteString(s)
		}
		return b.String(), true
	}

	if len(toks) == 1 && toks[0].Kind == TokenString {
		return unquoteJS(toks[0].Value)
	}

	// path.join(...) / path.resolve(...)
	name, after, ok := scanDotted(toks, 0)
	if !ok || after >= len(toks) || toks[after].Kind != TokenOp || toks[after].Value != "(" {
		return "", false
	}
	if _, isJoin := pathJoinCallees[name]; !isJoin {
		return "", false
	}
	callArgs, closeIdx, wf := splitCallArgs(toks[after:])
	if !wf || after+closeIdx != len(toks)-1 {
		return "", false
	}
	segs := make([]string, 0, len(callArgs))
	for _, a := range callArgs {
		s, ok := resolveExpr(a, depth+1)
		if !ok {
			return "", false
		}
		segs = append(segs, s)
	}
	return strings.Join(segs, "/"), true
}

// splitTopLevel splits a token slice on a top-level binary operator
// (depth 0 w.r.t. (), [], {}), returning the operand slices. A single
// operand (no split) returns one element.
func splitTopLevel(toks []Token, op string) [][]Token {
	var out [][]Token
	var cur []Token
	depth := 0
	for _, tk := range toks {
		if tk.Kind == TokenOp {
			switch tk.Value {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case op:
				if depth == 0 {
					out = append(out, cur)
					cur = nil
					continue
				}
			}
		}
		cur = append(cur, tk)
	}
	out = append(out, cur)
	return out
}

// normalizeSpec canonicalizes a module specifier so the catalogs
// match regardless of how the import was written. Today it strips the
// `node:` builtin scheme (`node:child_process` → `child_process`) so
// ESM builtin imports resolve like the bare names the catalogs use.
func normalizeSpec(spec string) string {
	return strings.TrimPrefix(spec, "node:")
}

// unquoteJS strips the surrounding quotes from a STRING token. A
// template literal containing `${` interpolation is unresolved (its
// value depends on runtime state) — the conservative-miss contract.
// Escape sequences are left raw: the sensitive-path patterns match on
// path segments, not on bytes an escape would encode.
func unquoteJS(v string) (string, bool) {
	if len(v) < 2 {
		return "", false
	}
	switch v[0] {
	case '\'', '"':
		if v[len(v)-1] != v[0] {
			return "", false
		}
		return v[1 : len(v)-1], true
	case '`':
		if v[len(v)-1] != '`' {
			return "", false
		}
		inner := v[1 : len(v)-1]
		if strings.Contains(inner, "${") {
			return "", false
		}
		return inner, true
	}
	return "", false
}
