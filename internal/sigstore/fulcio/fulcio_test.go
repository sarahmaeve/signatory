package fulcio

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sourceRepoDigestOID mirrors the production constant so a typo in
// SourceRepoDigestOID breaks the test loudly rather than the test
// silently agreeing with a wrong value.
//
// 1.3.6.1.4.1.57264.1.13 = "Source Repository Digest" per Sigstore's
// Fulcio claim OID registry.
var sourceRepoDigestOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 13}

// TestSourceRepoDigestOID_MatchesRegistry pins the exported OID
// constant. Both pypi and npm provenance flow through this single
// value; a drift here mis-targets the extension lookup for every
// ecosystem at once.
func TestSourceRepoDigestOID_MatchesRegistry(t *testing.T) {
	t.Parallel()
	assert.True(t, SourceRepoDigestOID.Equal(sourceRepoDigestOID),
		"exported OID must equal the Sigstore Fulcio source-repo-digest OID")
}

// TestExtractSourceRepoDigest_PinsHappyPath: a parsed x509 cert
// whose extension at the source-repo-digest OID carries a DER-encoded
// UTF8String SHA yields the SHA verbatim (inner string, not the DER
// outer bytes).
func TestExtractSourceRepoDigest_PinsHappyPath(t *testing.T) {
	t.Parallel()

	const wantSHA = "ec11c4a93de22cde2abe2bf74d70791033c2464c"

	derValue, err := asn1.MarshalWithParams(wantSHA, "utf8")
	require.NoError(t, err)

	cert := &x509.Certificate{
		Extensions: []pkix.Extension{
			{Id: sourceRepoDigestOID, Value: derValue},
		},
	}

	got, ok := ExtractSourceRepoDigest(cert)
	require.True(t, ok,
		"extension at the source-repo-digest OID is present; helper "+
			"must locate it and return ok=true")
	assert.Equal(t, wantSHA, got,
		"extracted value must be the inner UTF8String contents, not "+
			"the DER-encoded outer bytes")
}

// TestExtractSourceRepoDigest_ReturnsFalseWhenAbsent: a cert without
// the source-repo-digest extension yields ("", false).
func TestExtractSourceRepoDigest_ReturnsFalseWhenAbsent(t *testing.T) {
	t.Parallel()

	otherOID := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 9}
	otherValue, err := asn1.MarshalWithParams("https://example/run", "utf8")
	require.NoError(t, err)

	cert := &x509.Certificate{
		Extensions: []pkix.Extension{
			{Id: otherOID, Value: otherValue},
		},
	}

	got, ok := ExtractSourceRepoDigest(cert)
	assert.False(t, ok,
		"missing source-repo-digest OID must return ok=false")
	assert.Equal(t, "", got)
}

// TestExtractSourceRepoDigest_NilCert: nil-safety contract.
func TestExtractSourceRepoDigest_NilCert(t *testing.T) {
	t.Parallel()

	got, ok := ExtractSourceRepoDigest(nil)
	assert.False(t, ok)
	assert.Equal(t, "", got)
}

// TestExtractSourceRepoDigest_RejectsNonSHAValue is the trust-boundary
// test. The Fulcio cert chain is NOT cryptographically verified by
// callers, so a malicious publisher fully controls these bytes. The
// recovered value flows verbatim into git ls-tree / cat-file / diff
// argv downstream via version_pin_table. Anything that is not a real
// git object id must be rejected here exactly like an absent
// extension — ("", false) — never passed through to a git argv.
func TestExtractSourceRepoDigest_RejectsNonSHAValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  string
		ok    bool
	}{
		{"argv flag injection", "--upload-pack=/tmp/evil", "", false},
		{"leading dash", "-rf", "", false},
		{"newline desync", "abc\n0000000000000000000000000000000000000000", "", false},
		{"space in value", "deadbeef deadbeef", "", false},
		{"non-hex garbage", "not-a-real-sha", "", false},
		{"too short to be an oid", "dead", "", false},
		{"empty", "", "", false},
		{"valid sha1", "ec11c4a93de22cde2abe2bf74d70791033c2464c",
			"ec11c4a93de22cde2abe2bf74d70791033c2464c", true},
		{"valid sha256",
			"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			derValue, err := asn1.MarshalWithParams(tc.value, "utf8")
			require.NoError(t, err)
			cert := &x509.Certificate{
				Extensions: []pkix.Extension{
					{Id: sourceRepoDigestOID, Value: derValue},
				},
			}

			got, ok := ExtractSourceRepoDigest(cert)
			assert.Equal(t, tc.ok, ok,
				"a non-SHA cert value must be treated like an absent "+
					"extension so it never reaches a git argv")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestIsGitObjectID covers the shape gate directly: only 40-hex
// (SHA-1) or 64-hex (SHA-256), any case, nothing else.
func TestIsGitObjectID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"sha1 lower", "ec11c4a93de22cde2abe2bf74d70791033c2464c", true},
		{"sha1 upper", "EC11C4A93DE22CDE2ABE2BF74D70791033C2464C", true},
		{"sha256", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"empty", "", false},
		{"abbreviated", "ec11c4a", false},
		{"flag-shaped", "--upload-pack=x", false},
		{"newline", "ec11c4a93de22cde2abe2bf74d70791033c2464\n", false},
		{"non-hex", "g3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", false},
		{"41 hex", "ec11c4a93de22cde2abe2bf74d70791033c2464cc", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsGitObjectID(tc.in))
		})
	}
}

// oid builds a Fulcio claim OID 1.3.6.1.4.1.57264.1.<n>.
func oid(n int) asn1.ObjectIdentifier {
	return asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, n}
}

func certWith(t *testing.T, exts map[int]string) *x509.Certificate {
	t.Helper()
	c := &x509.Certificate{}
	for n, v := range exts {
		der, err := asn1.MarshalWithParams(v, "utf8")
		require.NoError(t, err)
		c.Extensions = append(c.Extensions, pkix.Extension{Id: oid(n), Value: der})
	}
	return c
}

// TestExtractBuilderIdentity_HappyPath: the GitHub-Actions OIDC build
// claims (source repo URI .1.12, source ref .1.14, build signer /
// workflow URI .1.9) are recovered from the cert. This is the
// TanStack-motivated signal: a change in the workflow ref across
// attested versions is the careful-variant tell that presence-only
// attestation misses.
func TestExtractBuilderIdentity_HappyPath(t *testing.T) {
	t.Parallel()

	cert := certWith(t, map[int]string{
		12: "https://github.com/tanstack/router",
		14: "refs/heads/main",
		9:  "https://github.com/tanstack/router/.github/workflows/release.yml@refs/heads/main",
	})

	id, ok := ExtractBuilderIdentity(cert)
	require.True(t, ok, "a cert carrying the build claims must extract")
	assert.Equal(t, "https://github.com/tanstack/router", id.SourceRepoURI)
	assert.Equal(t, "refs/heads/main", id.SourceRepoRef)
	assert.Equal(t,
		"https://github.com/tanstack/router/.github/workflows/release.yml@refs/heads/main",
		id.BuildSignerURI)
}

// TestExtractBuilderIdentity_AbsentAndNil: a cert without any build
// claim, and a nil cert, both yield ok=false (caller degrades to
// "builder unknown").
func TestExtractBuilderIdentity_AbsentAndNil(t *testing.T) {
	t.Parallel()

	_, ok := ExtractBuilderIdentity(nil)
	assert.False(t, ok)

	_, ok = ExtractBuilderIdentity(&x509.Certificate{})
	assert.False(t, ok, "no build-claim OIDs present → ok=false")
}

// TestExtractBuilderIdentity_RejectsUnsafeValues: these strings land
// in a persisted signal value and analyst-facing JSON. A
// newline/control-char-bearing or absurdly long value (attacker
// controls the cert; it is not cryptographically verified here) must
// be rejected, not propagated, so it can't corrupt downstream output.
func TestExtractBuilderIdentity_RejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	cert := certWith(t, map[int]string{
		12: "https://github.com/o/r\nInjected: evil",
		9:  "https://github.com/o/r/.github/workflows/x.yml@refs/heads/main",
	})
	id, ok := ExtractBuilderIdentity(cert)
	require.True(t, ok, "the safe build-signer URI still extracts")
	assert.Empty(t, id.SourceRepoURI,
		"the newline-bearing source-repo URI must be dropped, not propagated")
	assert.Equal(t,
		"https://github.com/o/r/.github/workflows/x.yml@refs/heads/main",
		id.BuildSignerURI)

	long := certWith(t, map[int]string{9: strings.Repeat("a", 5000)})
	_, ok = ExtractBuilderIdentity(long)
	assert.False(t, ok, "an over-long value is rejected (no usable claim left)")
}
