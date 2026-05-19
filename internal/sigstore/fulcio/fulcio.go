// Package fulcio extracts the Sigstore Fulcio "Source Repository
// Digest" claim — the publisher-stamped head commit SHA of the
// checkout a CI workflow built from — out of an x509 certificate.
//
// It is ecosystem-neutral: PyPI ships it inside a PEP 740 attestation
// envelope, npm inside a Sigstore provenance bundle, but the cert →
// OID → DER-unwrap → git-object-id-shape step is identical. The
// per-ecosystem envelope walk lives in each registry collector; this
// package owns only the cert-level extraction and the trust-boundary
// shape gate, so both ecosystems share one audited implementation
// rather than copying the OID and the git-argv defense.
//
// The Fulcio cert chain is NOT cryptographically verified here. This
// is presence-and-extraction at signatory's trust posture, where the
// Fulcio-issued cert claims are taken as evidence rather than verified
// end-to-end. Callers treat the recovered value as attacker-controlled
// (the publisher set it); IsGitObjectID is the chokepoint that keeps a
// forged value from reaching a git subprocess argv.
package fulcio

import (
	"crypto/x509"
	"encoding/asn1"
)

// SourceRepoDigestOID is Sigstore Fulcio's "Source Repository Digest"
// claim OID. Fulcio's CA stamps this extension onto certs issued for
// CI OIDC tokens; the value is the head commit SHA of the repository
// checkout the workflow run executed against.
//
// Per Sigstore's claim registry (Fulcio v1.3+ encoding):
// https://github.com/sigstore/fulcio/blob/main/docs/oid-info.md
var SourceRepoDigestOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 13}

// ExtractSourceRepoDigest walks cert.Extensions for the Fulcio
// "Source Repository Digest" OID and returns its DER-unwrapped string
// value.
//
// Returns ("", false) when:
//   - cert is nil (caller passed an empty cert chain);
//   - the OID isn't present (cert was issued for a non-Sigstore
//     identity, or a Sigstore builder that doesn't populate this
//     claim);
//   - the extension value isn't a DER-decodable string (corrupt cert,
//     format drift, or attacker-supplied garbage);
//   - the decoded value is not a syntactically valid git object id
//     (40-hex SHA-1 or 64-hex SHA-256). The value is attacker-
//     controlled and flows verbatim into `git ls-tree`, `git cat-file
//     --batch` stdin, and `git diff` argv downstream via the
//     version_pin_table SHA. Anything that is not a bare object id —
//     flag-shaped, whitespace/newline-bearing, or non-hex — is
//     rejected at this trust boundary rather than handed to git.
//
// Silent fall-through is the contract: callers treat absence as "no
// exact SHA available" and degrade (empty git_head → tag-match path).
func ExtractSourceRepoDigest(cert *x509.Certificate) (string, bool) {
	if cert == nil {
		return "", false
	}
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(SourceRepoDigestOID) {
			continue
		}
		var s string
		if rest, err := asn1.Unmarshal(ext.Value, &s); err == nil && len(rest) == 0 && IsGitObjectID(s) {
			return s, true
		}
		// Format drift defense: pre-v1.3 Fulcio emitted raw bytes for
		// some claims. asn1.Unmarshal failure here means the value
		// isn't a wrapped string; cleanest recovery is to admit
		// "didn't recognise" rather than ship raw bytes (which would
		// include the SHA but also potentially garbage header bytes,
		// breaking exact-match comparison downstream).
		return "", false
	}
	return "", false
}

// IsGitObjectID reports whether s is a syntactically valid git object
// name: a lowercase-or-uppercase hex string of exactly SHA-1 (40) or
// SHA-256 (64) length. This is the shape git itself accepts as a full
// object id; abbreviated ids are intentionally rejected because Fulcio
// always stamps the full commit SHA, and accepting a short prefix
// would only widen what attacker-controlled bytes we forward to git.
// No allocation, no regexp — a tight constant-time scan on the
// trust-boundary hot path.
func IsGitObjectID(s string) bool {
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
