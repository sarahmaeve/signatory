package git

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseCommitSigningLog covers the byte-level parser with
// hand-constructed inputs. Byte literals use explicit 0x1F / 0x1E
// escapes so the test spec is readable and matches exactly what
// the git log format emits in practice.
func TestParseCommitSigningLog(t *testing.T) {
	t.Parallel()

	const us = "\x1f"
	const rs = "\x1e"

	cases := []struct {
		name string
		data string
		want []commitSigningRow
	}{
		{
			name: "empty input produces empty slice not nil",
			data: "",
			want: []commitSigningRow{},
		},
		{
			name: "single unsigned commit",
			data: "abc123" + us + "Alice" + us + "alice@example.com" + us + "N" + us + "" + us + "" + rs,
			want: []commitSigningRow{
				{Hash: "abc123", AuthorName: "Alice", AuthorEmail: "alice@example.com", SignatureStatus: "N"},
			},
		},
		{
			name: "single per-developer signed commit",
			data: "def456" + us + "Bob" + us + "bob@example.com" + us + "G" + us + "Bob Signer" + us + "DEADBEEFCAFEBABE" + rs,
			want: []commitSigningRow{
				{
					Hash: "def456", AuthorName: "Bob", AuthorEmail: "bob@example.com",
					SignatureStatus: "G", SignerName: "Bob Signer", KeyID: "DEADBEEFCAFEBABE",
				},
			},
		},
		{
			name: "web-flow signed commit",
			data: "789abc" + us + "GitHub" + us + "noreply@github.com" + us + "G" + us + "GitHub" + us + "B5690EEEBB952194" + rs,
			want: []commitSigningRow{
				{
					Hash: "789abc", AuthorName: "GitHub", AuthorEmail: "noreply@github.com",
					SignatureStatus: "G", SignerName: "GitHub", KeyID: "B5690EEEBB952194",
				},
			},
		},
		{
			name: "three commits separated by record terminators plus newline",
			data: "a" + us + "A" + us + "a@x" + us + "N" + us + "" + us + "" + rs +
				"\nb" + us + "B" + us + "b@x" + us + "G" + us + "B Signer" + us + "0123456789ABCDEF" + rs +
				"\nc" + us + "C" + us + "c@x" + us + "N" + us + "" + us + "" + rs,
			want: []commitSigningRow{
				{Hash: "a", AuthorName: "A", AuthorEmail: "a@x", SignatureStatus: "N"},
				{Hash: "b", AuthorName: "B", AuthorEmail: "b@x", SignatureStatus: "G", SignerName: "B Signer", KeyID: "0123456789ABCDEF"},
				{Hash: "c", AuthorName: "C", AuthorEmail: "c@x", SignatureStatus: "N"},
			},
		},
		{
			name: "pipe characters in author name do not confuse parser",
			data: "h" + us + "A|uthor | name" + us + "a@x" + us + "N" + us + "" + us + "" + rs,
			want: []commitSigningRow{
				{Hash: "h", AuthorName: "A|uthor | name", AuthorEmail: "a@x", SignatureStatus: "N"},
			},
		},
		{
			name: "truncated record (fewer than 6 fields) is skipped silently",
			data: "short" + us + "only" + us + "three" + rs +
				"good" + us + "A" + us + "a@x" + us + "N" + us + "" + us + "" + rs,
			want: []commitSigningRow{
				{Hash: "good", AuthorName: "A", AuthorEmail: "a@x", SignatureStatus: "N"},
			},
		},
		{
			name: "trailing record-terminator only input produces empty slice",
			data: rs,
			want: []commitSigningRow{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseCommitSigningLog([]byte(tc.data))
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestClassifySigning walks every %G? flag value and both web-flow
// key IDs plus a novel per-developer key, locking in the
// classification table specified in the signing.go doc comment.
func TestClassifySigning(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		row  commitSigningRow
		want signingClass
	}{
		// Unsigned equivalents.
		{"N (no signature)", commitSigningRow{SignatureStatus: "N"}, classUnsigned},
		{"empty status", commitSigningRow{SignatureStatus: ""}, classUnsigned},
		{"B (bad signature)", commitSigningRow{SignatureStatus: "B", KeyID: "DEADBEEFCAFEBABE"}, classUnsigned},
		{"E (cannot check)", commitSigningRow{SignatureStatus: "E", KeyID: "DEADBEEFCAFEBABE"}, classUnsigned},
		{"R (revoked key)", commitSigningRow{SignatureStatus: "R", KeyID: "DEADBEEFCAFEBABE"}, classUnsigned},
		{"future unknown flag Z", commitSigningRow{SignatureStatus: "Z", KeyID: "DEADBEEFCAFEBABE"}, classUnsigned},

		// Valid-signature equivalents, per-developer key.
		{"G (good) per-dev", commitSigningRow{SignatureStatus: "G", KeyID: "DEADBEEFCAFEBABE"}, classPerDeveloper},
		{"U (unknown validity) per-dev", commitSigningRow{SignatureStatus: "U", KeyID: "DEADBEEFCAFEBABE"}, classPerDeveloper},
		{"X (sig expired) per-dev", commitSigningRow{SignatureStatus: "X", KeyID: "DEADBEEFCAFEBABE"}, classPerDeveloper},
		{"Y (key expired) per-dev", commitSigningRow{SignatureStatus: "Y", KeyID: "DEADBEEFCAFEBABE"}, classPerDeveloper},

		// Web-flow keys (both listed IDs, both cases).
		{"G with current web-flow key uppercase", commitSigningRow{SignatureStatus: "G", KeyID: "B5690EEEBB952194"}, classWebFlow},
		{"G with current web-flow key lowercase", commitSigningRow{SignatureStatus: "G", KeyID: "b5690eeebb952194"}, classWebFlow},
		{"G with older web-flow key", commitSigningRow{SignatureStatus: "G", KeyID: "4AEE18F83AFDEB23"}, classWebFlow},
		{"G with web-flow key surrounded by whitespace", commitSigningRow{SignatureStatus: "G", KeyID: "  B5690EEEBB952194 "}, classWebFlow},

		// Empty key ID with good status falls through to per-developer.
		// This is an oddity of git output (some signature types report
		// status G without filling GK) and the conservative call is to
		// NOT classify it as web-flow.
		{"G with empty key falls to per-dev", commitSigningRow{SignatureStatus: "G", KeyID: ""}, classPerDeveloper},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifySigning(tc.row)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCollectCommitSigning_AttackerControlledGitConfigCannotExec is the
// CVE-2025-41390 / TALOS-2025-2243 reproduction test, neutralized at the
// gitenv chokepoint by the safeOverrides "-c gpg.program=..." argv prefix.
//
// Threat shape (see design/analysis/cve-2025-41390.md): an attacker
// distributes a tarball whose .git/config contains
//
//	[gpg]
//	    program = /tmp/pwned-gpg
//
// and whose history includes one commit carrying a `gpgsig` header
// (real or forged — the doc notes git invokes gpg.program either way
// to determine the signature status). When the operator runs
// `signatory analyze --path ./malicious-repo legit/repo`, the
// collectCommitSigning code path runs `git log --format=...%G?...HEAD`,
// which causes git to exec the configured gpg.program. The on-disk
// `.git/config` directive is the file-vector sibling of CVE-2025-41390:
// CWE-829, attacker-controlled config driving arbitrary command exec.
//
// This test materializes the exploit shape on disk:
//
//   - A real git repo with one ordinary commit
//   - A second commit object built via `git hash-object` carrying a
//     forged gpgsig header (no real PGP material — the body is
//     bogus PGP-shaped text; status determination via %G? still
//     hands it to gpg.program)
//   - A `.git/config` `[gpg] program = …` directive pointing at a
//     shell shim that touches a marker file
//
// Then it runs collectCommitSigning. With the fix in place
// (gitenv.NewCmd injects `-c gpg.program=/usr/bin/false` and siblings),
// git's effective gpg.program is /usr/bin/false, the on-disk shim
// never executes, and the marker file does not appear.
//
// Revert proof: remove the safeOverrides prepend in gitenv.NewCmd; this
// test fails because git honors the on-disk gpg.program and the shim
// touches the marker.
//
// NOTE: t.Setenv-free — the attack vector is on-disk .git/config, not
// the parent env. Safe under t.Parallel.
func TestCollectCommitSigning_AttackerControlledGitConfigCannotExec(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "first commit")

	// Capture base commit's tree and parent for the forged commit
	// object.
	parentBytes, err := runGit(t.Context(), repo, "rev-parse", "HEAD")
	require.NoError(t, err)
	parentID := strings.TrimSpace(string(parentBytes))

	treeBytes, err := runGit(t.Context(), repo, "rev-parse", "HEAD^{tree}")
	require.NoError(t, err)
	treeID := strings.TrimSpace(string(treeBytes))

	// Forge a commit object carrying a gpgsig header. The signature
	// body is bogus PGP-shaped text — git's commit-object reader
	// stores it verbatim; %G? hands the body to gpg.program for
	// status determination, which is the attack trigger we're
	// defending against.
	//
	// The header continuation discipline (each non-first line of the
	// signature prefixed with " ") follows RFC 5322 / git's commit-
	// object format. Without it, git rejects the object on read.
	//
	// The author/committer timestamp uses time.Now() (not a hardcoded
	// epoch) because collectCommitSigning's `--since=...` window
	// would otherwise filter out a fixed-old forged commit before %G?
	// evaluates — and a filtered commit defeats the test even when
	// gpg.program is honored. A recent timestamp keeps the forged
	// commit inside the 12-month window for any future test run.
	now := time.Now().Unix()
	forged := fmt.Sprintf(
		"tree %s\n"+
			"parent %s\n"+
			"author Test User <test@example.invalid> %d +0000\n"+
			"committer Test User <test@example.invalid> %d +0000\n"+
			"gpgsig -----BEGIN PGP SIGNATURE-----\n"+
			" \n"+
			" iHUEABYIAB0WIQQfakeSignatureFakeSignatureFakeFakeF\n"+
			" AAoJECfakeFFakeF=fake\n"+
			" -----END PGP SIGNATURE-----\n"+
			"\n"+
			"Forged signed commit\n",
		treeID, parentID, now, now)

	hashCmd := gitenv.NewCmd(t.Context(), "-C", repo, "hash-object", "-w", "-t", "commit", "--stdin")
	hashCmd.Stdin = strings.NewReader(forged)
	var stdout, stderr bytes.Buffer
	hashCmd.Stdout = &stdout
	hashCmd.Stderr = &stderr
	require.NoErrorf(t, hashCmd.Run(),
		"hash-object must accept the forged commit; stderr: %s", stderr.String())
	forgedHash := strings.TrimSpace(stdout.String())
	mustRunGit(t, repo, "update-ref", "refs/heads/main", forgedHash)

	// Plant the .git/config exec directive AND the shim that proves
	// exec. The shim touches a marker file we probe after the
	// collector runs. Tempdir-rooted so cleanup is automatic and
	// there's no risk of cross-test interference.
	markerDir := t.TempDir()
	markerPath := filepath.Join(markerDir, "pwned")
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "evil-gpg")
	// #!/bin/sh shim with touch — same shape as the env-dump shim
	// in cmd/signatory/collectors_test.go (installFakeGitEnvDump).
	shim := fmt.Sprintf("#!/bin/sh\ntouch %q\nexit 0\n", markerPath)
	require.NoError(t, os.WriteFile(shimPath, []byte(shim), 0o755)) //nolint:gosec // G306: shim must be executable

	// Append the malicious gpg.program directive to the existing
	// .git/config — same shape as a malicious tarball-shipped config.
	configPath := filepath.Join(repo, ".git", "config")
	cfg, err := os.ReadFile(configPath) //nolint:gosec // G304: configPath is t.TempDir-rooted
	require.NoError(t, err)
	cfg = fmt.Appendf(cfg, "\n[gpg]\n\tprogram = %s\n", shimPath)
	require.NoError(t, os.WriteFile(configPath, cfg, 0o644)) //nolint:gosec // G306: git's own perms on .git/config

	// Sanity check: marker absent before the collector runs. If this
	// fires, the test scaffolding itself is broken (the tempdir was
	// just created).
	_, err = os.Stat(markerPath)
	require.Truef(t, os.IsNotExist(err),
		"marker must not exist before the collector runs; got err=%v", err)

	// Run the collector. The exact emitted-signal shape doesn't
	// matter for this test — what matters is that the .git/config-
	// supplied gpg.program shim was NOT invoked. The collector may
	// emit failures or absences; that's fine, it's the file-vector
	// defense we're proving.
	c := NewCollector(repo)
	_, _ = c.Collect(t.Context(), &profile.Entity{ID: "e1", URL: "https://github.com/legit/repo"})

	_, err = os.Stat(markerPath)
	assert.Truef(t, os.IsNotExist(err),
		"attacker-controlled gpg.program in .git/config must NOT execute; "+
			"marker file %q indicates git honored the on-disk directive — "+
			"see design/analysis/cve-2025-41390.md",
		markerPath)
}
