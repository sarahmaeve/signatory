// Package astfeature defines the language-neutral types that
// source-evolution AST analyzers consume and produce: SourceFile (the
// per-file input) and Counts (the per-version output).
//
// Counts was originally the golang analyzer's Features type. It was
// moved here so the type is shared: MatrixRow.AST is a single Counts
// value regardless of source language, and every consumer (anomaly
// detection, the matrix JSON schema, the store, deltas) stays
// language-blind. Per-language analyzers (today golang; pypi is
// future work) populate the same fields; the relocation is a
// no-behavior-change refactor — the json tags travel with the struct,
// so the marshaled matrix is byte-for-byte identical.
//
// Whether language-specific signals that have no cross-language analog
// (e.g. Python eval/exec/__import__) get new shared fields, a typed
// extension, or a per-language split is deliberately deferred until a
// second analyzer exists and the comparability question can be
// answered from real data rather than guessed.
package astfeature

// SourceFile is one source file's path-and-bytes presented to an
// analyzer.
//
// Path is posix-style relative to the module root and is used only
// for parser position reporting; the analyzer never opens the path
// from the filesystem.
type SourceFile struct {
	Path    string
	Content []byte
}

// Counts is the per-version tally of AST constructs from one Analyze
// pass. Each field counts occurrences of a specific construct across
// the entire source tree presented to that call.
type Counts struct {
	// InitCount is the number of package-level init() functions
	// across all files. Methods and functions whose names happen to
	// be "init" but have a receiver are NOT counted — only top-level
	// init declarations run on import.
	InitCount int `json:"init_count"`

	// NetworkCallSites is the number of package-level call sites
	// that initiate network egress (matched against
	// NetworkEgressCallSites in patterns.go). Counts call sites, not
	// distinct call targets — `http.Get(a); http.Get(b)` is two.
	NetworkCallSites int `json:"network_call_sites"`

	// SensitivePathReads is the number of call sites in the
	// SensitivePathReadCallSites catalog whose first argument
	// statically resolves to a path matching any
	// SensitivePathPatterns entry. Statically-resolvable paths are
	// string literals and filepath.Join calls whose arguments are
	// themselves resolvable; fully dynamic paths are not counted
	// (documented gap; analyst sees the call-site count via
	// neighboring features).
	SensitivePathReads int `json:"sensitive_path_reads"`

	// ExecCalls is the number of call sites in the ExecCallSites
	// catalog (os/exec.{Command,CommandContext}). Argument content
	// is NOT inspected — a spike in this field, especially within
	// init() functions, is the signal; the analyst reads the spike
	// version's source to interpret intent.
	ExecCalls int `json:"exec_calls"`

	// XORAssignments is the number of `^=` (token.XOR_ASSIGN)
	// statements across all files. The BufferZoneCorp F004 finding
	// cited "systematic XOR + string-split obfuscation"; XOR
	// assignment in a loop body is the canonical decode pattern.
	// Conservative heuristic: legitimate Go rarely uses `^=` outside
	// crypto-adjacent code, so a non-zero count in a non-crypto
	// package is itself meaningful.
	//
	// Gap: `data[i] = data[i] ^ key[i]` (binary XOR inside a regular
	// `=` assignment) is NOT counted. Closing the gap requires
	// loop-context analysis to avoid false positives on legitimate
	// bit-twiddling.
	XORAssignments int `json:"xor_assignments"`

	// Base64DecodeCalls is the number of call sites in the
	// Base64DecodeCallSites catalog. Decoding base64 at runtime
	// within or near init() is a strong obfuscated-payload signal;
	// analytic / logging code that decodes external base64 is rare
	// in package-init.
	Base64DecodeCalls int `json:"base64_decode_calls"`
}
