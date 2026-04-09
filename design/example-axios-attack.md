# Example: Axios Supply Chain Attack (March 2026)

This document analyzes the axios npm supply chain attack as a case study for
signatory's signal model, particularly the dual-edged nature of criticality.

## What Happened

The axios npm package (~100M weekly downloads for v1.x, ~83M for v0.x) was
compromised through a maintainer account takeover (ATO). The attacker changed
the maintainer's associated email to an attacker-controlled address
(`ifstap@proton.me`), then injected malicious code into releases v1.14.1
and v0.30.4, adding `plain-crypto-js@4.2.1` as a malicious dependency.

The attack window was approximately 3 hours (March 31, 2026, 00:21–03:20
UTC). In that window, the compromised versions were downloaded and installed
across thousands of downstream projects and CI/CD pipelines.

The attack weaponized the distribution channel itself, bypassing CI/CD
guardrails. It was not a code vulnerability — it was a supply chain attack
on the publication pipeline. Notably, the axios source code was never
modified — only `package.json` was changed to inject the malicious dependency.

Attribution: DPRK-affiliated.
- Sapphire Sleet (Microsoft)
- UNC1069 (Google GTIG), active since 2018, attributed via WAVESHAPER.V2
  malware lineage and AstrillVPN node reuse on C2 infrastructure
- BlueNoroff / Lazarus Group (Vectra AI)
- WAVESHAPER / UNC1069 overlap (Palo Alto Unit 42)

The attack was patched in under 24 hours — extremely fast for open source —
but compromised thousands of downstream projects in that window. Google
estimates "hundreds of thousands of stolen secrets could potentially be
circulating" as a result.

### This Attack Is Not Isolated

Google explicitly connects the axios compromise to a broader pattern of
concurrent supply chain attacks. UNC6780/TeamPCP poisoned GitHub Actions
and PyPI packages associated with Trivy, Checkmarx, and LiteLLM to deploy
the SANDCLOCK credential stealer in the same timeframe. Multiple threat
actors, multiple ecosystems, simultaneously.

## Measured Blast Radius (from Wiz)

- axios is present in **~80% of cloud and code environments**
- In the ~3-hour attack window, **3% of affected environments executed**
  the malicious versions
- At axios's scale (~100M weekly downloads), 3% execution in 3 hours
  represents tens of thousands of compromised environments

This is the first concrete measurement of blast radius from any of the
vendor analyses. It gives signatory a benchmark: if anomalies on packages
at this criticality level can be surfaced in minutes rather than hours,
the execution rate drops proportionally.

## Technical Attack Chain (from Unit 42 and Microsoft analysis)

1. **Maintainer account takeover** — compromised npm credentials for an
   axios maintainer.

2. **Dependency seeding** — attackers first published `plain-crypto-js@4.2.0`
   as a *clean* package to establish publishing history and reduce scrutiny.
   Then `@4.2.1` added the malicious install-time logic. This is a two-stage
   attack designed to defeat naive "how old is this package?" checks.

3. **Dependency injection** — modified axios `package.json` to add
   `plain-crypto-js@^4.2.1` as a dependency. Axios source code untouched.
   Microsoft notes: "The publication metadata differs from the project's
   normal CI-backed publishing pattern (missing trusted publisher binding
   and missing corresponding repo tag/commit trail for the malicious
   version)."

4. **Postinstall hook execution** — `npm install` triggers `setup.js` via
   npm's `postinstall` lifecycle hook. Heavily obfuscated (string reversal,
   Base64, XOR cipher with key `OrDeR_7077`).

5. **Platform-specific RAT deployment** (~15 seconds):
   - macOS: AppleScript → C++ Mach-O binary at `/Library/Caches/com.apple.act.mond`
   - Windows: PowerShell copied as `%PROGRAMDATA%\wt.exe`, VBScript → PowerShell RAT, registry persistence
   - Linux: Node.js execSync → Python RAT at `/tmp/ld.py`

6. **C2 beaconing** — all variants beacon every 60 seconds to
   `sfrclak[.]com:8000`, using paths that mimic npm registry URLs
   (e.g., `packages.npm[.]org/product0`).

7. **Anti-forensic cleanup** — dropper deletes `setup.js`, removes the
   postinstall hook, replaces `package.json` with a clean decoy file.
   Evidence of compromise is erased from the package directory.

8. **Multi-version targeting** — published to both v1.14.1 and v0.30.4,
   deliberately targeting users pinned to older major versions.

## Version Targeting Detail

Publishing malicious patches to both the current (v1.x) and a long-dormant
(v0.30.x) version branch is a deliberate tactic. Attackers targeted users
who had version-pinned to older releases — a common enterprise practice.
A signal that flags "unusual publication to a long-dormant version branch"
would catch this pattern.

## Signals That Would Have Been Relevant

| Signal | What it catches |
|--------|----------------|
| Distribution integrity | Published package includes `plain-crypto-js` — absent from source repo |
| New dependency anomaly | A previously unknown package suddenly added as a transitive dependency |
| Criticality score | axios is extremely high-criticality — any anomaly warrants immediate investigation |
| Publish pattern change | Account takeover may change publish timing, IP, or tooling |
| **Publication metadata divergence** | **Malicious versions missing trusted publisher binding, no corresponding repo tag/commit trail — deviates from project's normal CI-backed publishing pattern (Microsoft)** |
| **Trusted publisher binding absence** | **Package normally uses OIDC-based trusted publishing; malicious version published without it** |
| Dependency provenance | `plain-crypto-js@4.2.1` scores extremely poorly: new package, no history, no community, no provenance |
| **Dependency seeding pattern** | **`plain-crypto-js@4.2.0` published clean first to establish history; `@4.2.1` added malware — defeats naive age-based trust (Microsoft)** |
| Install script change | New `postinstall` hook appearing in an established package that previously had none |
| Dormant version branch publication | Patch published to long-inactive v0.30.x branch — unusual for a package that has moved to v1.x |
| Anti-forensic behavior | Package modifies its own `package.json` and deletes scripts post-install (detectable by comparing package state at install time vs. after) |
| **Version range exploitation** | **Consumers using `^` or `~` ranges auto-upgraded to malicious version — consumer-side pinning hygiene is itself a posture signal** |
| **Maintainer email change** | **Maintainer's associated email changed to `ifstap@proton.me` — account metadata change on a critical package is a high-priority anomaly (Google GTIG)** |
| **Concurrent ecosystem attacks** | **Multiple threat actors attacking multiple ecosystems simultaneously (npm + PyPI + GitHub Actions) — cross-ecosystem correlation could surface coordinated campaigns (Google GTIG)** |

Even without detecting the account takeover itself, signatory could have
flagged that a top-10 npm package suddenly depends on a brand-new package
with no provenance. That signal alone should trigger immediate investigation.

Multiple independent signals converge here. Any one of them is suspicious;
the combination is damning. This validates the compositional signal model —
no single signal is definitive, but the composite picture is clear.

## Design Insight: Criticality as a Multiplier

Criticality is not just a score — it is a **multiplier** that amplifies both
positive and negative signals:

- **High criticality + stable signals** = high confidence, but also
  high-priority monitoring target
- **High criticality + any anomaly** = immediate alert, regardless of how
  small the anomaly appears in isolation
- **High criticality + degradation** = organizational emergency

**The monitoring threshold should be inversely proportional to criticality.**
The more critical a dependency is, the smaller the anomaly needed to trigger
investigation.

This is the dual-edged nature of popularity and adoption:
- Wide install base is a positive signal (many eyes, active maintenance)
- Wide install base is also a risk amplifier (high-value target, maximum
  blast radius if compromised)
- The same signal that builds trust also demands greater vigilance

## Lessons for Signatory

1. **Distribution integrity is a high-value, automatable signal.** Comparing
   published packages against their source repos is one of the most concrete
   things signatory can do. A divergence between the two is never benign.

2. **Transitive dependency provenance matters.** The attack vector was not
   axios itself but a malicious transitive dependency. Signatory must assess
   not just direct dependencies but what *they* depend on — and flag when
   a well-established package suddenly introduces an unknown dependency.

3. **Speed matters for critical dependencies.** 24 hours to patch is
   impressive. Thousands of projects compromised in 24 hours is devastating.
   For high-criticality dependencies, signatory's monitoring and alerting
   must operate on a timeline of hours, not days.

4. **CVE-centric models miss this class of attack entirely.** There was no
   CVE for axios before the compromise. Traditional vulnerability scanning
   would not have flagged it. Signatory's signal-based approach — looking at
   provenance, distribution integrity, and behavioral anomalies — addresses
   the gap that CVE-centric tools leave open.

5. **State-level attackers target the supply chain.** Lazarus Group / 
   BlueNoroff attribution means the adversary is well-resourced and
   sophisticated. The supply chain is a deliberate target for nation-state
   actors, not just opportunistic attackers. Signatory's trust model must
   be designed for this threat level.

6. **Install scripts are a high-priority signal for npm.** The `postinstall`
   hook was the execution vector. A new or changed install script in an
   established package is one of the strongest individual anomaly signals
   available in the npm ecosystem. This should be a v0.1 signal for the
   npm provider.

7. **Dormant version branch activity is suspicious.** Publishing to v0.30.x
   while v1.x is active is an unusual pattern that specifically targets
   pinned users. Signatory should track version branch activity patterns
   and flag publications to long-inactive branches.

8. **Signatory operates upstream of endpoint security.** Palo Alto's
   recommendations are post-compromise: detect the RAT, isolate the system,
   rebuild. Signatory's value is *before* compromise: flag the anomalous
   package before `npm install` executes. These are complementary, not
   competing — but signatory occupies the gap that endpoint products don't
   cover.

9. **Anti-forensic behavior is itself a signal.** The payload deleted its
   own traces. If signatory snapshots or checksums package contents at
   install time and compares post-install, self-modifying packages are
   detectable. This is an npm-specific signal worth investigating for v0.1.

10. **Publication metadata divergence is a high-confidence signal.**
    Microsoft's analysis reveals that the malicious axios versions were
    published without trusted publisher binding and without corresponding
    repo tags or commit trails. This is a deviation from the project's
    normal CI-backed publishing pattern. Signatory should compare
    publication metadata across versions of the same package — a change
    in *how* something was published is as important as *what* was published.

11. **Dependency seeding defeats naive age checks.** The attackers published
    a clean version of `plain-crypto-js` first to build false history.
    Package age alone is insufficient — signatory must look at the *pattern*
    of publication (single initial version followed immediately by a
    second version in a short window, no organic adoption between them).

12. **Consumer-side version pinning is a posture signal.** The attack
    propagated via npm's `^` range resolution. Whether a consuming project
    pins exact versions, uses ranges, or has lockfile discipline is a
    signal about that project's exposure to this class of attack. This
    is a signal about the *consumer*, not the dependency.

13. **Trusted Publishing (OIDC) is a concrete positive signal.** Microsoft
    recommends adopting trusted publishing to eliminate stored credentials.
    Whether a package uses OIDC-based trusted publishing is a binary,
    queryable signal. If a package normally uses it and a version was
    published without it, that's an immediate red flag.

14. **Maintainer account metadata changes are a signal.** Google identified
    that the compromised maintainer's email was changed to an
    attacker-controlled address. Changes to maintainer contact information
    on critical packages — especially to privacy-focused email providers —
    should surface as anomalies.

15. **Cross-ecosystem correlation could detect coordinated campaigns.**
    The axios attack was concurrent with UNC6780/TeamPCP attacks on PyPI
    and GitHub Actions. If signatory monitors multiple ecosystems, it could
    potentially detect coordinated multi-ecosystem campaigns that
    single-ecosystem tools would miss.

16. **3 hours is enough.** The attack window was approximately 3 hours.
    For a package with 100M+ weekly downloads, that's enough to compromise
    thousands of projects and CI/CD pipelines. Detection and alerting for
    critical packages must operate on a timeline of minutes to hours.

17. **Every major vendor's response is post-compromise.** Google provides
    YARA rules and SecOps queries. Microsoft provides Defender signatures
    and Sentinel hunting queries. Palo Alto provides WildFire and Cortex
    detections. All of these detect the RAT *after* it's running. None of
    them prevent the malicious `npm install` from executing. Signatory
    occupies the gap upstream of all of them.

## Broader Pattern

Google's quote captures the thesis: supply chain compromise *"abuses the
inherent trust that users and enterprise administrators place in hardware,
software, and updates supplied by reputable vendors as well as the trust
they may not realize they are placing in collaborative code-sharing
communities."*

Signatory's purpose is to make that implicit trust explicit, auditable, and
revocable.

## Sources

- Vectra AI: "The Axios Breach: A Wake-Up Call for Software Supply Chain
  Security" — initial reporting, attribution to BlueNoroff/Lazarus Group
- Palo Alto Unit 42: technical analysis with full attack chain forensics,
  IOCs, payload analysis, attribution overlap with WAVESHAPER/UNC1069
- Microsoft Threat Intelligence: attribution to Sapphire Sleet, publication
  metadata divergence analysis, trusted publishing recommendations,
  dependency seeding technique, Defender detection signatures and
  hunting queries
- Google GTIG: attribution to UNC1069 via WAVESHAPER.V2 lineage, precise
  attack timeline (3-hour window), maintainer email change detail,
  concurrent UNC6780/TeamPCP campaign context, YARA rules, infrastructure
  attribution via AstrillVPN node reuse
- Wiz: blast radius measurement (80% environment presence, 3% execution
  in 3-hour window), operational impact quantification
