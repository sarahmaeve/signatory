# Example: prt-scan AI-Assisted GitHub Campaign (March–April 2026)

This document analyzes the prt-scan campaign as a counterpoint to the axios
attack, illustrating a different threat profile: AI-augmented, low-skill,
high-volume supply chain attacks targeting CI/CD misconfigurations.

Source: Wiz Research (full report), Dark Reading, Aikido Security.

## What Happened

A single threat actor used AI-assisted automation to open 500+ malicious
pull requests against GitHub repositories over six waves spanning March 11
to April 3, 2026, targeting a well-known misconfiguration: the
`pull_request_target` workflow trigger in GitHub Actions.

The actor operated through six GitHub accounts, all linked via Proton Mail
email aliases (`testedbefore@proton.me`, `testedbefore+89@proton.me`,
`testedbefore+55@proton.me`, `testedbefore+99@proton.me`,
`elzotebo@proton.me`, `elzotebo+88@proton.me`).

Fewer than 10% of attempts succeeded, but dozens of repositories were
compromised, including at least 2 npm packages (106 malicious versions
across @codfish/eslint-config and @codfish/actions). Verified credential
theft included AWS keys, Cloudflare API tokens, and Netlify auth tokens.

A related earlier campaign ("hackerbot-claw," late February 2026) targeted
the same misconfiguration against higher-profile repos with more success
across 7 high-profile attacks using 5 different exploitation methods.

## Six Waves: Attacker Evolution

The campaign shows clear learning and iteration over three weeks:

### Phase 1: Probing (March 11–16)

**Wave 1a: testedbefore** (March 11–13, 10 PRs)
- Small repos targeted. Testing multiple injection vectors.
- Raw shell payloads: `setup.py` using `os.system()`, Makefile targets.
- Already building the RECON/DISPATCH pattern structure.

**Wave 1b: beforetested-boop** (March 13–16, 47 PRs)
- Scaling up. Larger projects targeted.
- Same crude payloads: `package.json` and Makefile injections.

### Phase 2: Deployment (March 28–29)

**Wave 2a: 420tb** (March 28, 11 PRs in 37 minutes)
- After two-week break. Beginning to polish approach.

**Wave 2b: 69tf420** (March 28–29)
- Consistent raw 351-line bash script (`.github/prt_exfil.sh`) with
  five-phase payload: EXFIL → RECON → DISPATCH → LABEL_BYPASS → DELAYED.
- No obfuscation. Dropped directly into workflow directory.

### Phase 3: AI-Augmented Scale (April 2–3)

**Wave 3a: elzotebo** (April 2, 13 PRs)
- Experimenting with "smart" file placement.
- High-profile repos targeted.
- Includes AI hallucinations (e.g., `pip.py`), frequently falls back to
  bash payload.

**Wave 3b: ezmtebo** (April 2–3, 475+ PRs in 26 hours)
- The publicly reported wave. Account created at 20:59 UTC, first
  malicious PR opened two minutes later.
- Full AI-generated, language-aware payloads:
  - `conftest.py` for Python/pytest repos
  - `package.json` scripts for Node.js repos
  - `build.rs` for Rust repos
  - `action.yml` composite actions for GitHub Actions
- Heavy stages base64-encoded. Repository-aware targeting.

## Attack Mechanism

1. **Scan** — identify repos using `pull_request_target` trigger in GitHub
   Actions (automated)
2. **Fork** — fork the target repository
3. **Branch** — create branch named `prt-scan-{12-hex-chars}`
4. **Inject** — AI-generated, language-appropriate payload into a file
   that executes during CI
5. **PR** — open PR with title "ci: update build configuration" and body
   "Automated build configuration update." (consistent across all waves)
6. **Trigger** — `pull_request_target` runs workflow with full repo
   permissions and access to secrets, even from an untrusted fork

### Five-Phase Payload Structure

Every payload follows the same structure regardless of injection point:

| Phase | Action | Works? |
|-------|--------|--------|
| EXFIL | Dump env vars + token to base64-encoded workflow logs | Yes |
| RECON | Enumerate secrets/workflows via API, probe cloud metadata (AWS/Azure/GCP IMDS) | Yes |
| DISPATCH | Push temp workflow to capture secret values | No (needs write perms) |
| LABEL_BYPASS | Create and apply labels to bypass gated workflows | No (circular, needs write) |
| DELAYED | Background `/proc/*/environ` scanner for 10min, exfil via PR comments | Rarely (needs shared runners) |

Only the first two phases typically work. The last three reveal the
attacker's fundamental misunderstanding of GitHub's permission model.

### Credential Targets

The DELAYED phase's secret scanner targets (among others):
```
NETLIFY, ALCHEMY, INFURA, STRIPE, AWS_SECRET, NPM_TOKEN, DOCKER,
CLOUDFLARE, DATABASE_URL, PRIVATE_KEY, SENTRY, SENDGRID, TWILIO,
PAYPAL, OPENAI, ANTHROPIC, GEMINI, DEEPSEEK, COHERE, MONGODB,
REDIS_URL, SSH_PRIVATE
```

Notable: AI API keys (OpenAI, Anthropic, Gemini, DeepSeek, Cohere) are
explicitly targeted alongside traditional infrastructure credentials.

## What Worked and What Didn't

**High-profile targets that blocked the attack:** Sentry, OpenSearch, IPFS,
NixOS, Jina AI, recharts. Defenses that worked:
- First-time contributor approval gates
- Actor-restricted workflows
- Path-based trigger conditions

**What succeeded:** Small hobbyist projects with no contributor gates.
Mostly exposed ephemeral GitHub credentials. Two npm packages compromised
across 106 versions.

## Contrast with Axios

| Dimension | Axios | prt-scan |
|-----------|-------|----------|
| Sophistication | Nation-state, precision | Low-skill, AI-augmented |
| Targeting | Single high-value package | 500+ repos, spray-and-pray |
| Vector | Maintainer account takeover | PRs exploiting CI misconfiguration |
| Success rate | ~100% (until caught) | <10%, but dozens compromised |
| AI role | Not evident in attack | AI used to generate and scale attack |
| Anti-forensics | Sophisticated cleanup | Sloppy, inconsistent |
| Impact per hit | Catastrophic (100M+ weekly downloads) | Mostly limited (hobbyist projects, ephemeral creds) |
| Overall impact | Thousands of projects, hundreds of thousands of secrets | Dozens of projects, mostly ephemeral credentials |
| Attacker understanding | Deep (professional tradecraft) | Shallow (automation without understanding) |

These represent two ends of a spectrum:
- **Axios**: low volume, high precision, high impact per target
- **prt-scan**: high volume, low precision, low impact per target but
  compensated by scale

Both are supply chain attacks. Signatory must address both threat profiles.

## Signals That Would Have Been Relevant

| Signal | What it catches |
|--------|----------------|
| CI/CD configuration hygiene | Repos using `pull_request_target` on untrusted PRs without restrictions |
| First-time contributor gates | Whether a repo requires approval for first-time contributor PRs |
| PR source provenance | PR from a recently forked repo by a new/low-history account |
| PR velocity anomaly | 475 PRs in 26 hours from linked accounts |
| PR metadata uniformity | Identical title/body across hundreds of PRs — trivially detectable |
| Contributor account signals | 6 accounts with Proton Mail aliases from same base address, created in clusters |
| Branch naming patterns | `prt-scan-{hex}` prefix consistent across all waves |
| Code review status | PRs that trigger CI without human review |
| Fork relationship analysis | Malicious PRs come from forks — fork provenance is a signal |
| Language mismatch | `build.rs` injected into a Python SDK repo — wrong language for the target |
| AI hallucination artifacts | `pip.py` and other nonsensical file names generated by AI |

## Design Insights

### 1. AI-augmented attacks change the economics

The prt-scan attacker used AI to:
- Scan for vulnerable targets at scale
- Generate language-appropriate payloads matched to each repo's tech stack
- Create multi-phase payloads beyond their own understanding
- Sustain ~7 PRs/hour for 22+ hours

This means the barrier to entry for supply chain attacks has dropped
dramatically. A "low-sophistication" attacker can now launch 500 attacks
in a day. Even at <10% success, the absolute number of compromises is
significant.

**For signatory:** the threat model must assume high-volume, AI-generated
attacks are the baseline, not the exception. Signatory's signals need to
work at scale — not just for analyzing one package at a time, but for
detecting patterns across a dependency tree or an organization's exposure.

### 2. "Automation, not understanding"

Wiz's characterization is precise. The attacker built:
- Multi-language wrappers that adapt to each repo's tech stack
- Modular base64-encoded stages with unique nonces per target
- Five-phase payloads with EXFIL, RECON, DISPATCH, LABEL_BYPASS, DELAYED

But they failed to understand:
- DISPATCH requires write permissions the token doesn't have
- LABEL_BYPASS is circular and dead code
- Metadata probing only works on self-hosted runners
- Language targeting sometimes mismatches (Rust payload in Python repo)

This is a new signal category: **competent-looking code applied
illogically.** The AI generated plausible payloads, but the attacker
couldn't evaluate whether they'd work in context. Future AI-assisted
attacks will close this gap as models improve.

### 3. Basic CI/CD hygiene works — for those who have it

High-profile targets (Sentry, OpenSearch, NixOS) blocked the attack with
standard defenses:
- First-time contributor approval gates
- Actor-restricted workflows
- Path-based trigger conditions

These are binary, queryable signals. A repo either requires contributor
approval or it doesn't. Signatory can surface this immediately.

**The problem is the long tail.** Small projects without these gates were
the ones that got hit. This mirrors the Glasswing gap: major projects
have defenses, the long tail doesn't.

### 4. Consumer posture signals are as important as dependency signals

The `pull_request_target` misconfiguration is well-documented. Repos using
it without restrictions are voluntarily exposed. This is a signal about
the *consumer's own security posture*, not about their dependencies.

**For signatory:** the "consumer posture" signal group (version pinning,
lockfile discipline) should extend to CI/CD configuration hygiene. A
signatory survey should tell you not just "your dependencies have these
trust profiles" but also "your own repo configuration exposes you to
these attack vectors."

### 5. Disposable identity clusters are a detectable pattern

Six accounts, all Proton Mail, using `+` aliases from two base addresses.
Same branch naming convention. Same PR title and body. Same User-Agent
(`python-requests/2.32.5`). Consistent across all waves.

Each account individually might pass a cursory check. The *cluster* —
email alias patterns, coordinated timing, identical operational
signatures — is the signal. This is textbook counterintelligence
pattern detection.

**For signatory:** identity clustering analysis is a future capability
that would surface this kind of coordinated campaign. In the near term,
individual contributor signals (account age, activity history, email
provider patterns) provide partial coverage.

### 6. Volume attacks require different detection than precision attacks

Axios required deep signal analysis of a single, high-value target.
prt-scan required pattern detection across hundreds of targets. These
are complementary capabilities:

- **signatory analyze** (single target) — catches axios-style attacks
- **signatory survey** (dependency tree) — catches prt-scan-style exposure
- Future: cross-org correlation could detect coordinated campaigns

### 7. The attacker is learning

Each wave showed improvement:
- Wave 1: crude bash scripts, small targets
- Wave 2: structured five-phase payload, larger targets
- Wave 3: AI-generated, language-aware, repository-adaptive payloads

The gap between "AI-augmented amateur" and "AI-augmented professional"
is closing with each iteration. What's sloppy today will be polished
tomorrow.

## Lessons for Signatory

1. **CI/CD configuration hygiene is a v0.1-adjacent signal.** The
   `pull_request_target` misconfiguration is concrete and queryable.
   First-time contributor approval gates are a binary yes/no signal.
   Both could be surfaced with minimal implementation effort.

2. **PR-level analysis is validated as a future priority.** Both axios
   (via publication metadata) and prt-scan (via malicious PRs) show that
   drill-down to individual PRs and their provenance is essential.

3. **10% success on a sloppy attack is alarming.** A competent attacker
   using the same AI-assisted approach would have a significantly higher
   success rate. The window is closing.

4. **The two attack profiles are complementary threats.** Signatory's
   signal model must handle both precision attacks (axios) and volume
   attacks (prt-scan). The compositional signal framework supports this —
   the same signals serve both, but the detection patterns differ.

5. **The long tail is where the damage happens.** High-profile projects
   defended themselves. Small projects didn't. This is signatory's core
   audience — giving the long tail visibility into their own exposure.

6. **Credential theft cascades.** Stolen AWS keys, Cloudflare tokens,
   and NPM tokens from CI environments can enable further supply chain
   attacks. A prt-scan compromise of a small project's CI could lead to
   an axios-style attack on that project's npm packages. The two attack
   types can chain together.

7. **AI API keys as targets.** The DELAYED phase explicitly hunts for
   OpenAI, Anthropic, Gemini, DeepSeek, and Cohere keys. Stolen AI API
   credentials could be used to power the next wave of AI-assisted
   attacks — a self-reinforcing cycle.
