package golang

// CallSite identifies a (package-path, function-name) pair the
// analyzer counts as a feature occurrence.
//
// Package paths are full Go import paths ("net/http", not "http")
// so that aliased imports (`import h "net/http"`) and locally-defined
// packages with the same leaf name don't collide with stdlib matches.
//
// Function names are top-level package functions only. Methods bound
// to types — e.g., (*http.Client).Do, which a malicious payload may
// call as `http.DefaultClient.Do(req)` — are NOT detected by CallSite
// matching alone; v0.1 accepts this gap because the BufferZoneCorp
// payload variants in the public corpus reach for package-level
// functions (http.Get, http.Post, http.NewRequest), and method
// detection requires either type resolution or a separate
// selector-chain walker. See the package caveats.
type CallSite struct {
	Pkg string
	Fn  string
}

// NetworkEgressCallSites is the catalog of package-level Go functions
// that initiate network egress. A call to any of these counts toward
// Features.NetworkCallSites.
//
// Keep this catalog conservative: false negatives are recoverable
// (the matrix still surfaces the call from neighboring features such
// as sensitive-path reads in the same init), but false positives
// erode analyst trust in spike rows. When in doubt, omit; add only
// after a corpus example demonstrates the call is reached for
// network egress.
//
// This is a security catalog, not a user preference. Editing it down
// silently disables detection of the payload class this collector
// exists to catch — change it through PR review with test coverage.
var NetworkEgressCallSites = []CallSite{
	// Package-level functions.
	{Pkg: "net/http", Fn: "Get"},
	{Pkg: "net/http", Fn: "Post"},
	{Pkg: "net/http", Fn: "PostForm"},
	{Pkg: "net/http", Fn: "Head"},
	{Pkg: "net/http", Fn: "NewRequest"},
	{Pkg: "net/http", Fn: "NewRequestWithContext"},
	{Pkg: "net", Fn: "Dial"},
	{Pkg: "net", Fn: "DialTimeout"},
	{Pkg: "net", Fn: "DialIP"},
	{Pkg: "net", Fn: "DialTCP"},
	{Pkg: "net", Fn: "DialUDP"},
	{Pkg: "net", Fn: "DialUnix"},
	// Method calls on package-level *Client / *Transport values.
	// These match the pkg.Var.Method form via callSiteOf's method-
	// chain handling. Local variables bound to *http.Client (e.g.,
	// `client := &http.Client{}; client.Do(req)`) are NOT matched
	// — that's the v0.1 local-var gap; documented in package caveats.
	{Pkg: "net/http", Fn: "DefaultClient.Do"},
	{Pkg: "net/http", Fn: "DefaultClient.Get"},
	{Pkg: "net/http", Fn: "DefaultClient.Post"},
	{Pkg: "net/http", Fn: "DefaultClient.PostForm"},
	{Pkg: "net/http", Fn: "DefaultClient.Head"},
	{Pkg: "net/http", Fn: "DefaultTransport.RoundTrip"},
}

// ExecCallSites is the catalog of package-level functions that spawn
// external processes. A call to any of these counts toward
// Features.ExecCalls. The argument-content (which binary, with what
// args) is NOT inspected at this layer — the count alone is the
// signal; the analyst reads the spike row to interpret intent.
var ExecCallSites = []CallSite{
	{Pkg: "os/exec", Fn: "Command"},
	{Pkg: "os/exec", Fn: "CommandContext"},
}

// Base64DecodeCallSites is the catalog of base64-decode entry points.
// Counts toward Features.Base64DecodeCalls. Decoding is the relevant
// direction for obfuscated-payload detection: encoding (EncodeToString)
// is benign for analytics/logging contexts; decoding at runtime within
// init() or near network egress is a strong obfuscation signal.
//
// Coverage limitations (v0.1):
//   - Local variables bound to a base64.*Encoding value (e.g.,
//     `enc := base64.StdEncoding; enc.DecodeString(s)`) are NOT
//     matched. The pkg.Var.Method chain is required.
//   - Custom *base64.Encoding values via base64.NewEncoding are not
//     tracked.
var Base64DecodeCallSites = []CallSite{
	{Pkg: "encoding/base64", Fn: "StdEncoding.Decode"},
	{Pkg: "encoding/base64", Fn: "StdEncoding.DecodeString"},
	{Pkg: "encoding/base64", Fn: "URLEncoding.Decode"},
	{Pkg: "encoding/base64", Fn: "URLEncoding.DecodeString"},
	{Pkg: "encoding/base64", Fn: "RawStdEncoding.Decode"},
	{Pkg: "encoding/base64", Fn: "RawStdEncoding.DecodeString"},
	{Pkg: "encoding/base64", Fn: "RawURLEncoding.Decode"},
	{Pkg: "encoding/base64", Fn: "RawURLEncoding.DecodeString"},
	{Pkg: "encoding/base64", Fn: "NewDecoder"},
}

// SensitivePathReadCallSites is the catalog of package-level Go
// functions that read filesystem state. A call to any of these whose
// first argument resolves to a path matching SensitivePathPatterns
// counts toward Features.SensitivePathReads.
//
// Both presence-revealing (Stat, Lstat) and content-revealing (Open,
// ReadFile, ReadDir) operations are included: an attacker enumerating
// which credentials exist on the host is worth detecting separately
// from one reading the bytes.
//
// Writes (os.Create, os.WriteFile, os.OpenFile with O_WRONLY) are
// NOT covered here — the go-stdlib-ext authorized_keys-append payload
// is a write, not a read. v0.1 documents this gap; later work adds a
// SensitivePathWrites catalog and Features field rather than mingling
// read-vs-write semantics in one count.
var SensitivePathReadCallSites = []CallSite{
	{Pkg: "os", Fn: "Open"},
	{Pkg: "os", Fn: "OpenFile"},
	{Pkg: "os", Fn: "ReadFile"},
	{Pkg: "os", Fn: "ReadDir"},
	{Pkg: "os", Fn: "Stat"},
	{Pkg: "os", Fn: "Lstat"},
	{Pkg: "io/ioutil", Fn: "ReadFile"},
	{Pkg: "io/ioutil", Fn: "ReadDir"},
}

// SensitivePathPatterns are filesystem-path substrings whose presence
// in a statically-resolvable path argument to a SensitivePathRead
// call site counts toward Features.SensitivePathReads.
//
// Patterns are matched against the resolved path with strings.Contains
// (not regex, not glob). Pattern selection drawn from observed
// supply-chain payloads — the 2026-04-30 BufferZoneCorp campaign
// (go-stdlib-ext appending to ~/.ssh/authorized_keys; go-stdlog
// probing IMDS at 169.254.169.254; go-metrics-sdk tampering with
// go.sum) — and from common credential staging locations called out
// in cloud-shadow-IT advisories.
//
// Conservative on purpose: a pattern that fires too aggressively
// floods the matrix with false positives and erodes analyst trust.
// Add patterns only after a corpus example demonstrates the path is
// reached for credential exfiltration or persistence.
var SensitivePathPatterns = []string{
	"/.ssh/", ".ssh/authorized_keys", ".ssh/id_rsa", ".ssh/id_ed25519",
	"/.aws/", ".aws/credentials", ".aws/config",
	"/.npmrc", ".npmrc",
	"/.netrc",
	"/.kube/", ".kube/config",
	"/.docker/", ".docker/config.json",
	"/.config/gh/", ".config/gh/hosts.yml",
	"/.gnupg/",
	"/etc/passwd", "/etc/shadow",
	"169.254.169.254", // IMDS — informed by go-stdlog payload
	"go.sum",          // go-metrics-sdk variant directly tampers go.sum
}
