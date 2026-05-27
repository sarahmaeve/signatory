package rust

import (
	"slices"
	"strings"
)

// Module is the parsed view of one Rust source file, reduced to the
// constructs the trust extractor cares about. NOT a full AST: use
// imports (path + alias), call sites with their enclosing fn name,
// the XOR-deobfuscation primitive count. Anything the security
// catalog does not need is dropped (AST.md §4 "subset not grammar").
type Module struct {
	// Uses is every `use a::b::c [as d];` statement and the
	// individual entries of every `use a::b::{c, d, e};` group. Glob
	// imports (`use a::b::*;`) and nested groups (`use a::{b::c}`)
	// are documented conservative misses; they're rare in Rust code
	// outside of preludes and modelling them adds parser surface for
	// little signal.
	Uses []Use

	// Calls is every callable site found by the parser, with the
	// scope discriminator the analyzer uses to populate
	// ImportTimeCallSites (calls inside `fn main()` of `build.rs`).
	Calls []Call

	// XorAssigns counts `^=` augmented assignments — the canonical
	// XOR-deobfuscation loop primitive (the Trapdoor cargo payload's
	// dominant obfuscation step, design/threat-landscape/
	// 2026-05-24-trapdoor-crypto-stealer.md). Parity with the Go,
	// Python, and Node analyzers (binary `^` in a plain `=`
	// assignment is the same documented gap there).
	XorAssigns int
}

// Use is one `use` binding. Path is the dotted-by-`::` path; Alias
// is the name the caller binds the import to (the last path segment
// if no `as` clause, the `as` target otherwise).
type Use struct {
	// Path is the path segments: ["std", "env", "var"] for
	// `use std::env::var;`. For grouped imports each entry is its
	// own Use record with the group prefix duplicated:
	// `use std::env::{var, set_var};` → two Uses, both with
	// Path[:2] == ["std", "env"].
	Path []string

	// Alias is the name the path binds to in this scope. For
	// `use std::env::var as v;` → Alias is "v". For
	// `use std::env::var;` → Alias is "var" (the last segment).
	// For `use std::env;` → Alias is "env".
	Alias string
}

// Call is one call site: the (alias-resolved) `::`-or-`.`-separated
// callee, its enclosing fn name (or "" for top-level), whether it's a
// macro call, the line, and the statically-resolved first/second
// positional arguments.
//
// Callee carries the source-syntactic form preserved: `::` between
// module-path segments, `.` between method-chain segments. The
// analyzer matches Callee against catalogs that record both forms.
// The leading segment is alias-resolved against Uses; trailing
// segments are not (the alias resolver is path-prefix-only).
type Call struct {
	// Callee is the (alias-resolved-at-the-head) source-syntactic
	// form of the call target. Examples:
	//   - "std::env::var"
	//   - "Command::new"
	//   - "fs::write"      (after `use std::fs;`)
	//   - "cmd.arg"        (method call on a name; the analyzer
	//                       can't resolve method-chain catalog
	//                       matches without receiver flow — gap)
	Callee string

	// InFn is the name of the enclosing fn definition (e.g. "main",
	// "foo"), or "" for a call outside any fn (mostly impossible in
	// Rust — calls at top-level appear inside static initializers,
	// which we don't track separately).
	InFn string

	// Macro is true if this is a macro invocation (e.g. `vec![1]`,
	// `println!("x")`, `thread_local! { ... }`). Macro args are
	// opaque — FirstArg/SecondArg are populated only when the
	// opener is `(` AND the first/second arg resolves to a literal.
	Macro bool

	// FirstArg is the statically-resolved first positional argument
	// (string literal or simple concatenation). Empty when not
	// resolvable. Feeds SensitivePathReads, EnvCredentialReads, etc.
	FirstArg string

	// SecondArg is the same for the second positional argument.
	// `std::fs::write(path, data)` and `OpenOptions::new()...`
	// shapes care about the second arg (the data) in some catalogs.
	SecondArg string

	Line int
}

// Adversarial-input work bounds — same rationale as the python and
// node analyzers: signatory parses untrusted source up to the 10 MiB
// BlobStreamer cap, so a crafted deep nest must degrade to a
// conservative miss, never O(n^2) or a stack overflow.
const (
	// maxBraceDepth caps the brace-nesting stack to keep
	// pathological inputs from heap-allocating an unbounded fn
	// stack. 256 matches node's maxArgScanTokens / template depth.
	maxBraceDepth = 1024
	// maxPathSegments caps the number of `::`-separated segments a
	// single use-statement or call path can contain. Real Rust
	// rarely exceeds 8; 64 is generous.
	maxPathSegments = 64
)

// rustNonCallKeywords are bare names that, even when immediately
// followed by `(` or `::`, are not call sites — control flow,
// `if`/`while`/`match` conditions, primitive type names appearing in
// turbofish (`<i32>`), and the boolean literals. A multi-segment
// callee whose leading segment is one of these is still a call
// (e.g. `match::not_a_real_thing()` — pathological — we still
// record it, but the parser is lenient).
var rustNonCallKeywords = map[string]struct{}{
	"if": {}, "else": {}, "for": {}, "while": {}, "loop": {},
	"match": {}, "return": {}, "break": {}, "continue": {},
	"fn": {}, "let": {}, "mut": {}, "ref": {}, "move": {},
	"unsafe": {}, "async": {}, "await": {}, "yield": {},
	"impl": {}, "trait": {}, "struct": {}, "enum": {}, "union": {},
	"type": {}, "mod": {}, "use": {}, "pub": {}, "crate": {},
	"extern": {}, "static": {}, "const": {}, "in": {},
	"where": {}, "as": {}, "dyn": {}, "true": {}, "false": {},
	"self": {}, "Self": {}, "super": {},
}

// Parse lexes then walks the token stream. Lenient like the lexer:
// malformed input yields a best-effort partial Module, never an
// error abort, because a trust extractor must keep producing signal
// on adversarial source (AST.md §4).
func Parse(src []byte) (*Module, error) {
	toks, err := Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{
		toks:     toks,
		mod:      &Module{},
		useIndex: make(map[string][]string),
	}
	p.run()
	return p.mod, nil
}

type parser struct {
	toks []Token
	i    int
	mod  *Module

	// useIndex is the alias→path lookup table populated as
	// recordUse runs. assembleCallee consults this in O(1) per call
	// site; the alternative — scanning p.mod.Uses linearly per call
	// — is O(N*M) in N use-statements + M call sites and falls over
	// on alias-bomb adversarial input
	// (robustness_test.go: TestParse_AdversarialInput "alias bomb").
	//
	// First-wins semantics: a later `use` that shadows the same alias
	// does not overwrite the index entry, matching the original
	// linear scan's `break`-on-first-match behaviour.
	useIndex map[string][]string

	// fnStack tracks the names of enclosing `fn` definitions.
	// Pushed when a `fn name(...) {` is recognized; popped when the
	// matching `}` closes. Bounded by maxBraceDepth defensively.
	fnStack []string

	// frames tracks every open brace, tagging which were fn bodies.
	// Same length as the open-brace stack. Bounded by maxBraceDepth.
	frames []frame
}

// frame is one `{ }` nesting level. fnName is non-empty when this
// brace opened a fn body — at the matching `}` we pop the
// corresponding entry off fnStack.
type frame struct {
	fnName string // empty if this brace did not open a fn body
}

func (p *parser) run() {
	for p.i < len(p.toks) {
		t := p.toks[p.i]

		// Track scope-changing operators before considering name tokens.
		if t.Kind == TokenOp {
			switch t.Value {
			case "{":
				// A brace that wasn't claimed by a `fn name(...)`
				// header just below — record as a non-fn frame.
				p.pushBrace("")
				p.i++
				continue
			case "}":
				p.popBrace()
				p.i++
				continue
			case "^=":
				p.mod.XorAssigns++
				p.i++
				continue
			}
			p.i++
			continue
		}

		if t.Kind != TokenName {
			p.i++
			continue
		}

		// Keyword-handled name tokens.
		switch t.Value {
		case "use":
			if next, ok := p.parseUse(p.i); ok {
				p.i = next
				continue
			}
		case "fn":
			if next, ok := p.parseFnHeader(p.i); ok {
				p.i = next
				continue
			}
		case "macro_rules":
			// macro_rules! bodies describe SYNTAX patterns, not
			// executable code — pattern tokens like `^=` inside
			// the body must not feed XorAssigns / Calls. Skip the
			// entire body and any inner braces.
			//
			// Surfaced by the anyhow dogfood: src/ensure.rs has a
			// macro_rules! arm whose pattern includes the literal
			// `^=` token tree, which previously inflated
			// XorAssigns to 1 on a legit error-handling crate.
			// Parallel to node's "code inside template-literal
			// ${} is not tokenized" gap (AST.md §4).
			if next, ok := p.skipMacroRulesBody(p.i); ok {
				p.i = next
				continue
			}
		}

		// Try call parsing for any non-keyword name token.
		if _, isKw := rustNonCallKeywords[t.Value]; isKw {
			p.i++
			continue
		}
		if next, ok := p.tryParseCall(p.i); ok {
			p.i = next
			continue
		}

		p.i++
	}
}

// pushBrace appends a frame to the brace stack. fnName is non-empty
// when this brace opens a fn body — the matching `}` will pop the
// corresponding entry off fnStack.
func (p *parser) pushBrace(fnName string) {
	if len(p.frames) >= maxBraceDepth {
		// Bounded — drop the frame silently rather than recurse.
		return
	}
	if fnName != "" {
		p.fnStack = append(p.fnStack, fnName)
	}
	p.frames = append(p.frames, frame{fnName: fnName})
}

// popBrace matches a `}`. Lenient: an unbalanced `}` (closing brace
// with no opener) is silently ignored rather than panicking.
func (p *parser) popBrace() {
	if len(p.frames) == 0 {
		return
	}
	top := p.frames[len(p.frames)-1]
	p.frames = p.frames[:len(p.frames)-1]
	if top.fnName != "" && len(p.fnStack) > 0 {
		p.fnStack = p.fnStack[:len(p.fnStack)-1]
	}
}

// currentFn returns the innermost enclosing fn name, or "" if not
// inside any fn body. Used as the InFn marker on emitted Calls.
func (p *parser) currentFn() string {
	if len(p.fnStack) == 0 {
		return ""
	}
	return p.fnStack[len(p.fnStack)-1]
}

// parseUse handles `use a::b::c [as d];` and grouped variants
// `use a::b::{c, d as e};`. Returns the token index past the
// terminating `;` (or the end of the use-statement we recognized)
// and ok=true on success.
//
// Conservative gaps (documented misses):
//   - Glob imports `use a::b::*;` — Alias is left empty; the
//     analyzer ignores Uses with empty Alias.
//   - Nested groups `use a::{b::c, d::e};` — the outer group is
//     walked, but inner `::` paths within group entries aren't
//     re-expanded; we record each entry as-is.
//   - `self` in groups `use a::{self, b};` — `self` is treated as
//     a plain segment (Alias = "self"), which is harmless because
//     no catalog name is literally "self".
func (p *parser) parseUse(i int) (int, bool) {
	if i >= len(p.toks) || p.toks[i].Value != "use" {
		return i, false
	}
	j := i + 1

	// Skip `pub` if present (rare on `use`; allowed grammar).
	if j < len(p.toks) && p.toks[j].Value == "pub" {
		j++
	}

	prefix, after, ok := p.scanUsePath(j)
	if !ok {
		// Not a recognizable use-statement; let the main loop
		// advance.
		return i, false
	}
	j = after

	// Three continuations are possible:
	//   1. `;`            → single import. Alias = last segment.
	//   2. `as <name> ;`  → renamed import.
	//   3. `::{ a, b };`  → grouped import.
	// Anything else is a parse error we eat leniently.
	//
	// scanUsePath stops at the `::` preceding the group's `{` (it
	// requires a NAME after `::`, and `{` is an OP). Step past that
	// trailing `::` here so the group-detection check below fires.
	if j+1 < len(p.toks) && p.toks[j].Value == "::" && p.toks[j+1].Value == "{" {
		j++
	}

	if j < len(p.toks) && p.toks[j].Value == ";" {
		p.recordUse(prefix, lastSegment(prefix))
		return j + 1, true
	}
	if j+1 < len(p.toks) && p.toks[j].Value == "as" && p.toks[j+1].Kind == TokenName {
		alias := p.toks[j+1].Value
		p.recordUse(prefix, alias)
		// Advance past optional `;`.
		k := j + 2
		if k < len(p.toks) && p.toks[k].Value == ";" {
			k++
		}
		return k, true
	}
	if j < len(p.toks) && p.toks[j].Value == "{" {
		// Grouped import — walk each comma-separated entry.
		k := p.parseUseGroup(j+1, prefix)
		// Advance past optional trailing `;`.
		if k < len(p.toks) && p.toks[k].Value == ";" {
			k++
		}
		return k, true
	}
	// Unrecognized continuation — emit what we have with the
	// last-segment alias so the use isn't lost.
	p.recordUse(prefix, lastSegment(prefix))
	return j, true
}

// scanUsePath walks an `a::b::c` path starting at i, returning the
// segments and the token index just past the last segment. Stops at
// the first non-name, non-`::` token. Bounded by maxPathSegments.
func (p *parser) scanUsePath(i int) (segments []string, end int, ok bool) {
	if i >= len(p.toks) || p.toks[i].Kind != TokenName {
		return nil, i, false
	}
	segments = append(segments, p.toks[i].Value)
	j := i + 1
	for j+1 < len(p.toks) && p.toks[j].Value == "::" && p.toks[j+1].Kind == TokenName {
		if len(segments) >= maxPathSegments {
			break
		}
		segments = append(segments, p.toks[j+1].Value)
		j += 2
	}
	return segments, j, true
}

// parseUseGroup walks `name [as alias], name [as alias], ...}` and
// records one Use per entry, prefixed by `prefix`. Returns the token
// index just past the closing `}`. Bounded — gives up at maxPathSegments
// per entry and at the matching `}`. Lenient: unterminated group
// reaches EOF rather than erroring.
func (p *parser) parseUseGroup(i int, prefix []string) int {
	n := len(p.toks)
	j := i
	for j < n {
		if p.toks[j].Value == "}" {
			return j + 1
		}
		if p.toks[j].Value == "," {
			j++
			continue
		}
		// Inside a group, accept a single name or a nested path
		// `a::b::c [as d]`. Nested-into-deeper-group is a
		// documented miss (rare in practice).
		if p.toks[j].Kind == TokenName {
			segs, after, ok := p.scanUsePath(j)
			if !ok {
				j++
				continue
			}
			full := slices.Concat(prefix, segs)
			alias := lastSegment(full)
			j = after
			if j+1 < n && p.toks[j].Value == "as" && p.toks[j+1].Kind == TokenName {
				alias = p.toks[j+1].Value
				j += 2
			}
			p.recordUse(full, alias)
			continue
		}
		j++
	}
	return n
}

// recordUse appends a Use entry. Empty-path/empty-alias is skipped
// — those correspond to malformed input the lenient walk silently
// dropped. Glob imports (`use a::b::*;`) emit an entry with Alias =
// "" so the analyzer can choose to ignore them as a single check.
//
// Also populates p.useIndex with the alias→path mapping so
// assembleCallee can resolve the leading segment in O(1). First-wins:
// if alias is already indexed, the existing entry is preserved.
func (p *parser) recordUse(path []string, alias string) {
	if len(path) == 0 {
		return
	}
	copied := slices.Clone(path)
	p.mod.Uses = append(p.mod.Uses, Use{
		Path:  copied,
		Alias: alias,
	})
	if alias != "" {
		if _, present := p.useIndex[alias]; !present {
			p.useIndex[alias] = copied
		}
	}
}

// lastSegment returns the final element of a path, or "" for empty
// input. Used as the default Alias when no `as` clause is present.
func lastSegment(path []string) string {
	if len(path) == 0 {
		return ""
	}
	return path[len(path)-1]
}

// skipMacroRulesBody recognizes the shape `macro_rules ! NAME {` and
// returns the index just past the matching `}`. Tokens inside the
// body are NOT processed by the main walk — they're macro pattern /
// template syntax, not executable code.
//
// Returns ok=false when the expected `! NAME {` continuation isn't
// present; the caller falls through to ordinary name handling. This
// keeps the recognizer specific to the literal `macro_rules!` shape
// and avoids swallowing real source if a user has a local variable
// happens to be named `macro_rules` (rare but legal — Rust permits
// `macro_rules` as an ordinary identifier outside the
// `macro_rules!` construct).
func (p *parser) skipMacroRulesBody(i int) (int, bool) {
	if i+3 >= len(p.toks) {
		return i, false
	}
	if p.toks[i].Value != "macro_rules" {
		return i, false
	}
	if p.toks[i+1].Value != "!" {
		return i, false
	}
	if p.toks[i+2].Kind != TokenName {
		return i, false
	}
	if p.toks[i+3].Value != "{" {
		return i, false
	}
	// matchBracket consumes the `{` and returns the index just past `}`.
	return matchBracket(p.toks, i+3), true
}

// parseFnHeader recognizes `fn name(...)` or
// `pub fn name<T: Trait>(...)` and pushes a fn frame when the next
// `{` arrives. Returns the index past the `{` and ok=true on
// success.
//
// Lenient: a malformed header (no name, no `(`, mismatched generic
// brackets) yields ok=false and the main loop advances past `fn`
// without pushing a frame.
func (p *parser) parseFnHeader(i int) (int, bool) {
	if i >= len(p.toks) || p.toks[i].Value != "fn" {
		return i, false
	}
	// Find the name immediately after `fn` (or after `fn pub` —
	// `pub` actually precedes `fn`, but a defensive skip is cheap).
	j := i + 1
	if j < len(p.toks) && p.toks[j].Value == "pub" {
		j++
	}
	if j >= len(p.toks) || p.toks[j].Kind != TokenName {
		return i + 1, true // advance past `fn` and give up
	}
	name := p.toks[j].Value
	j++
	// Skip optional generics `<...>`. Track angle-bracket depth so
	// `fn f<T: Trait<U>>` (with the trailing `>>` lexed as two `>`
	// tokens per AST.md §4) doesn't confuse the scan.
	if j < len(p.toks) && p.toks[j].Value == "<" {
		j = p.skipAngleBlock(j)
	}
	// Now expect `(`. Find the matching `)` to skip the parameter
	// list — its contents are not catalog-relevant.
	if j >= len(p.toks) || p.toks[j].Value != "(" {
		return i + 1, true
	}
	j = matchParen(p.toks, j)
	// Optional return type `-> ...`. Skip to `{` or `;`.
	for j < len(p.toks) && p.toks[j].Value != "{" && p.toks[j].Value != ";" {
		// `where` clauses, return-type generics, lifetime bounds all
		// fall through here.
		if p.toks[j].Value == "<" {
			j = p.skipAngleBlock(j)
			continue
		}
		j++
	}
	if j >= len(p.toks) {
		return i + 1, true
	}
	if p.toks[j].Value == ";" {
		// Forward declaration in a trait — no body, no frame.
		return j + 1, true
	}
	// Push the fn frame for the `{` we're about to consume.
	p.pushBrace(name)
	return j + 1, true
}

// skipAngleBlock advances past a `<...>` block (generics, type
// params, lifetime bounds). Returns the index just past the closing
// `>`. Bounded by the brace-stack discipline (it counts angle depth
// independently and gives up at maxBraceDepth).
//
// Because the lexer emits nested `>>` as two `>` tokens (AST.md §4
// generics decision), this counter naturally pops two levels per
// `>>` source byte sequence. Other ops involving `<` (`<<`, `<=`,
// `<<=`) are checked explicitly so they don't accidentally open a
// new angle frame.
func (p *parser) skipAngleBlock(i int) int {
	if i >= len(p.toks) || p.toks[i].Value != "<" {
		return i
	}
	depth := 0
	for j := i; j < len(p.toks); j++ {
		switch p.toks[j].Value {
		case "<":
			depth++
		case "<<", "<=", "<<=":
			// Not an angle opener.
		case ">":
			depth--
			if depth == 0 {
				return j + 1
			}
		case ">=":
			// `>=` is never an angle closer (`Vec<u32>=` is invalid).
		}
		if depth > maxBraceDepth {
			return j
		}
	}
	return len(p.toks)
}

// tryParseCall attempts to recognize a call site starting at i.
// Patterns handled:
//
//   - `name(args)`         — bare-identifier call
//   - `name::name::...::name(args)` — path call
//   - `name::name::...::name!(args)` / `![args]` / `!{args}` — macro
//   - `name.name.method(args)` — method chain on a name receiver
//   - Path-then-method: `Foo::new().bar()` — the analyzer can match
//     the path-call `Foo::new`; the chained `.bar()` is a separate
//     Call with Callee like `Foo::new().bar` (method-on-expression),
//     which the catalog matcher treats as a documented gap.
//
// Returns the token index just past the closing `)`/`]`/`}` and
// ok=true on success. On failure, ok=false and the caller advances
// by one.
func (p *parser) tryParseCall(i int) (int, bool) {
	if i >= len(p.toks) || p.toks[i].Kind != TokenName {
		return i, false
	}

	// Walk the leading path: `name::name::...::name`.
	segs, after, ok := p.scanPath(i)
	if !ok {
		return i, false
	}

	// After the path, allow `.name.name...` for short method chains
	// on a name receiver. We extend the callee with `.method` for
	// each step. The cost is one allocation per `.name` step which
	// is acceptable.
	for after+1 < len(p.toks) &&
		p.toks[after].Value == "." &&
		p.toks[after+1].Kind == TokenName {
		segs = append(segs, "."+p.toks[after+1].Value)
		after += 2
	}

	// Macro call: `name!(...)` / `name![...]` / `name!{...}`.
	macro := false
	if after < len(p.toks) && p.toks[after].Value == "!" {
		macro = true
		after++
	}

	// Skip optional turbofish: `foo::<T>(...)`.
	if after < len(p.toks) && p.toks[after].Value == "::" &&
		after+1 < len(p.toks) && p.toks[after+1].Value == "<" {
		after = p.skipAngleBlock(after + 1)
	}

	// Opener must be `(`, or for a macro any of `(`, `[`, `{`.
	if after >= len(p.toks) {
		return i, false
	}
	open := p.toks[after].Value
	if !((open == "(") || (macro && (open == "[" || open == "{"))) {
		return i, false
	}

	// Build the callee string from segments. The first segment may
	// be alias-resolved against Uses; trailing segments are not.
	callee := p.assembleCallee(segs)
	line := p.toks[i].Line

	// Resolve first / second positional args. Macro args are not
	// resolved when the opener is `[` or `{`.
	firstArg, secondArg := "", ""
	if open == "(" {
		firstArg = resolveArgN(p.toks, after, 1)
		secondArg = resolveArgN(p.toks, after, 2)
	}

	// Find the matching closer to advance past the call.
	closeIdx := matchBracket(p.toks, after)

	p.mod.Calls = append(p.mod.Calls, Call{
		Callee:    callee,
		InFn:      p.currentFn(),
		Macro:     macro,
		FirstArg:  firstArg,
		SecondArg: secondArg,
		Line:      line,
	})
	return closeIdx, true
}

// scanPath walks a `name::name::...::name` path starting at i, with
// the same shape as scanUsePath but returning (segments, end, ok)
// only when the leading token is a name. Turbofish `::<T>` and
// trailing `::` (no name after) terminate the scan.
func (p *parser) scanPath(i int) (segments []string, end int, ok bool) {
	if i >= len(p.toks) || p.toks[i].Kind != TokenName {
		return nil, i, false
	}
	segments = append(segments, p.toks[i].Value)
	j := i + 1
	for j+1 < len(p.toks) && p.toks[j].Value == "::" {
		// Stop at turbofish: `foo::<T>(...)` — the `::<` indicates
		// a type-arg list, not another path segment.
		if p.toks[j+1].Value == "<" {
			break
		}
		if p.toks[j+1].Kind != TokenName {
			break
		}
		if len(segments) >= maxPathSegments {
			break
		}
		segments = append(segments, p.toks[j+1].Value)
		j += 2
	}
	return segments, j, true
}

// assembleCallee builds the user-facing callee string from a
// segments slice. The first segment is alias-resolved against
// p.useIndex; the rest are joined with `::` (or with `.` if the
// segment already starts with `.`, indicating a method-chain step).
//
// Alias resolution: an exact match on a recorded Use.Alias replaces
// the first segment with the full Use.Path joined by `::`. Example:
// with `use std::env::var as v;` recorded, a Call to `v("X")`
// resolves the leading "v" segment into "std::env::var". The lookup
// is O(1) — recordUse maintains useIndex incrementally.
func (p *parser) assembleCallee(segments []string) string {
	if len(segments) == 0 {
		return ""
	}
	head := segments[0]
	resolved, ok := p.useIndex[head]
	if !ok {
		resolved = []string{head}
	}
	// Append the rest, preserving `.method` vs `::name` form.
	var sb strings.Builder
	for i, s := range resolved {
		if i > 0 {
			sb.WriteString("::")
		}
		sb.WriteString(s)
	}
	for _, s := range segments[1:] {
		if strings.HasPrefix(s, ".") {
			sb.WriteString(s) // method-chain step, e.g. ".arg"
		} else {
			sb.WriteString("::")
			sb.WriteString(s)
		}
	}
	return sb.String()
}

// matchBracket returns the index just past the bracket pair opened
// at i. Handles (), [], {} including nesting and string/comment
// skips (strings/comments are already opaque from the lexer). Lenient
// on unmatched openers: returns the end of the token stream.
func matchBracket(toks []Token, i int) int {
	if i >= len(toks) {
		return i
	}
	open := toks[i].Value
	var close string
	switch open {
	case "(":
		close = ")"
	case "[":
		close = "]"
	case "{":
		close = "}"
	default:
		return i + 1
	}
	depth := 1
	for j := i + 1; j < len(toks); j++ {
		switch toks[j].Value {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(toks)
}

// matchParen is matchBracket specialized to `(`/`)` — used by the
// fn-header scanner.
func matchParen(toks []Token, i int) int {
	return matchBracket(toks, i)
}

// resolveArgN returns the literal string of the n-th positional
// argument (1-based) starting from the `(` at openIdx, or "" when
// it can't be resolved without execution.
//
// Resolves: a TokenString literal (including byte strings, raw
// strings; the literal value is the unprocessed source form), or a
// `+`-concatenation of string literals (rare in Rust). Anything
// else — name references, call results, format strings — is a
// documented conservative gap.
//
// The args are split on top-level commas; nested calls' commas are
// respected via parenthesis depth.
func resolveArgN(toks []Token, openIdx, n int) string {
	if openIdx >= len(toks) || toks[openIdx].Value != "(" {
		return ""
	}
	args := splitArgs(toks, openIdx)
	if n-1 >= len(args) {
		return ""
	}
	return resolveArg(args[n-1])
}

// splitArgs returns the list of arg-token slices between the `(` at
// openIdx and the matching `)`. Each arg is the token range between
// commas at the top-level paren depth. Bounded by maxArgScanTokens.
func splitArgs(toks []Token, openIdx int) [][]Token {
	const maxArgScanTokens = 256
	var args [][]Token
	var current []Token
	depth := 1
	scanned := 0
	for j := openIdx + 1; j < len(toks); j++ {
		scanned++
		if scanned > maxArgScanTokens {
			break
		}
		t := toks[j]
		switch t.Value {
		case "(", "[", "{":
			depth++
			current = append(current, t)
		case ")", "]", "}":
			depth--
			if depth == 0 {
				if len(current) > 0 {
					args = append(args, current)
				}
				return args
			}
			current = append(current, t)
		case ",":
			if depth == 1 {
				args = append(args, current)
				current = nil
			} else {
				current = append(current, t)
			}
		default:
			current = append(current, t)
		}
	}
	if len(current) > 0 {
		args = append(args, current)
	}
	return args
}

// resolveArg returns the literal string value of a single arg's
// tokens, or "" if not resolvable. Handles:
//   - one TokenString — return its unquoted value
//   - "x" + "y" + ... — return concatenated unquoted values
func resolveArg(arg []Token) string {
	// Trim leading/trailing whitespace-like ops (none expected in
	// our lexer output, but defensive).
	if len(arg) == 0 {
		return ""
	}
	if len(arg) == 1 && arg[0].Kind == TokenString {
		return unquote(arg[0].Value)
	}
	// Check for `"a" + "b" + ...` form.
	var parts []string
	for _, t := range arg {
		if t.Kind == TokenString {
			u := unquote(t.Value)
			if u == "" && t.Value != `""` {
				return "" // unquote failure on a non-empty source
			}
			parts = append(parts, u)
			continue
		}
		if t.Kind == TokenOp && t.Value == "+" {
			continue
		}
		return "" // anything else — not resolvable
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "")
}

// unquote returns the value-content of a Rust string-literal source
// form. Handles the common shapes:
//
//   - "x"         → x
//   - r"x"        → x
//   - r#"x"#      → x
//   - r##"x"##    → x (any matched # count)
//   - b"x"        → x
//   - br"x"       → x
//   - br#"x"#     → x
//   - 'X'         → X (single char; only relevant where catalogs
//     might match a byte-or-char literal)
//
// Escape processing inside `"..."` strings is limited to the common
// `\\`, `\"`, `\n`, `\t`, `\r`, `\0` cases. Unicode escapes
// `\u{...}` are left as-is — catalog matching is purely byte
// comparison, and any catalog entry that needed Unicode-decoded
// matching would be redesigned, not propped up by escape-aware
// unquoting here.
func unquote(s string) string {
	// Strip leading b / r / br prefix.
	for {
		switch {
		case strings.HasPrefix(s, "br") && len(s) > 2 && (s[2] == '"' || s[2] == '#'):
			s = s[2:]
			continue
		case strings.HasPrefix(s, "b") && len(s) > 1 && (s[1] == '"' || s[1] == '\''):
			s = s[1:]
			continue
		case strings.HasPrefix(s, "r") && len(s) > 1 && (s[1] == '"' || s[1] == '#'):
			s = s[1:]
			continue
		}
		break
	}

	// Raw string: count hashes, strip matching closure.
	if strings.HasPrefix(s, "#") {
		hashes := 0
		for hashes < len(s) && s[hashes] == '#' {
			hashes++
		}
		if hashes >= len(s) || s[hashes] != '"' {
			return s // malformed
		}
		body := s[hashes+1:]
		// Strip trailing matched `"<#>{hashes}`.
		want := `"` + strings.Repeat("#", hashes)
		if strings.HasSuffix(body, want) {
			return body[:len(body)-len(want)]
		}
		return body
	}
	if strings.HasPrefix(s, `"`) {
		body := strings.TrimSuffix(s[1:], `"`)
		return processStringEscapes(body)
	}
	if strings.HasPrefix(s, "'") {
		body := strings.TrimSuffix(s[1:], "'")
		return processStringEscapes(body)
	}
	return s
}

// processStringEscapes resolves the common Rust escape sequences in
// the body of a regular (non-raw) string literal. Conservative on
// uncommon escapes — passes them through as the source form.
func processStringEscapes(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				sb.WriteByte('\n')
			case 't':
				sb.WriteByte('\t')
			case 'r':
				sb.WriteByte('\r')
			case '0':
				sb.WriteByte(0)
			case '\\':
				sb.WriteByte('\\')
			case '"':
				sb.WriteByte('"')
			case '\'':
				sb.WriteByte('\'')
			default:
				// Pass through unknown escapes as-is.
				sb.WriteByte('\\')
				sb.WriteByte(s[i+1])
			}
			i += 2
			continue
		}
		sb.WriteByte(c)
		i++
	}
	return sb.String()
}
