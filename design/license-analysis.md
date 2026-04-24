# Signatory: License Analysis

## Recommendation

**Apache 2.0** — pending final decision.

## Ecosystem Survey

| Tool | Purpose | License |
|---|---|---|
| Sigstore (cosign, rekor, fulcio) | Supply chain signing | Apache 2.0 |
| Trivy | Vulnerability scanner | Apache 2.0 |
| Syft | SBOM generation | Apache 2.0 |
| Grype | Vulnerability scanner | Apache 2.0 |
| in-toto | Supply chain layout | Apache 2.0 |
| SLSA tools | Provenance | Apache 2.0 |
| Socket.dev | Supply chain risk | Proprietary |
| Phylum | Supply chain analysis | Proprietary |

The open-source supply chain security ecosystem is overwhelmingly
Apache 2.0. Commercial competitors are proprietary.

## License Comparison

| License | Patent Grant | Commercial Use | Copyleft | Enterprise-Friendly | Complexity |
|---|---|---|---|---|---|
| **Apache 2.0** | Yes | Yes | No | Yes | Moderate |
| MIT | No | Yes | No | Yes | Minimal |
| BSD 3-Clause | No | Yes | No | Yes | Minimal |
| GPL v3 | Yes | Restricted | Yes | No — many companies ban it | High |
| AGPL v3 | Yes | Very restricted | Yes (network) | No — blanket bans common | High |
| BSL/SSPL | Varies | Restricted | Sort of | Confusing | High |

## Signatory's Dependencies

| Dependency | License | Compatible with Apache 2.0? |
|---|---|---|
| Kong | MIT | Yes |
| testify | MIT | Yes |
| modernc.org/sqlite | BSD 3-Clause | Yes |

All dependencies are compatible with Apache 2.0.

## Arguments for Apache 2.0

1. **Patent grant.** Signatory will handle cryptographic attestation and
   signing. Apache 2.0's explicit patent grant protects users. MIT and
   BSD do not include patent grants.

2. **Ecosystem alignment.** Every open-source tool in the supply chain
   security space uses Apache 2.0. A different license creates
   integration friction.

3. **Enterprise adoption.** Companies have legal playbooks for Apache 2.0.
   It is on the pre-approved list at most enterprises. Organizations that
   most need supply chain security tools are often the ones with the
   strictest license restrictions.

4. **Future commercial option.** Apache 2.0 for the core does not prevent
   offering a commercial enterprise version later (hosted dashboard,
   federated burn list service, etc.). This is the Grafana/GitLab
   model — open core with commercial additions.

## Arguments Against Alternatives

**MIT:** Simpler but no patent protection. For a tool handling
cryptographic operations and attestation, this is a meaningful gap.

**GPL/AGPL:** Would limit enterprise adoption. Organizations that need
supply chain security tools are exactly the ones with GPL restrictions.
AGPL would be relevant for the future hosted version but would severely
limit core adoption.

**BSL/SSPL:** Not OSI-approved. Creates confusion and signals "not
really open source" — exactly the wrong message for a trust tool.

## Open Questions

- Should we consider dual licensing (Apache 2.0 + commercial) for
  the enterprise tier?
- Is a CLA (Contributor License Agreement) needed if we want to
  retain the option to relicense later?
- Should the design documents, case studies, and dogfood analyses
  be under a different license (e.g., CC BY 4.0) than the code?

## Status

Pending decision. Tracked as issue #22.
