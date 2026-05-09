package pypi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fulcioSourceRepoDigestOID is what extractFulcioSourceRepoDigest
// must look up. Mirrored in the test so a typo in the production
// constant breaks the test loudly rather than the test silently
// agreeing with a wrong value.
//
// 1.3.6.1.4.1.57264.1.13 = "Source Repository Digest" per Sigstore's
// Fulcio claim OID registry. Fulcio populates this from the GitHub
// OIDC token's `sha` claim — the head commit of the workflow run
// that produced the artifact. Cryptographically vouched: only
// Fulcio's CA can issue certs with this extension, and Fulcio only
// issues against legitimate OIDC tokens.
var fulcioSourceRepoDigestOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 13}

// TestExtractFulcioSourceRepoDigest_PinsHappyPath is the smallest
// claim of the PEP 740 → exact_gitHead pipeline: given a parsed
// x509 cert whose extension at the source-repo-digest OID carries
// a DER-encoded UTF8String SHA, the helper returns the SHA verbatim.
//
// Per Fulcio v1.3+ encoding (the current format produced by
// fulcio.sigstore.dev), every extension value is a DER-encoded
// ASN.1 string — not raw bytes. This test pins the unwrap.
//
// Synthesised cert rather than a real Fulcio fixture: the OID-
// lookup logic is what's under test. Real-cert verification lands
// in a separate test once the wire model and base64 path are wired.
func TestExtractFulcioSourceRepoDigest_PinsHappyPath(t *testing.T) {
	t.Parallel()

	const wantSHA = "ec11c4a93de22cde2abe2bf74d70791033c2464c"

	// Wrap the SHA as a DER UTF8String — what Fulcio v1.3+ emits.
	derValue, err := asn1.MarshalWithParams(wantSHA, "utf8")
	require.NoError(t, err)

	cert := &x509.Certificate{
		Extensions: []pkix.Extension{
			{
				Id:    fulcioSourceRepoDigestOID,
				Value: derValue,
			},
		},
	}

	got, ok := extractFulcioSourceRepoDigest(cert)
	require.True(t, ok,
		"extension at the source-repo-digest OID is present; helper must "+
			"locate it and return ok=true")
	assert.Equal(t, wantSHA, got,
		"extracted value must be the inner UTF8String contents, not the "+
			"DER-encoded outer bytes — failure here means the ASN.1 unwrap "+
			"step was skipped")
}

// TestExtractFulcioSourceRepoDigest_ReturnsFalseWhenAbsent pins
// the negative path: a cert without the source-repo-digest
// extension yields ("", false). Drives the caller's fallback
// behaviour (empty git_head → tag-match path in the downstream
// artifact-vs-repo collector).
func TestExtractFulcioSourceRepoDigest_ReturnsFalseWhenAbsent(t *testing.T) {
	t.Parallel()

	// A cert with a different extension (build-signer URI, not
	// source-repo-digest). The helper must not return that value.
	otherOID := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 9}
	otherValue, err := asn1.MarshalWithParams("https://example/run", "utf8")
	require.NoError(t, err)

	cert := &x509.Certificate{
		Extensions: []pkix.Extension{
			{
				Id:    otherOID,
				Value: otherValue,
			},
		},
	}

	got, ok := extractFulcioSourceRepoDigest(cert)
	assert.False(t, ok,
		"missing source-repo-digest OID must return ok=false so the "+
			"caller falls through to the empty-git_head path")
	assert.Equal(t, "", got)
}

// TestExtractFulcioSourceRepoDigest_NilCert covers the nil-safety
// contract: callers might pass a nil cert (e.g. when the bundle's
// cert chain was empty). Helper must not panic.
func TestExtractFulcioSourceRepoDigest_NilCert(t *testing.T) {
	t.Parallel()

	got, ok := extractFulcioSourceRepoDigest(nil)
	assert.False(t, ok)
	assert.Equal(t, "", got)
}

// TestExtractGitHeadFromAttestation_RecoversFromBase64DER pins the
// end-to-end claim of the PEP 740 → exact_gitHead pipeline: given
// an AttestationResponse whose bundles[].attestations[].
// verification_material.certificate carries a base64-encoded DER
// cert with the source-repo-digest OID, the helper recovers the SHA.
//
// PyPI's Integrity API uses Sigstore bundle format v0.3+, which
// encodes the cert as a single leaf (not a chain). Per PEP 740 and
// the bundle protobuf spec confirmed by spec lookup:
//
//   - verification_material.certificate is "base64(DER(cert))"
//   - the cert is a single leaf, not a CertificateChain
//   - OID 1.3.6.1.4.1.57264.1.13 carries the source repo digest as
//     a DER UTF8String
//
// This test synthesises a self-signed cert with that exact extension,
// then reproduces the wire-to-SHA chain: base64 decode → x509 parse →
// OID extension lookup → DER UTF8String unwrap.
func TestExtractGitHeadFromAttestation_RecoversFromBase64DER(t *testing.T) {
	t.Parallel()

	const wantSHA = "ec11c4a93de22cde2abe2bf74d70791033c2464c"

	// Construct the OID extension value: a DER UTF8String wrapping
	// the SHA. asn1.MarshalWithParams(..., "utf8") produces the
	// 0x0C-tagged form Fulcio emits.
	oidValue, err := asn1.MarshalWithParams(wantSHA, "utf8")
	require.NoError(t, err)

	// Build a self-signed cert carrying the extension. Real Fulcio
	// certs are EC P-256 with full SCT and chain context; for the
	// extraction test only the extension matters.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtraExtensions: []pkix.Extension{
			{Id: fulcioSourceRepoDigestOID, Value: oidValue},
		},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	require.NoError(t, err)

	// Wrap in the on-the-wire shape PyPI ships.
	attest := &AttestationResponse{
		Version: 1,
		Bundles: []AttestationBundle{
			{
				Publisher: AttestationPublisher{Kind: "GitHub"},
				Attestations: []SigstoreAttestation{
					{
						VerificationMaterial: VerificationMaterial{
							Certificate: base64.StdEncoding.EncodeToString(derBytes),
						},
					},
				},
			},
		},
	}

	got, ok := extractGitHeadFromAttestation(attest)
	require.True(t, ok,
		"with a base64-DER cert carrying the source-repo-digest OID, "+
			"the helper must walk the bundle path and recover the SHA")
	assert.Equal(t, wantSHA, got,
		"recovered SHA must match the OID extension's UTF8String value")
}

// TestExtractGitHeadFromAttestation_NilOrEmpty pins the
// "no exact SHA available" path that the caller (recordArtifactURL)
// uses to decide between filling git_head vs leaving it empty.
func TestExtractGitHeadFromAttestation_NilOrEmpty(t *testing.T) {
	t.Parallel()

	got, ok := extractGitHeadFromAttestation(nil)
	assert.False(t, ok)
	assert.Equal(t, "", got)

	// Empty bundle slice: "trusted publishing not in use".
	got, ok = extractGitHeadFromAttestation(&AttestationResponse{Version: 1})
	assert.False(t, ok)
	assert.Equal(t, "", got)

	// Bundle with no attestations: malformed but possible; must
	// not panic and must return ok=false.
	got, ok = extractGitHeadFromAttestation(&AttestationResponse{
		Version: 1,
		Bundles: []AttestationBundle{{Publisher: AttestationPublisher{Kind: "GitHub"}}},
	})
	assert.False(t, ok)
	assert.Equal(t, "", got)
}
