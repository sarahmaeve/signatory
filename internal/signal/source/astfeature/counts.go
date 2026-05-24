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

	// DynamicEvalCalls is the number of call sites that execute code
	// built at runtime from data: Python eval/exec/__import__/compile.
	// Cross-language by intent (a future JS/Ruby analyzer would count
	// eval the same way), not Python-only — Go has no such primitive
	// and leaves this zero. exec(base64.b64decode(...)) at import is
	// the dominant real PyPI supply-chain payload shape; this field
	// plus Base64DecodeCalls spiking together across versions is its
	// fingerprint.
	DynamicEvalCalls int `json:"dynamic_eval_calls"`

	// ImportTimeCallSites is the number of call sites that run at
	// import time — module-scope calls, the language-neutral analog
	// of Go's package init() execution surface. Named generally
	// rather than reusing InitCount because a Python module-scope
	// call is not an init() function; mislabeling it init_count in
	// the emitted JSON would be an analyst-facing lie. Go leaves this
	// zero today (its import-time surface is InitCount); Python
	// populates it instead of InitCount.
	ImportTimeCallSites int `json:"import_time_call_sites"`

	// InstallHookOverrides is the number of classes wired into the
	// package's install/build lifecycle — today: a setup.py class
	// subclassing a setuptools/distutils command (install, develop,
	// build_py, …). The iconic PyPI vector: the payload lives in the
	// command's run() method and executes at `pip install`, which
	// import-time call counting cannot see. Cross-language by intent
	// (any ecosystem with source-level lifecycle hooks reuses it);
	// Go has no source install hook and leaves this zero.
	InstallHookOverrides int `json:"install_hook_overrides"`

	// EnvCredentialReads is the number of call/access sites that read
	// a process-environment entry whose name matches a credential /
	// cloud-token / CI-secret catalog (AWS_SECRET_ACCESS_KEY,
	// NPM_TOKEN, GITHUB_TOKEN, VAULT_TOKEN, ACTIONS_ID_TOKEN_REQUEST_*,
	// …). Cross-language by intent — JS `process.env.X`, Go
	// `os.Getenv`, Python `os.environ` are the same primitive; only
	// the node analyzer populates it today, others leave it zero.
	// Reading a named secret out of the environment at import time is
	// the dominant npm credential-harvest primitive (TanStack,
	// litellm, bufferzonecorp) and is invisible to SensitivePathReads
	// (which only sees on-disk credential paths). Catalog-matched, not
	// "any env read", so ordinary config reads do not spike it.
	EnvCredentialReads int `json:"env_credential_reads"`

	// SensitivePathWrites is the write analog of SensitivePathReads:
	// the number of file-write sinks whose statically-resolved path
	// targets a persistence / credential-tampering location
	// (~/.ssh/authorized_keys, shell rc files, crontab, systemd user
	// units, agent/IDE config dirs like .claude/.vscode, git hook
	// dirs). The recurring post-exploitation step in TanStack,
	// node-ipc, and bufferzonecorp; a read-only model is blind to it.
	// Cross-language by intent; node populates it via the fs
	// write-sink family, others leave it zero. Statically-resolvable
	// paths only — a runtime-built path is a conservative miss, never
	// a false spike.
	SensitivePathWrites int `json:"sensitive_path_writes"`

	// CloudMetadataCalls is the number of network call sites whose
	// statically-resolved destination is a cloud instance-metadata or
	// SSRF-pivot endpoint (AWS/GCP/Azure IMDS 169.254.169.254, ECS
	// 169.254.170.2, GKE metadata.google.internal, in-cluster Vault).
	// A near-zero-false-positive spike: legitimate package code almost
	// never contacts the metadata service at import time, while
	// credential-theft payloads (TanStack, litellm) do it to mint
	// cloud tokens. Distinct from NetworkCallSites (which counts any
	// egress) because the destination class IS the signal. Cross-
	// language; node populates it, others leave it zero.
	CloudMetadataCalls int `json:"cloud_metadata_calls"`
}
