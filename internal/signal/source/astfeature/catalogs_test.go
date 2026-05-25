package astfeature

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The TestCatalog_* tests pin catalog membership independent of any
// analyzer's wiring. A regression that drops an entry from a
// language-neutral catalog should fail here, not buried inside a
// per-language analyzer test where the absence might be confused
// with a per-language gap.

// TestCatalog_SensitivePath_Membership locks in the SensitivePath
// catalog as the union of authentication keystores, system
// credential files, shell history, and browser / OS credential
// stores. Each category is exercised separately so a regression
// names which class lost an entry.
func TestCatalog_SensitivePath_Membership(t *testing.T) {
	t.Parallel()

	authKeystores := []string{
		"/.ssh/", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519",
		".aws/credentials", ".aws/config", "/.netrc", ".pypirc", ".npmrc",
		".git-credentials", "/.gnupg/", ".docker/config.json",
		"/.kube/config", "/.config/gcloud", "/.azure/",
	}
	for _, want := range authKeystores {
		assert.True(t, slices.Contains(sensitivePathPatterns, want),
			"auth keystore entry %q missing from sensitivePathPatterns", want)
	}

	systemCreds := []string{"/etc/shadow", "/etc/passwd"}
	for _, want := range systemCreds {
		assert.True(t, slices.Contains(sensitivePathPatterns, want),
			"system credential entry %q missing from sensitivePathPatterns", want)
	}

	shellHistory := []string{".bash_history", ".zsh_history"}
	for _, want := range shellHistory {
		assert.True(t, slices.Contains(sensitivePathPatterns, want),
			"shell history entry %q missing from sensitivePathPatterns", want)
	}

	browserStores := []string{
		"Login Data", "Cookies", "key4.db", "logins.json",
		"cookies.sqlite", "Local State", "Library/Keychains",
	}
	for _, want := range browserStores {
		assert.True(t, slices.Contains(sensitivePathPatterns, want),
			"browser/OS credential store entry %q missing", want)
	}

	// Crypto-wallet keystores added per the Trapdoor 2026-05 campaign.
	walletKeystores := []string{
		"/.sui/", "/.config/solana/", "/.aptos/",
		"/.ethereum/keystore/", "wallet.dat",
	}
	for _, want := range walletKeystores {
		assert.True(t, slices.Contains(sensitivePathPatterns, want),
			"crypto-wallet keystore entry %q missing — Trapdoor's "+
				"cargo build.rs payloads read these locations", want)
	}
}

// TestCatalog_PersistencePath_Membership locks in the
// PersistencePath catalog by category: shell rc files, scheduled
// task surfaces, agent/IDE config dirs, git hook dirs, credential-
// store tamper targets.
func TestCatalog_PersistencePath_Membership(t *testing.T) {
	t.Parallel()

	shellRc := []string{
		"/.bashrc", "/.bash_profile", "/.zshrc", "/.profile", "/.bash_aliases",
	}
	for _, want := range shellRc {
		assert.True(t, slices.Contains(persistencePathPatterns, want),
			"shell rc entry %q missing from persistencePathPatterns", want)
	}

	scheduled := []string{
		"/etc/cron", "/var/spool/cron", "/.config/systemd/", "/etc/systemd/",
		"/Library/LaunchAgents/", "/Library/LaunchDaemons/", "/.config/autostart/",
	}
	for _, want := range scheduled {
		assert.True(t, slices.Contains(persistencePathPatterns, want),
			"scheduled-task entry %q missing", want)
	}

	agentIDE := []string{"/.claude/", "/.vscode/", "/.cursor/", "/.idea/"}
	for _, want := range agentIDE {
		assert.True(t, slices.Contains(persistencePathPatterns, want),
			"agent/IDE config dir entry %q missing", want)
	}

	// AI-agent instruction loci added per the Trapdoor 2026-05
	// campaign — both file-level entries (.cursorrules, CLAUDE.md,
	// AGENTS.md, .windsurfrules) and sibling agent/IDE config dirs.
	aiAgentLoci := []string{
		"/.cursorrules", "/CLAUDE.md", "/AGENTS.md", "/.windsurfrules",
		"/.aider/", "/.zed/", "/.codex/", "/.continue/", "/.windsurf/",
	}
	for _, want := range aiAgentLoci {
		assert.True(t, slices.Contains(persistencePathPatterns, want),
			"AI-agent instruction locus %q missing — Trapdoor weaponized this carrier shape", want)
	}

	gitHooks := []string{"/.git/hooks/", "/hooks/post-commit", "/hooks/pre-push"}
	for _, want := range gitHooks {
		assert.True(t, slices.Contains(persistencePathPatterns, want),
			"git hook entry %q missing", want)
	}

	credTamper := []string{"/.npmrc", "/.netrc", "/.git-credentials"}
	for _, want := range credTamper {
		assert.True(t, slices.Contains(persistencePathPatterns, want),
			"credential-store tamper entry %q missing", want)
	}
}

// TestCatalog_CredentialEnv_Membership confirms the env-name catalog
// covers the canonical credential / token surfaces and the generic
// suffixes that catch vendor-prefixed variants.
func TestCatalog_CredentialEnv_Membership(t *testing.T) {
	t.Parallel()

	canonical := []string{
		"NPM_TOKEN", "GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY",
		"AWS_ACCESS_KEY_ID", "VAULT_TOKEN",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN", "ACTIONS_ID_TOKEN_REQUEST_URL",
		"GOOGLE_APPLICATION_CREDENTIALS", "OPENAI_API_KEY",
	}
	for _, want := range canonical {
		assert.True(t, slices.Contains(credentialEnvNames, want),
			"canonical credential env name %q missing", want)
	}

	genericSuffixes := []string{
		"_TOKEN", "_SECRET", "_API_KEY", "_APIKEY", "_PASSWORD",
		"_ACCESS_KEY", "_PRIVATE_KEY", "_CREDENTIALS",
	}
	for _, want := range genericSuffixes {
		assert.True(t, slices.Contains(credentialEnvNames, want),
			"generic suffix %q missing — vendor-prefixed credentials would not match", want)
	}
}

// TestCatalog_CloudMetadata_Membership confirms every documented
// instance-metadata / SSRF-pivot host is present.
func TestCatalog_CloudMetadata_Membership(t *testing.T) {
	t.Parallel()

	want := []string{
		"169.254.169.254",
		"169.254.170.2",
		"metadata.google.internal",
		"metadata.goog",
		"100.100.200.200",
		".internal.cloudapp.net",
	}
	for _, w := range want {
		assert.True(t, slices.Contains(cloudMetadataHosts, w),
			"cloud metadata host %q missing", w)
	}
}

// TestIsSensitivePath_Behavior covers the helper's surface: the
// substring matching, the backslash normalization, the .env
// basename special case, and the empty-string conservative miss.
func TestIsSensitivePath_Behavior(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"ssh_key", "/home/user/.ssh/id_rsa", true},
		{"aws_creds", "/Users/x/.aws/credentials", true},
		{"backslash_path", `C:\Users\x\.ssh\id_rsa`, true},
		{"bare_env", ".env", true},
		{"env_in_subdir", "config/.env", true},
		{"envrc_must_not_fire", ".envrc", false},
		{"environment_cfg_must_not_fire", "environment.cfg", false},
		{"empty_string_never_match", "", false},
		{"ordinary_path", "src/index.js", false},

		// Crypto-wallet keystores (Trapdoor 2026-05).
		{"sui_wallet", "/home/user/.sui/sui_config/keystore.aes", true},
		{"solana_id_json", "/home/user/.config/solana/id.json", true},
		{"aptos_config", "/home/user/.aptos/config.yaml", true},
		{"ethereum_keystore", "/home/user/.ethereum/keystore/UTC--2024-01-15", true},
		{"bitcoin_walletdat", "/home/user/.bitcoin/wallet.dat", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsSensitivePath(tc.path),
				"IsSensitivePath(%q)", tc.path)
		})
	}
}

// TestIsPersistencePath_Behavior covers the write-side helper.
func TestIsPersistencePath_Behavior(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"authorized_keys", "/home/ci/.ssh/authorized_keys", true},
		{"bashrc", "/root/.bashrc", true},
		{"cron_d", "/etc/cron.d/sysmon", true},
		{"claude_settings", "/home/x/.claude/settings.json", true},
		{"vscode_tasks", "/repo/.vscode/tasks.json", true},
		{"git_hooks", "/repo/.git/hooks/post-commit", true},
		{"backslash_normalize", `C:\Users\x\.cursor\setup.mjs`, true},
		{"empty_string_never_match", "", false},
		{"build_output", "./dist/bundle.js", false},

		// AI-agent instruction loci (Trapdoor 2026-05).
		{"cursorrules_at_root", "/repo/.cursorrules", true},
		{"claude_md_at_root", "/repo/CLAUDE.md", true},
		{"agents_md_at_root", "/repo/AGENTS.md", true},
		{"windsurfrules_at_root", "/repo/.windsurfrules", true},
		{"aider_dir_setup", "/home/x/.aider/setup.sh", true},
		{"zed_settings", "/repo/.zed/settings.json", true},
		{"codex_instructions", "/repo/.codex/instructions.md", true},
		{"continue_config", "/repo/.continue/config.json", true},
		{"windsurf_dir", "/repo/.windsurf/rules.md", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsPersistencePath(tc.path),
				"IsPersistencePath(%q)", tc.path)
		})
	}
}

// TestIsCredentialEnvName_Behavior covers exact, case-insensitive,
// and substring-via-suffix matching.
func TestIsCredentialEnvName_Behavior(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		env  string
		want bool
	}{
		{"exact_match", "NPM_TOKEN", true},
		{"exact_case_lower", "npm_token", true},
		{"vendor_suffix_token", "VENDOR_TOKEN", true},
		{"vendor_suffix_api_key", "MYSERVICE_API_KEY", true},
		{"vendor_lowercase_secret", "vendor_secret", true},
		{"ordinary_node_env", "NODE_ENV", false},
		{"port", "PORT", false},
		{"debug", "DEBUG", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsCredentialEnvName(tc.env),
				"IsCredentialEnvName(%q)", tc.env)
		})
	}
}

// TestIsCloudMetadataURL_Behavior covers the metadata-host
// substring match with the empty-string conservative miss.
func TestIsCloudMetadataURL_Behavior(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
		want bool
	}{
		{"aws_imds", "http://169.254.169.254/latest/meta-data/", true},
		{"ecs_task_role", "http://169.254.170.2/v2/credentials", true},
		{"gcp_metadata", "http://metadata.google.internal/computeMetadata/v1/", true},
		{"azure_internal", "https://test.internal.cloudapp.net/", true},
		{"empty_string_never_match", "", false},
		{"ordinary_https", "https://api.example.com/v1/users", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsCloudMetadataURL(tc.url),
				"IsCloudMetadataURL(%q)", tc.url)
		})
	}
}
