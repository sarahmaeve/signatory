package npm

import (
	"crypto/x509"
	"encoding/base64"

	"github.com/sarahmaeve/signatory/internal/sigstore/fulcio"
)

// AttestationsResponse is the npm provenance attestation envelope
// served by registry.npmjs.org's /-/npm/v1/attestations/<name>@<v>
// endpoint. Only the fields on the path from the response to the
// leaf cert are modelled — like RegistryPackage, the wire payload
// carries much more (DSSE envelope, Rekor entry, predicate) that the
// pin-table upgrade does not read.
//
// A version published with provenance typically yields two
// attestations: the SLSA provenance (whose Fulcio cert carries the
// source-repo-digest OID we want) and the npm publish attestation.
// The cert walk is first-match-wins across all of them, so the order
// and which one carries the OID does not matter.
type AttestationsResponse struct {
	Attestations []Attestation `json:"attestations"`
}

// Attestation is one entry in the response. The Sigstore bundle holds
// the verification material we extract the cert from.
type Attestation struct {
	PredicateType string            `json:"predicateType"`
	Bundle        AttestationBundle `json:"bundle"`
}

// AttestationBundle is the Sigstore bundle. Only the verification
// material is modelled.
type AttestationBundle struct {
	VerificationMaterial VerificationMaterial `json:"verificationMaterial"`
}

// VerificationMaterial carries the signing cert in one of two shapes
// Sigstore bundles use across versions: a single leaf
// (`certificate.rawBytes`, bundle v0.3+) or a chain
// (`x509CertificateChain.certificates[].rawBytes`, earlier bundles).
// Both are modelled so npm provenance is read regardless of which
// bundle version the publisher's tooling emitted. Pointers so an
// absent shape stays nil rather than decoding to a zero struct that
// looks like an empty cert.
type VerificationMaterial struct {
	Certificate          *CertBytes `json:"certificate"`
	X509CertificateChain *CertChain `json:"x509CertificateChain"`
}

// CertChain is the multi-cert shape; the leaf is certificates[0] but
// the walk tries every entry so a reordered chain still resolves.
type CertChain struct {
	Certificates []CertBytes `json:"certificates"`
}

// CertBytes wraps a base64 (standard encoding) DER certificate.
type CertBytes struct {
	RawBytes string `json:"rawBytes"`
}

// extractGitHeadFromNpmAttestation walks every cert in the envelope
// and returns the first Fulcio source-repo-digest it can recover. The
// chain per cert is: base64(std) decode → x509.ParseCertificate →
// fulcio.ExtractSourceRepoDigest (which applies the git-object-id
// trust-boundary gate).
//
// Designed for silent degradation, exactly like pypi's
// extractGitHeadFromAttestation: every failure mode (nil response, no
// attestations, neither cert shape present, malformed base64/DER,
// missing OID, non-SHA OID value) returns ("", false) and the caller
// keeps the gitHead pin rather than erroring.
func extractGitHeadFromNpmAttestation(resp *AttestationsResponse) (string, bool) {
	if resp == nil {
		return "", false
	}
	for _, att := range resp.Attestations {
		vm := att.Bundle.VerificationMaterial
		var raws []string
		if vm.Certificate != nil && vm.Certificate.RawBytes != "" {
			raws = append(raws, vm.Certificate.RawBytes)
		}
		if vm.X509CertificateChain != nil {
			for _, c := range vm.X509CertificateChain.Certificates {
				if c.RawBytes != "" {
					raws = append(raws, c.RawBytes)
				}
			}
		}
		for _, raw := range raws {
			der, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				continue
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				continue
			}
			if sha, ok := fulcio.ExtractSourceRepoDigest(cert); ok {
				return sha, true
			}
		}
	}
	return "", false
}
