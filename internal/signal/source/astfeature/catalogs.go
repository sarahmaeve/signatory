package astfeature

import (
	"strings"

	"github.com/sarahmaeve/signatory/internal/agentconfig"
)

// This file holds the language-neutral catalogs the per-language
// source-AST analyzers consult. The catalogs describe **what the
// payload targets** (OS / credential store / network endpoint /
// process-environment names) — properties of the host environment
// the malicious code operates against, not properties of any
// particular language's API.
//
// Per-language API catalogs (writeSinkCallees, processExecCallees,
// networkCallees, …) stay in each analyzer because those genuinely
// vary by ecosystem: fs.writeFile vs open(...).write() vs std::fs::
// write all express the same intent through different surface.
//
// Originally these catalogs were duplicated across
// internal/signal/source/node/analyze.go and
// internal/signal/source/python/analyze.go; the comment on the node
// declaration acknowledged the duplication ("the same language-
// neutral catalog the python analyzer uses"). Extracting them here
// makes the language-neutrality concrete: any analyzer that wires
// up SensitivePathReads / SensitivePathWrites / EnvCredentialReads
// / CloudMetadataCalls consumes the same lists, and any
// threat-landscape addition lands in one place.

// sensitivePathPatterns are credential / secret-material fragments
// matched as substrings against the backslash-normalized resolved
// file path. Tight on purpose: this must not fire on ordinary file
// I/O.
//
// The bare ".env" basename is detected separately by IsSensitivePath
// to avoid matching ".envrc" or "environment.cfg".
var sensitivePathPatterns = []string{
	"/.ssh/", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
	".aws/credentials", ".aws/config", "/.netrc", ".pypirc", ".npmrc",
	".git-credentials", "/.gnupg/", ".docker/config.json",
	"/.kube/config", "/.config/gcloud", "/.azure/", "/etc/shadow",
	"/etc/passwd", ".bash_history", ".zsh_history",
	// Browser / OS credential stores.
	"Login Data", "Cookies", "key4.db", "logins.json",
	"cookies.sqlite", "Local State", "Library/Keychains",
	// Crypto-wallet keystores. Added 2026-05 per the Trapdoor
	// cross-ecosystem campaign whose cargo build.rs payloads read
	// these locations to harvest blockchain credentials. Sui /
	// Solana / Aptos / Ethereum keystore / Bitcoin wallet.dat.
	"/.sui/", "/.config/solana/", "/.aptos/",
	"/.ethereum/keystore/", "wallet.dat",
	// Game-client credential stores. Added 2026-06 per the spadata
	// PyPI stealer, which decrypts the Roblox .ROBLOSECURITY session
	// cookie out of robloxcookies.dat under Roblox/LocalStorage.
	"robloxcookies.dat", "Roblox/LocalStorage",
}

// IsSensitivePath reports whether a statically-resolved path
// targets credential or secret material. The empty string
// (unresolved arg) is never sensitive — a runtime-built path is a
// conservative miss, not a guess.
//
// Backslashes are normalized to forward slashes before matching, so
// Windows-shaped paths still hit the posix catalog entries.
func IsSensitivePath(p string) bool {
	if p == "" {
		return false
	}
	norm := strings.ReplaceAll(p, "\\", "/")
	for _, pat := range sensitivePathPatterns {
		if strings.Contains(norm, pat) {
			return true
		}
	}
	// Bare dotenv file: basename exactly ".env" (avoid matching
	// "environment.cfg" or ".envrc"). Special-cased rather than
	// added to the catalog because substring matching can't express
	// "basename equals X".
	base := norm
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return base == ".env"
}

// persistencePathPatterns are persistence / credential-tamper write
// destinations. Distinct from sensitivePathPatterns (read-side
// credential material): these are the locations a payload WRITES
// to in order to survive uninstall or hijack future sessions — the
// recurring post-exploitation step across TanStack, node-ipc,
// bufferzonecorp, and the AI-agent-config injection class Trapdoor
// pioneered. Substring-matched against the backslash-normalized
// resolved path.
//
// The AI-agent instruction-file loci are derived at init from
// internal/agentconfig.RuntimePersistencePrefixes(), so the source
// of truth for "what is an agent-locus" lives in one place. Adding
// a new Locus there automatically expands this catalog; the
// previously-duplicated maintenance (Trapdoor 2026-05 added
// /.codex/ here but forgot the corresponding repofiles Family) is
// no longer possible.
var persistencePathPatterns = func() []string {
	out := []string{
		"/.ssh/authorized_keys", "/.ssh/config", "/.bashrc", "/.bash_profile",
		"/.zshrc", "/.profile", "/.bash_aliases",
		"/etc/cron", "/var/spool/cron", "/.config/systemd/", "/etc/systemd/",
		"/Library/LaunchAgents/", "/Library/LaunchDaemons/",
		"/.config/autostart/",
		// Non-AI editor config dirs an implant writes to survive uninstall.
		// AI-coding-agent loci (.claude, .cursor, .aider, .zed, .codex,
		// .continue, .windsurf and the agent-instruction files at root)
		// are appended from agentconfig.RuntimePersistencePrefixes() below.
		"/.vscode/", "/.idea/",
		// Git hook dirs (post-commit/pre-push implants).
		"/.git/hooks/", "/hooks/post-commit", "/hooks/pre-push",
		// Credential-store tampering (writing, not reading).
		"/.npmrc", "/.netrc", "/.git-credentials",
	}
	return append(out, agentconfig.RuntimePersistencePrefixes()...)
}()

// IsPersistencePath reports whether a statically-resolved write
// destination targets a persistence / credential-tamper location.
// The empty string (unresolved) is never a match — a runtime-built
// path is a conservative miss, never a false guess.
func IsPersistencePath(p string) bool {
	if p == "" {
		return false
	}
	norm := strings.ReplaceAll(p, "\\", "/")
	for _, pat := range persistencePathPatterns {
		if strings.Contains(norm, pat) {
			return true
		}
	}
	return false
}

// credentialEnvNames is the catalog of process-environment entry
// names whose read is a credential / cloud-token / CI-secret
// harvest. Exact-or-substring matched (case-insensitive on the
// input) so AWS_SECRET_ACCESS_KEY and a vendor-prefixed *_API_KEY
// both hit, while NODE_ENV / PORT / DEBUG do not. Tight on purpose
// — this must not fire on ordinary config reads.
var credentialEnvNames = []string{
	"NPM_TOKEN", "GITHUB_TOKEN", "GH_TOKEN", "VAULT_TOKEN",
	"AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "AWS_SESSION_TOKEN",
	"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN",
	"ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_URL",
	"ACTIONS_RUNTIME_TOKEN", "GOOGLE_APPLICATION_CREDENTIALS",
	"AZURE_CLIENT_SECRET", "AZURE_TENANT_ID", "CLOUDFLARE_API_TOKEN",
	"DIGITALOCEAN_ACCESS_TOKEN", "DOCKER_PASSWORD", "DOCKER_AUTH_CONFIG",
	"SLACK_TOKEN", "STRIPE_SECRET_KEY", "SENTRY_AUTH_TOKEN",
	"DATABASE_URL", "REDIS_URL", "MONGODB_URI", "SSH_PRIVATE_KEY",
	"PRIVATE_KEY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
	// Generic credential-bearing suffixes (substring-matched).
	"_TOKEN", "_SECRET", "_API_KEY", "_APIKEY", "_PASSWORD",
	"_ACCESS_KEY", "_PRIVATE_KEY", "_CREDENTIALS",
}

// IsCredentialEnvName reports whether an env-var name contains a
// credential/secret signal. Case-insensitive; matches by substring
// for every catalog entry — both the named ones (NPM_TOKEN,
// AWS_SECRET_ACCESS_KEY, …) and the generic suffixes (_TOKEN,
// _SECRET, …). False positives are accepted; the design policy
// is that a missed credential read is the more expensive failure
// mode than an over-flagged variant.
//
// Practical implications of the substring rule:
//
//   - VENDOR_TOKEN → fires (matches the _TOKEN suffix).
//   - MY_DATABASE_URL_PROD → fires (substring match on DATABASE_URL).
//     This is the intent: prefixed namespaced credentials still
//     classify as credential reads.
//   - NOT_NPM_TOKEN_REAL → fires (substring match on NPM_TOKEN AND
//     on _TOKEN — multiple catalog entries can match a single name).
//     Accepted false positive; the alternative (exact-only on the
//     named entries) drops legitimate prefixed forms like
//     STRIPE_DATABASE_URL.
//   - NODE_ENV / PORT / DEBUG → don't fire (no catalog entry as
//     substring).
func IsCredentialEnvName(name string) bool {
	u := strings.ToUpper(name)
	for _, c := range credentialEnvNames {
		if strings.Contains(u, c) {
			return true
		}
	}
	return false
}

// cloudMetadataHosts are instance-metadata / SSRF-pivot endpoints.
// A network call whose resolved URL contains one is credential-mint
// behavior — legitimate package code effectively never contacts
// these at import time, so this is a near-zero-false-positive
// signal.
var cloudMetadataHosts = []string{
	"169.254.169.254",          // AWS/Azure/GCP/OpenStack IMDS
	"169.254.170.2",            // ECS task-role credential endpoint
	"metadata.google.internal", // GCP/GKE metadata
	"metadata.goog",            // GCP short alias
	"100.100.200.200",          // Alibaba Cloud metadata
	".internal.cloudapp.net",   // Azure internal
}

// IsCloudMetadataURL reports whether a statically-resolved URL
// string targets a cloud metadata / SSRF-pivot host. Empty
// (unresolved) is never a match — a runtime-built URL is a
// conservative miss.
func IsCloudMetadataURL(u string) bool {
	if u == "" {
		return false
	}
	for _, h := range cloudMetadataHosts {
		if strings.Contains(u, h) {
			return true
		}
	}
	return false
}
