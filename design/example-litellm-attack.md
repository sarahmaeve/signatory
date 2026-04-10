# Example: LiteLLM PyPI Supply Chain Attack (March 2026)

This document analyzes the LiteLLM PyPI compromise as a case study for
signatory's PyPI signal collector. It is part of the broader TeamPCP
campaign that also touched npm, GitHub Actions, and OpenVSX in the
same week.

Source: Datadog Security Labs, "litellm Compromised: Inside the
TeamPCP PyPI Supply Chain Campaign"

## What Happened

The litellm PyPI package — a popular LLM proxy library with millions
of downloads — was compromised through stolen publishing credentials.
Two malicious versions were published: v1.82.7 and v1.82.8. Telnyx
was compromised the same week (v4.87.1, v4.87.2) as part of the same
campaign.

The malicious versions exfiltrated credentials, established
persistence, and polled for follow-on commands. Datadog explicitly
states any host that installed these versions should be treated as
"a full-credential exposure event."

Attribution: TeamPCP / UNC6780. The same actor responsible for the
prt-scan GitHub Actions campaign analyzed in
[example-prtscan-attack.md](example-prtscan-attack.md).

### TeamPCP Campaign Timeline (March 2026)

| Date | Target | Mechanism |
|------|--------|-----------|
| March 19 | Trivy | Stolen credentials |
| March 20–22 | npm worm | 45+ packages compromised |
| March 23 | Checkmarx, OpenVSX | Stolen credentials |
| March 24 | LiteLLM PyPI v1.82.7, v1.82.8 | Stolen credentials |
| March 27 | Telnyx PyPI v4.87.1, v4.87.2 | Stolen credentials |

A single threat actor compromised four ecosystems in nine days using
credential reuse and automated propagation. This is the
cross-ecosystem attack pattern we identified in the prt-scan analysis,
now confirmed.

## Two Execution Mechanisms

The campaign used different execution vectors in different versions
of the same package — a deliberate evasion tactic.

### v1.82.7: Source Code Injection

The malicious payload was injected directly into
`litellm/proxy/proxy_server.py`. Execution happened when the proxy
module was imported by an application.

This is the npm equivalent of modifying the package source — the
attack runs when the package is used, not when it's installed.

### v1.82.8: `.pth` File Injection (Higher Risk)

The package included a malicious `litellm_init.pth` file. Python
treats `.pth` files specially: executable lines in `.pth` files run
during Python interpreter startup, before any user code.

This means the payload executed every time Python started, regardless
of whether the user explicitly imported litellm. It's the Python
equivalent of npm's `postinstall` hook (axios case study) but worse:
postinstall runs once at install time, while `.pth` files run on
every Python interpreter startup.

**`.pth` file abuse is a Python-specific attack vector that signatory
must detect.** It is a high-priority signal for the PyPI provider.

## Payload Capabilities

The malware performed sequential operations:

1. **Credential harvesting**
   - Environment variables
   - SSH keys
   - Cloud credentials (AWS, GCP, Azure)
   - Kubernetes service account tokens
   - Docker configs
   - Shell history
   - Database credentials
   - Wallet files
   - CI/CD secrets

2. **Encryption** — AES-256 session key wrapped with RSA-4096

3. **Exfiltration** — sent to `models.litellm[.]cloud` with custom
   header `X-Filename: tpcp.tar.gz`

4. **Persistence**
   - `~/.config/sysmon/sysmon.py`
   - User systemd unit at `~/.config/systemd/user/sysmon.service`

5. **C2 polling** — `https://checkmarx[.]zone/raw` for follow-on
   commands

6. **Kubernetes exploitation** — created privileged `node-setup-*`
   pods when service account tokens were available, including
   destructive payload paths gated on Iranian timezone/locale

The Telnyx variant downloaded a WAV file from
`83.142.209[.]203:8080/ringtone.wav` with encrypted second-stage
payloads hidden in audio frames.

## Signals That Would Have Been Relevant

| Signal | What it catches |
|--------|----------------|
| **`.pth` file presence in PyPI package** | The v1.82.8 attack vector — `litellm_init.pth` would not exist in legitimate releases |
| **Publication metadata divergence** | Stolen-credential publication likely missing trusted publisher binding (PEP 740 attestations) |
| **New version published from unusual source** | If litellm normally publishes via CI, a manual upload via stolen API token deviates from the pattern |
| **Source code injection in main module** | v1.82.7 modified `proxy_server.py` — comparing published source to git tag would catch this |
| **Suspicious file in package** | `litellm_init.pth` in a package not previously containing `.pth` files |
| **Network exfiltration domains** | If we ever inspect package contents, hardcoded domains like `models.litellm[.]cloud` are red flags |
| **Cross-ecosystem actor correlation** | Same publisher email or infrastructure across multiple compromised packages in a short window |
| **Concurrent ecosystem attacks** | Trivy + npm + Checkmarx + LiteLLM + Telnyx in nine days = coordinated campaign — high baseline suspicion |
| **Maintainer credential rotation patterns** | Recent password reset, new API token issued, MFA changes |
| **Version bump pattern anomaly** | Two malicious versions in rapid succession (v1.82.7 + v1.82.8) — common for "first one didn't work, try harder" |

## Comparison with Axios Attack

| Dimension | Axios (npm) | LiteLLM (PyPI) |
|---|---|---|
| Attack vector | Maintainer account takeover | Stolen publisher credentials |
| Execution trigger | `npm install` postinstall hook | Python interpreter startup (`.pth`) |
| Source code modified? | No — injected dependency only | Yes — `proxy_server.py` modified |
| Multiple versions targeted | Yes (v1.14.1 + v0.30.4) | Yes (v1.82.7 + v1.82.8) |
| Anti-forensic cleanup | Yes (deleted setup.js) | No (less mature) |
| Threat actor | Lazarus / BlueNoroff (DPRK) | TeamPCP / UNC6780 |
| Multi-version strategy | Pin-targeting (current + dormant) | Iteration (two attempts at execution) |
| Cross-ecosystem | No (npm only) | Yes (PyPI + npm + GitHub Actions + OpenVSX) |

These are different attack groups using different techniques in the
same week. The supply chain is being attacked from multiple directions
by sophisticated actors simultaneously.

## Lessons for the PyPI Signal Collector

### 1. `.pth` files are a high-priority signal

PyPI packages should be scanned for `.pth` files. A package containing
a `.pth` file is not inherently malicious — some legitimate packages
use them — but it should always trigger inspection. A package that
suddenly introduces a `.pth` file in a new version when previous
versions had none is a strong anomaly signal.

### 2. Publication metadata divergence matters even more on PyPI

PyPI now supports trusted publishing via OIDC (PEP 740 attestations)
similar to npm's trusted publisher binding. If a package normally
publishes via OIDC and a new version appears without it, that is a
strong signal of credential compromise. The PyPI provider should
collect:

- Whether each version was published via trusted publishing
- The uploader identity (if available)
- The upload IP / source (if available via PyPI API)
- Comparison of upload mechanism across version history

### 3. Source code divergence detection is more important on PyPI than npm

Axios was compromised by injecting a dependency, not by modifying
source. LiteLLM v1.82.7 was compromised by directly modifying source.
This means the signatory check "does the published package match the
source repo?" is even more valuable for PyPI than for npm.

For PyPI specifically:
- Compare the published `.tar.gz` or wheel contents against the git
  tag for that version
- Flag any file differences as a high-priority anomaly
- This is computationally expensive but catches the most direct
  modification attacks

### 4. Cross-ecosystem actor correlation is now mandatory

Two of our three case studies (prt-scan, litellm) involve the same
threat actor operating across multiple ecosystems in the same week.
The signal model needs:

- **Actor fingerprinting** across ecosystems — same publisher email,
  same C2 infrastructure, same payload techniques
- **Temporal correlation** — multiple compromises in the same window
  raise the baseline suspicion for everything in that window
- **Campaign tracking** — once a campaign is identified, its IOCs
  (domains, IPs, file paths) become a burn list applicable across
  ecosystems

This argues for the federated burn list design from `trust-model.md`
to support cross-ecosystem campaign IOCs, not just package-specific
burns.

### 5. PyPI has unique attack surface signals

| Signal | Risk |
|---|---|
| `.pth` file present | Interpreter-time execution |
| `setup.py` with arbitrary code | Install-time execution (less risky than `.pth`) |
| `__init__.py` modifications between versions | Import-time execution |
| C extension binaries | Hard to audit, possible backdoor |
| Wheel vs sdist divergence | Different content in different distribution formats |
| Optional dependencies | Hidden install-time scripts |

The PyPI provider should collect all of these, not just publish
metadata.

## Risk Assessment

This attack is the second confirmation in three case studies that
**stolen publishing credentials are the dominant attack vector for
established packages**. axios used credential takeover via npm. LiteLLM
used stolen PyPI credentials. prt-scan used stolen GitHub credentials.

Vulnerability scanning does not catch any of these. There is no CVE
for "credentials were stolen." The defense is publication integrity
monitoring — verifying that publications match the project's normal
patterns, are signed by trusted infrastructure, and originate from
expected sources.

Signatory's value here is exactly the publication integrity signals
the design has emphasized. Adding `.pth` file detection and source
code divergence checking to the PyPI provider is straightforward;
the trust model already supports these as anomaly signals.

## Lessons for Signatory

1. **PyPI provider must check for `.pth` files.** This is a
   Python-specific install-time execution vector with no equivalent
   in other ecosystems. It is a high-confidence anomaly signal when
   a `.pth` file appears in a package that previously had none.

2. **Publication metadata is the highest-value signal across all
   ecosystems.** Three of our case studies (axios, litellm, prt-scan)
   involved attacks that bypassed source code review by publishing
   from compromised credentials. Vulnerability scanners cannot catch
   these. Publication integrity can.

3. **Cross-ecosystem campaigns are not hypothetical.** TeamPCP hit
   four ecosystems in nine days. Any signal collector that only looks
   at one ecosystem will miss the broader pattern. Federated burn
   lists for campaign IOCs (domains, IPs, infrastructure) are a v0.2
   priority that just became more urgent.

4. **`.pth` files run on every Python startup, not just install.**
   This means an infected dev machine continues to leak credentials
   long after the package is "uninstalled." Persistence detection
   must look at `.pth` files in site-packages and user-site
   directories.

5. **Two malicious versions in rapid succession is a signal.** Both
   axios and litellm published two malicious versions within hours
   of each other. The pattern: try one mechanism, if it doesn't
   immediately catch on, try another. Anomalous version cadence
   (multiple versions within hours when normal cadence is weeks) is
   itself a signal.

6. **Same campaign, multiple lenses.** This is the third TeamPCP
   case in our analysis (prt-scan, axios was different actor, this
   is the connecting case). The cross-ecosystem visibility we get
   by analyzing all three together is exactly what individual
   ecosystem-specific tools cannot provide.

## Sources

- Datadog Security Labs: "litellm Compromised: Inside the TeamPCP PyPI
  Supply Chain Campaign" (primary source)
- Cross-references: Google GTIG prt-scan analysis (same threat actor),
  Trivy disclosure, Checkmarx disclosure
