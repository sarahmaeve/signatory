package pypi

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
)

// fulcioSourceRepoDigestOIDProd is Sigstore Fulcio's "Source
// Repository Digest" claim OID. Fulcio's CA stamps this extension
// onto certs issued for GitHub-Actions OIDC tokens; the value is
// the head commit SHA of the repository checkout the workflow
// run executed against — the publisher-stamped commit we want for
// exact_gitHead pairing.
//
// Per Sigstore's claim registry (Fulcio v1.3+ encoding):
// https://github.com/sigstore/fulcio/blob/main/docs/oid-info.md
//
// Each value is a DER-encoded ASN.1 string (UTF8String for hex-
// shaped values like this one). The unwrap is load-bearing —
// returning the raw extension Value bytes would surface the DER
// header (e.g. "\x0c\x28...") prepended to the SHA.
var fulcioSourceRepoDigestOIDProd = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 13}

// extractFulcioSourceRepoDigest walks cert.Extensions for the
// Fulcio "Source Repository Digest" OID and returns its DER-
// unwrapped string value.
//
// Returns ("", false) when:
//   - cert is nil (caller passed an empty cert chain);
//   - the OID isn't present (cert was issued for a non-Sigstore
//     identity, or for a Sigstore identity whose builder doesn't
//     populate this claim);
//   - the extension value isn't a DER-decodable string (corrupt
//     cert, format drift, or attacker-supplied garbage);
//   - the decoded value is not a syntactically valid git object id
//     (40-hex SHA-1 or 64-hex SHA-256). The Fulcio cert chain is NOT
//     cryptographically verified here, so this string is fully
//     attacker-controlled; it flows verbatim into `git ls-tree`,
//     `git cat-file --batch` stdin, and `git diff` argv downstream
//     (via the version_pin_table SHA). Anything that is not a bare
//     object id — flag-shaped, whitespace/newline-bearing, or
//     non-hex — is rejected at this trust boundary rather than
//     handed to a git subprocess.
//
// Silent fall-through is the contract: the caller (PyPI registry
// collector emitting artifact_url) treats absence as "no exact
// SHA available" and ships empty git_head, which routes the
// downstream artifact-vs-repo collector to its tag-match path.
// An error here would force the whole signal-collection chain to
// degrade — falling back is correct.
func extractFulcioSourceRepoDigest(cert *x509.Certificate) (string, bool) {
	if cert == nil {
		return "", false
	}
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(fulcioSourceRepoDigestOIDProd) {
			continue
		}
		var s string
		if rest, err := asn1.Unmarshal(ext.Value, &s); err == nil && len(rest) == 0 && isGitObjectID(s) {
			return s, true
		}
		// Format drift defense: pre-v1.3 Fulcio emitted raw bytes
		// for some claims. asn1.Unmarshal failure here means the
		// value isn't a wrapped string; cleanest recovery is to
		// admit "didn't recognise" rather than ship the raw bytes
		// (which would include the SHA but also potentially garbage
		// header bytes, breaking exact-match comparison downstream).
		return "", false
	}
	return "", false
}

// isGitObjectID reports whether s is a syntactically valid git
// object name: a lowercase-or-uppercase hex string of exactly SHA-1
// (40) or SHA-256 (64) length. This is the shape git itself accepts
// as a full object id; abbreviated ids are intentionally rejected
// because Fulcio always stamps the full commit SHA, and accepting a
// short prefix would only widen what attacker-controlled bytes we
// forward to git. No allocation, no regexp — a tight constant-time
// scan on the trust-boundary hot path.
func isGitObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
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
