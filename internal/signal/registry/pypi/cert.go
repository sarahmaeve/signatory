package pypi

import (
	"crypto/x509"
	"encoding/base64"

	"github.com/sarahmaeve/signatory/internal/sigstore/fulcio"
)

// extractFulcioSourceRepoDigest delegates to the shared
// internal/sigstore/fulcio extractor. Kept as a package-private
// alias so this package's PEP 740 envelope walk
// (extractGitHeadFromAttestation) and its existing tests read against
// one name; the cert-level OID lookup, DER unwrap, and the git-argv
// trust-boundary shape gate are owned by the shared package so npm
// provenance reuses the same audited code rather than copying it.
func extractFulcioSourceRepoDigest(cert *x509.Certificate) (string, bool) {
	return fulcio.ExtractSourceRepoDigest(cert)
}

// extractGitHeadFromAttestation walks a PEP 740 AttestationResponse
// and returns the first source-repo-digest SHA it can recover from
// any bundle's attestation cert. The chain it follows:
//
//	attest.Bundles[i].Attestations[j].VerificationMaterial.Certificate
//	  → base64-decode → x509.ParseCertificate → OID 1.3.6.1.4.1.57264.1.13
//
// Designed for silent degradation. Every failure mode (nil response,
// no bundles, no attestations, empty cert string, malformed base64,
// malformed cert, missing OID, malformed OID value) returns
// ("", false) and the caller (recordArtifactURL) ships an empty
// git_head — pair resolution falls through to tag-match, the
// existing pre-attestation behaviour.
//
// First-match-wins across bundles and attestations: a distribution
// with multiple bundles (e.g. re-attested by a second publisher)
// returns the first SHA. The PyPI Integrity API today emits one
// bundle per distribution; the loop is forward-compat for future
// shapes rather than load-bearing.
//
// The DSSE envelope signature, the Rekor inclusion proof, and the
// Fulcio CA chain are NOT verified here. This is presence-and-
// extraction, not cryptographic verification — the trust posture
// signatory operates at, where the Fulcio-issued cert claims are
// taken as evidence rather than verified end-to-end. Compliance-
// grade verification would layer on top via sigstore-go.
func extractGitHeadFromAttestation(attest *AttestationResponse) (string, bool) {
	if attest == nil {
		return "", false
	}
	for _, bundle := range attest.Bundles {
		for _, a := range bundle.Attestations {
			if a.VerificationMaterial.Certificate == "" {
				continue
			}
			der, err := base64.StdEncoding.DecodeString(a.VerificationMaterial.Certificate)
			if err != nil {
				continue
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				continue
			}
			if sha, ok := extractFulcioSourceRepoDigest(cert); ok {
				return sha, true
			}
		}
	}
	return "", false
}
