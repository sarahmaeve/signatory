// Package contentinjection scans supply-chain text content for
// structural prompt-injection primitives — the byte-level and regex
// patterns an attacker must include to make an AI-targeted payload
// functional, regardless of the prose semantics.
//
// Specced by design/potential/anti-subversion.md (promoted from
// "potential" by the Trapdoor 2026-05 campaign, which weaponized
// .cursorrules and CLAUDE.md as zero-width-Unicode carriers).
//
// # The forgery-resistance property
//
// The scanner targets *structural* surfaces rather than semantic
// content. Each primitive corresponds to a byte-level or regex
// pattern an attacker who needs the payload to function cannot also
// hide. A zero-width character that's been stripped no longer hides
// the token-split; a markdown comment that renders visible no longer
// hides the imperative prose. The functional payload and the
// detected primitive are the same bytes.
//
// # The primitives
//
// Eight primitives per the design doc, grouped by detection mechanism:
//
//   - Rune-scan family (single pass, three findings emitted):
//     PrimitiveInvisibleUnicode, PrimitiveBidiControl, PrimitiveTagBlock.
//   - Regex family: PrimitiveMarkdownComment, PrimitiveMarkdownImage,
//     PrimitiveLexicalInjection.
//   - Length-distribution family: PrimitiveEncodedBlob.
//   - Script-mix family: PrimitiveConfusableMixedScript (added 2026-05).
//
// # The shared-package property
//
// The package is intentionally placed at internal/contentinjection
// (top-level under internal/, not under internal/signal/) because
// it is consumed by two distinct surfaces per the design docs:
//
//   - The anti-subversion signal collectors (Trapdoor-shape agent-
//     config scans, README / PR / release-notes scans) live under
//     internal/signal/ and consume this package via Scan / ScanFile.
//   - The hardening egress fence (design/hardening.md §1) will use
//     the same primitive definitions to gate signatory's own MCP
//     output against injection-shaped content.
//
// Both surfaces share the same threat-model vocabulary; this package
// is the single source of truth for what those primitives mean.
//
// # False-positive policy
//
// The design doc names false-negatives as the more expensive failure
// mode — an attacker who slips by is a real breach, while a noisy
// alert on benign content is a label-and-move-on cost. Detection
// thresholds throughout are tuned accordingly: the package will
// surface ordinary i18n test fixtures, package READMEs that discuss
// prompt injection as a topic, and signature-bearing release notes
// as positive findings. The analyst layer is responsible for
// weighting by file role and project topic.
package contentinjection
