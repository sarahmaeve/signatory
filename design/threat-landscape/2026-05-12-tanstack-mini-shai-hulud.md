# 2026-05-12: TanStack Mini Shai-Hulud — Valid Attestations as Cover for OIDC-Federated Republish

## Source

Two sources, both fetched 2026-05-12:

- Socket Threat Research, "TanStack npm packages compromised in Mini
  Shai-Hulud supply chain attack"
  (`socket.dev/blog/tanstack-npm-packages-compromised-mini-shai-hulud-supply-chain-attack`).
  Campaign-wide writeup covering 84 npm artifacts under the `@tanstack`
  namespace, including `@tanstack/react-router` (12M+ weekly downloads),
  with concurrent propagation to OpenSearch (npm), `mistralai` and
  `guardrails-ai` (PyPI), and Squawk packages. Attributes the campaign
  to TeamPCP via attacker-controlled GitHub account `voicproducoes`
  (which hosted a repository titled "A Mini Shai-Hulud has Appeared"
  signed "With Love TeamPCP": "We've been online over 2 hours now
  stealing creds"). Socket's AI Scanner flagged the artifacts within
  six minutes of publication.
- TanStack maintainers' own postmortem, "npm Supply Chain Compromise:
  A Postmortem" (`tanstack.com/blog/npm-supply-chain-compromise-postmortem`).
  TanStack-specific details on the exploited workflow, the OIDC
  extraction technique, the detection timeline, and post-incident
  hardening. Disambiguates Socket's campaign-wide observations from
  what was actually done against TanStack.

This entry pairs with the existing TeamPCP coverage in
[`example-litellm-attack.md`](example-litellm-attack.md) and
[`example-prtscan-attack.md`](example-prtscan-attack.md). The prior
TeamPCP entries documented stolen-credential and
`pull_request_target`-injection variants; this entry documents the
**OIDC-federated republish** variant, which preserves every trust
binding the prior variants broke.

## Why this entry exists

The attack exercises a path the v0.1 signal set models almost
correctly, and the "almost" is the point.

[`example-axios-attack.md`](example-axios-attack.md) named
*"Trusted Publishing (OIDC) is a concrete positive signal"* (lesson 13)
and *"Publication metadata divergence is a high-confidence signal"*
(lesson 10) — specifically, the axios malicious versions were published
**without** trusted-publisher binding, deviating from the project's
normal CI-backed publishing pattern. Signatory's npm collector wires
this directly: `internal/signal/registry/npm/collector.go` (around the
`publish_provenance_continuity` block, ~lines 600–636) counts
attestation-presence transitions across recent versions, "direction-
agnostic. A lost attestation is the axios shape." `internal/signal/types.go`
registers `build_provenance_attestation` with the caveat that
*"attestation alone is not trust — a verifier must check it against a
known-good build configuration."*

TanStack is the operational test of that caveat. The malicious
publishes were authenticated against the project's valid OIDC
trusted-publisher binding (`release.yml@refs/heads/main`). The attacker
executed code inside the legitimate publishing workflow's runtime
environment — via `pull_request_target` "Pwn Request" on
`bundle-size.yml` plus GitHub Actions cache poisoning across the
fork-to-base trust boundary — and scraped the OIDC token that the
legitimate workflow had already minted, by reading the
`Runner.Worker` process's memory directly (`/proc/<pid>/maps` +
`/proc/<pid>/mem`). The malicious code never made an
`ACTIONS_ID_TOKEN_REQUEST_URL` request of its own; it lifted the
token the runner had already obtained for the legitimate publish step.
It then posted directly to `registry.npmjs.org` with the scraped
token, bypassing the workflow's own Publish Packages step entirely.
From the registry's perspective the binding was satisfied. The
current `publish_provenance_continuity` signal would record **zero**
attestation transitions across these versions.

The existing v0.1 instrumentation answers *"did this package keep its
attestation?"* The TanStack profile demands *"what does this attestation
claim about the build environment, and is that claim consistent with the
project's prior publishes?"* The fields needed to answer the second
question (workflow ref, repository, environment) already arrive in the
provenance payload — the PyPI fuzz testdata at
`internal/signal/registry/pypi/fuzz_test.go` (around line 124)
demonstrates the `publisher.{kind,repository,workflow,environment}`
shape — but the npm collector surfaces only `latest_has_attestation`
(boolean) and `attestation_transitions` (count). The workflow ref the
attestation binds to is never written out as a signal value, so no
downstream conclusion can name a discrepancy in it.

## The attack shape

Reconstructed from the Socket writeup and the TanStack postmortem
(facts attributed to one source or the other where they disagree or
where only one source has the detail):

1. Target a repository with a `pull_request_target`-triggered workflow
   that reads fork-controlled input. TanStack's exploited workflow was
   `bundle-size.yml`, triggered on
   `pull_request_target.paths: ['packages/**', 'benchmarks/**']` and
   checking out `ref: refs/pull/<pr-number>/merge` — fork code, run
   with base-repo context. The `pull_request_target` misconfiguration
   is the same one
   [`example-prtscan-attack.md`](example-prtscan-attack.md)
   documented; the difference is the consequence — prt-scan used it
   to dump secrets, TanStack used it to seed a poisoned cache.
2. Plant content into a GHA cache key that the base-branch publishing
   workflow will later restore. TanStack's poisoned key was
   `Linux-pnpm-store-<hash>` keyed on `hashFiles('**/pnpm-lock.yaml')`,
   restored by `release.yml`'s Setup Tools step on the next push to
   main. The trust-boundary leak is mechanical and named:
   `actions/cache@v5`'s post-job save uses a runner-internal token,
   not `GITHUB_TOKEN`, so PR runs write to base-repo cache scope
   without needing elevated permissions. The cache-poisoning step
   does not require the fork PR to do anything that looks like
   privileged access from the workflow's perspective.
3. Wait for the legitimate `release.yml` to run on main. When it
   restores the poisoned pnpm store, attacker binaries execute during
   the build step. They locate the GitHub Actions `Runner.Worker`
   process via `/proc/*/cmdline`, then read `/proc/<pid>/maps` and
   `/proc/<pid>/mem` to scrape the OIDC token the runner already
   minted for the legitimate publish step. The malicious code makes
   **no** `ACTIONS_ID_TOKEN_REQUEST_URL` request of its own — the
   token is lifted from another process's memory. The `/proc`-based
   memory-scrape primitive is verbatim from the March 2025
   `tj-actions/changed-files` compromise (CVE-2025-30066); see
   [`2025-03-14-tj-actions-changed-files.md`](2025-03-14-tj-actions-changed-files.md)
   for the primary-source payload and the
   `"…","isSecret":true` regex the script greps against the dump.
   The **use** differs: tj-actions printed scraped secrets to
   workflow stdout (a passive log-exfil model that worked only
   against public repos with readable logs); TanStack used the
   scraped OIDC token actively to publish, which works against
   private repos and needs no attacker-side log-scraping pipeline.
   Primitive: 14 months old. Use: new this incident.
4. Post directly to `registry.npmjs.org` with the scraped token,
   bypassing the workflow's own Publish Packages step entirely. The
   publish is authenticated against the project's
   `release.yml@refs/heads/main` trusted-publisher binding; the
   registry has no view that the binding was satisfied by an
   out-of-band write rather than the workflow's defined publish step.
   None of the malicious publishes used a maintainer's npm token (per
   the postmortem).
5. Identify maintainer-owned packages via `npms.io` search. Bundle the
   payload into each publish tarball: a ~2.3 MB obfuscated
   `router_init.js`, plus an injected
   `optionalDependencies: "@tanstack/setup": "github:tanstack/router#<sha>"`
   pointing at the attacker's payload commit on a fork
   (`zblgg/configuration`, commit
   `65bf499d16a5e8d25ba95d69ec9790a6dd4a1f14`).
6. Publish under the `latest` dist-tag. **The Sigstore provenance
   attestation is generated as a normal artifact of the legitimate
   publishing workflow** and submitted to the transparency log. From
   the registry's perspective, the publish is indistinguishable from
   the project's normal CI-backed pattern.
7. Repository-state poisoning, observed in the broader Mini-Shai-Hulud
   campaign **though not in the TanStack compromise specifically**: a
   stolen GitHub token plus the GraphQL `createCommitOnBranch`
   mutation (no local clone) is used to commit malware directly into
   `.github/workflows/`, `.claude/`, and `.vscode/` directories on
   compromised repos, authored as `claude@users.noreply.github.com` —
   the legitimate Claude Code bot identity, without a corresponding
   GitHub App installation. The TanStack-specific fork PR
   (`zblgg/configuration`) was pushed via plain `git push`, not via
   the GraphQL path.
8. Worm to other maintainer-owned packages from inside the runner,
   using the same OIDC-from-memory path. The 84 affected `@tanstack`
   artifacts were reached this way.

The payload's runtime behavior is a refinement of patterns
[`example-litellm-attack.md`](example-litellm-attack.md) and
[`2026-05-02-bufferzonecorp-campaign.md`](2026-05-02-bufferzonecorp-campaign.md)
already document:

- Reentrancy guard via `process.env.__DAEMONIZED`; spawn detached child
  with `stdio: ['ignore','ignore','ignore']` and `.unref()`.
- Credential sweep across `GITHUB_REPOSITORY`,
  `ACTIONS_ID_TOKEN_REQUEST_*`, `AWS_ACCESS_KEY_ID`,
  `AWS_SECRET_ACCESS_KEY`, `AWS_ROLE_ARN`,
  `AWS_WEB_IDENTITY_TOKEN_FILE`, `VAULT_TOKEN`, `VAULT_ADDR`, AWS IMDSv2
  (`169.254.169.254/latest/api/token`), ECS metadata (`169.254.170.2`),
  in-cluster Vault (`vault.svc.cluster.local:8200`), and the K8s
  service-account token / CA cert at
  `/var/run/secrets/kubernetes.io/serviceaccount/`. The
  [bufferzonecorp entry](2026-05-02-bufferzonecorp-campaign.md) already
  extended the credential-target list with IMDS, GCP/Azure metadata, and
  K8s SA tokens; TanStack confirms the same target set with Vault added.
- Full enumeration of GitHub Actions secrets via paginated
  `GET https://api.github.com/repos/<GITHUB_REPOSITORY>/actions/secrets?per_page=100`.
- Exfiltration via `http://filev2.getsession.org/file/`, routed through
  Session protocol service nodes (`snode`). At the network layer the
  traffic looks like the legitimate Session messenger, not a
  request-capture host.
- Developer-workstation persistence: writes `.claude/router_runtime.js`,
  `.claude/settings.json`, `.claude/setup.mjs`, `.vscode/setup.mjs`,
  `.vscode/tasks.json` — survives `npm uninstall` and re-executes on
  agent or editor invocation.

IOCs recorded for reference, not as a burn list:

| File | SHA256 |
|------|--------|
| `router_init.js` / `router_runtime.js` | `ab4fcadaec49c03278063dd269ea5eef82d24f2124a8e15d7b90f2fa8601266c` |
| `tanstack_runner.js` | `2ec78d556d696e208927cc503d48e4b5eb56b31abc2870c2ed2e98d6be27fc96` |

Per the discipline established in
[`2026-04-21-vercel-contextai-incident.md`](2026-04-21-vercel-contextai-incident.md)
§"Does not ingest Vercel or Context.ai post-incident claims as signals,"
these are recorded as threat-landscape inputs. Signatory does not
maintain per-incident hash blocklists.

## What this validates in our existing model

The cross-ecosystem operator pattern is now a third independent data
point. The
[bufferzonecorp entry](2026-05-02-bufferzonecorp-campaign.md) named the
`campaign:` / `operator:` entity URI as a v0.2 candidate, motivated by
a single operator running GitHub repos + rubygems. Socket's
campaign-wide writeup attributes concurrent npm + PyPI compromises
(`mistralai`, `guardrails-ai` on PyPI) to the same operator (TeamPCP,
already known from
[`example-litellm-attack.md`](example-litellm-attack.md)) in the same
window. The TanStack maintainers' postmortem does not address the
cross-ecosystem path directly — TanStack itself is npm-only — so the
propagation claim rests on Socket's campaign attribution rather than
on direct maintainer observation. The abstraction is no longer
speculative, but the cross-ecosystem evidence is one-sided.

The publication-integrity signal group remains correctly named — the
issue is depth, not location. Attestation **presence** is a positive
signal as documented; the new fact is that it is no longer a sufficient
positive on its own.

The developer-workstation persistence locus echoes a shape
[bufferzonecorp](2026-05-02-bufferzonecorp-campaign.md) §"Credential-
target list is incomplete" already opened (`~/.ssh/authorized_keys`
write, post-commit git hook). Agent and IDE config directories
(`.claude/`, `.vscode/`, by extension `.cursor/`, `.zed/`, `.aider*/`)
are a sibling persistence locus: they execute on tool invocation rather
than on shell-spawn or git-action, and a malicious `.claude/settings.json`
runs inside future agent sessions on the developer's machine.

## What this exposes as a gap

### Attestation content is not surfaced, only attestation presence

The npm collector at
`internal/signal/registry/npm/collector.go` records
`latest_has_attestation` and `attestation_transitions`. The provenance
payload that backs those booleans carries `subject`, `predicate`, and
specifically the `buildDefinition.externalParameters.workflow` (or the
PyPI-side `publisher.{repository,workflow,environment}`) — the
attestation's claim about *what built this artifact*. Today none of that
detail leaves the collector as a signal value.

The TanStack-shaped detection is a per-version comparison: does the
attesting workflow ref match the ref that produced the *prior* attested
versions of the same package? A change in the attesting workflow
identity on an established package is the signal, and it is recoverable
from data already collected. The work is in the collector (write
additional signal fields) and in the analyst pattern (compare across
versions), not in any new collection path.

This argues for two concrete additions:

- A new signal value alongside `latest_has_attestation`:
  `latest_attestation_builder` (or `..._workflow_ref`) capturing the
  workflow ref the attestation binds to. Same shape on npm and PyPI;
  PyPI's `publisher.workflow` field is already extractable by the
  existing fuzz target.
- Extend `publish_provenance_continuity` to count workflow-ref
  transitions, parallel to its existing presence-transition count. A
  workflow-ref change without a corresponding maintainer-announced CI
  reshape is the high-signal anomaly.

### Workflow posture is not collected at all

The `pull_request_target` misconfiguration is the same one
[prt-scan](example-prtscan-attack.md) documented, but only as a
*consumer-side* signal ("does this consumer's own repo expose
itself?"). TanStack uses it as a *publisher-side* signal: the integrity
of any attestation produced by a workflow exposed to fork-controlled
input is contingent on the workflow's posture, not on the attestation's
cryptographic validity.

The collection surface is the publisher repo's `.github/workflows/`
tree. Features worth surfacing as conclusion-producing observations:

- Workflows triggered by `pull_request_target` that read fork-controlled
  input (PR head ref, fork-supplied env, fork-writable cache keys, or
  artifact restore from fork builds).
- Action references unpinned by SHA — *including own-organization
  composite actions and infrastructure repos*, not just third-party
  ones. TanStack's exploited workflow pulled in
  `TanStack/config/.github/setup@main`, their own infrastructure
  repo's composite action at a floating ref; that ref transitively
  resolved to `actions/cache@v5` and was part of the trust-boundary
  leak. The "pin by SHA" rule covers more surface than the
  `ci_action_pin_tightness` framing suggests: it applies to every
  action ref a publishing workflow consumes, including the org's own
  infra. (See `design/analysis/signatory-provenance-v1.json` F007 and
  pattern `MP-GO-HYG-01` for the existing concern shape.)
- `actions/cache` configurations that don't isolate fork-writable keys
  from base-branch reads. The mechanical property to surface: cache
  post-job save uses a runner-internal token rather than
  `GITHUB_TOKEN`, so a fork PR running the same job can write a cache
  entry the base branch will later restore. Any workflow that both
  (a) is reachable from `pull_request_target` and (b) writes cache
  with a key the publishing workflow restores satisfies the
  TanStack-shaped trust-boundary leak.
- OIDC token requests (`ACTIONS_ID_TOKEN_REQUEST_*` env access, or
  `permissions: id-token: write`) in workflows that also accept fork
  input — necessary but **not sufficient** as a workflow-posture
  signal. The TanStack exploit lifted the OIDC token from the
  `Runner.Worker` process's memory, not from the env-var endpoint, so
  even a job without `id-token: write` in its own permissions is
  exposed if it runs on the same runner as a job that has it. The
  posture signal should treat *any* job with `id-token: write` whose
  runner is reachable from fork-influenced code as exposed, not just
  the job that requests the token.

These are publisher-side conclusions: they describe the publisher
repo's posture, parallel to but distinct from the consumer-side
posture surface
[prt-scan §"Consumer posture signals are as important as dependency signals"](example-prtscan-attack.md)
named.

### Sigstore transparency-log content is not consulted

A separate angle on the same data: the provenance attestation is
published to a public transparency log. The attestation's
`builderId`/`workflow` fields are queryable from the log entry without
needing to scrape the publisher repo. For burn or staleness re-checks,
the log is a cheap GET-only source compatible with the architectural
constraint that signatory collectors do not POST. Worth recording as a
collection surface, separate from the registry-side `dist.attestations`
block.

### Bot-identity impersonation is not a recognized conclusion shape

The GraphQL `createCommitOnBranch` mutation authored commits as
`claude@users.noreply.github.com`. That email is the legitimate Claude
Code GitHub App's commit identity. The detection signal is the
combination — commits claim a known bot identity, but no corresponding
GitHub App installation is active on the repo, or the commit arrived
via the GraphQL API rather than the bot's normal API path, or the
signed-status doesn't match the bot's normal signing posture.

[bufferzonecorp](2026-05-02-bufferzonecorp-campaign.md) §"Module-name
typosquat (not just publisher-name typosquat)" generalized typosquat
detection from publisher names to module names. Bot-identity
impersonation is the same generalization applied to commit author
identity: known-bot-identity-without-corresponding-installation is the
analog of generic-noun-module-name-without-canonical-namespace. Worth a
sibling pattern under identity-graph; cheap to compute against a
small allowlist of known bot identity strings.

### Host-class corpus needs a P2P-messaging-network entry

`filev2.getsession.org` is not a request-capture-as-a-service host in
the `webhook.site` sense. It is a service node in the Session
decentralized messenger, and exfil traffic to it is encrypted under the
Session protocol. The
[bufferzonecorp host-class typology](2026-05-02-bufferzonecorp-campaign.md)
§"C2-destination-class as a corpus signal" is the right home; this is a
new class entry (P2P-messaging-network / decentralized-relay) sibling
to request-capture-as-a-service. The same architectural discipline
applies — typology of host classes with bounded membership, not a
vendor IOC list.

## What this depends on for detection

The 20-minute detection window between the first malicious publish
(2026-05-12 19:20:39 UTC) and the external researcher's GitHub issue
(~19:50 UTC, ashishkurmi at StepSecurity, issue #7383) is the headline
number, and the Socket AI Scanner's six-minute artifact flag is the
collection-side number. Both are encouraging. Both depended on the
attacker being loud in ways they did not have to be. The TanStack
maintainers' postmortem is explicit that detection was 100% external:
there was no internal publish-monitoring, no rate-limit alert, no CI
failure tracking tied to publish events. The 20-minute number is what
the open-source community caught, not what TanStack caught.

What got detection to 20 minutes:

- The payload broke TanStack's test suite. The legitimate publishing
  workflow's publish step skipped on test failure — but the attacker
  had already exfiltrated the OIDC token and republished out-of-band
  (step 4 above), so the broken tests didn't prevent the compromise.
  They just made it loud: failing CI on `main`, unexpected publishes,
  community pings.
- The version cadence was extreme: 84 versions across the namespace in
  under two hours. Anomalous against any prior baseline.
- The injected `optionalDependencies: "@tanstack/setup":
  "github:tanstack/router#<sha>"` is grep-visible in the published
  `package.json` — an IOC that doesn't require runtime analysis.

A more careful attacker could have removed all three:

- Keep tests green. The publish step would have completed normally;
  the out-of-band publish would not have been visible against the
  workflow's own publish output.
- Slow the cadence. Publish one malicious version per package every
  few hours. Stays under any naive rate-anomaly threshold.
- Inject runtime-only payload via a less grep-visible vector than
  `optionalDependencies` (a postinstall script, a fetched binary, a
  conditional code path triggered by build-environment fingerprint).

Signatory's lesson here is not to downgrade the detection-cadence
framing in
[`example-axios-attack.md`](example-axios-attack.md) lesson 16, but to
be explicit about its dependencies. Minutes-to-hours detection is
achievable when *publish-cadence anomaly + CI failure signal +
community ping* all fire together. The composite remains strong; any
single component alone is much weaker. The
attestation-builder-identity signal this entry proposes is exactly the
kind of component that holds up even when the attacker is quiet —
workflow-ref divergence in the attestation is observable regardless of
test state, cadence, or payload shape.

## Empirical: what the current signal model says at T+~21h

A `signatory analyze pkg:npm/@tanstack/react-router --refresh --clone`
run on 2026-05-12 at 15:55 UTC, approximately 21 hours after the
first malicious publish at 2026-05-11T19:20:39 UTC. The full
38-signal cross-section is preserved at
[`raw-data/2026-05-12-tanstack-react-router-signals.json`](raw-data/2026-05-12-tanstack-react-router-signals.json);
the Socket campaign package list (417 rows) is at
[`raw-data/2026-05-12-mini-shai-hulud-socket-packages.csv`](raw-data/2026-05-12-mini-shai-hulud-socket-packages.csv).

### Registry state at run-time

`latest = 1.169.2` (published 2026-05-05T20:37:38Z) — the
**pre-compromise** version. The malicious 1.169.5 and 1.169.8 (per
the CSV, both published 2026-05-11T19:20+ UTC) had been pulled
server-side by npm Security in the intervening day. signatory queried
a post-incident *cleaned* registry state.

### The verdict the current signal model produced

38 signals across 8 collectors (npm-registry, github, git, repofiles,
openssf-scorecard, exfilwatch, artifact-vs-repo). **Zero indicated
compromise.** Headline values:

- `publish_origin_consistency = very-high`, `attestation_transitions = 0`
- `trusted_publishing = very-high`, `present = true`
- `version_publish_burst.burst_detected = false` — the 84-version
  flash on 2026-05-11T19:20–19:26 is invisible because npm Security
  removed those versions from the registry; the burst signal is
  structurally backward-looking against the registry's current state
- `exfilwatch.exfil_capture_host = []` — bufferzonecorp's
  host-class-corpus signal is wired up and returns no matches against
  the post-cleanup tarball
- `artifact-vs-repo.artifact_repo_divergence = absent`, reason
  *"tarball-to-commit pairing unresolved"* — the collector ran, the
  data path isn't complete (the npm-registry `git_head` field is
  empty in the artifact_url signal, and the Sigstore-log fallback
  isn't being chased)

### What this confirms; what this exposes

Confirms the entry's central argument: attestation presence and
zero-transition consistency are not sufficient as a terminal
positive. The signal model labels a package that was actively
serving malware ~21 hours earlier as **very-high
publish_origin_consistency**, because the post-incident registry
state is structurally pristine.

Exposes three observations the entry did not anticipate:

- A cadence-gap signal between repo activity and publish activity.
  `last_commit = 2026-05-12T13:32` (today, post-incident hardening
  work) vs `last_publish = 2026-05-05T20:37:38Z` (six days ago,
  pre-compromise) is the post-incident-investigation fingerprint
  — distinct from during-incident burst detection. Not currently
  composed into a derived signal.
- `github.commit_signing` (ratio 0.9, 10 commits sampled,
  web-flow-included) and `git.per_developer_commit_signing_ratio`
  (ratio 0, 1000 commits, web-flow-excluded) produce contradictory
  answers on the same nominal axis. Already named as a
  methodological issue in
  [`../analysis/signatory-provenance-v1.json`](../analysis/signatory-provenance-v1.json)
  F002 when signatory analyzed itself; confirmed general on an
  external target here.
- TanStack's 2950 tags include zero signed (2943 annotated unsigned,
  7 lightweight). Per
  [`2025-03-14-tj-actions-changed-files.md`](2025-03-14-tj-actions-changed-files.md)
  §"GHA tag mutability is a structural platform property," any tag
  rewrite would be undetectable from tag metadata alone. This
  package is structurally exposed on the tag→SHA-anomaly axis the
  tj-actions entry promotes from "needed" to "confirmed class."

The raw JSON carries the full 38-signal cross-section for any future
analysis that wants to audit a specific signal value rather than rely
on this summary.

## What this does *not* do

### Does not weaken the trusted-publishing positive signal

`build_provenance_attestation` and the OIDC-trusted-publishing positive
signal remain correctly named in `internal/signal/types.go`. The
TanStack profile does not invert the signal; it sharpens its required
verification depth. The signal's own description already says
*"attestation alone is not trust — a verifier must check it against a
known-good build configuration."* TanStack documents the specific
configuration property that needs to be checked (attesting workflow
ref consistency across versions). Trusted publishing is still
materially better than personal-token publishing; it is no longer a
terminal positive on its own.

### Does not add `voicproducoes`, `zblgg`, `git-tanstack.com`, or any specific hash to a burn list

`voicproducoes` (campaign-wide attacker infrastructure per Socket) and
`zblgg/configuration` (the TanStack-specific malicious fork per the
postmortem) are facts of this incident, not durable signals. The
domain `git-tanstack.com` and the file hashes recorded above are
threat-landscape inputs, not signal-table contents.
[bufferzonecorp §"Does not add `webhook.site` to a burn list"](2026-05-02-bufferzonecorp-campaign.md)
is the precedent; this entry follows the same discipline.

### Does not promote any new collector ahead of design

The PyPI signal collector already extracts `publisher.workflow` from
PEP 740 attestations as fuzz input. Surfacing that as a queryable
signal value is a within-collector extension, not a new collector. The
npm collector parallel is the same shape. A workflow-posture collector
that reads `.github/workflows/` from publisher repos is a new surface,
but the immediate move is to surface what we already collect — the
attestation's claim about its builder — not to add a new collection
path.

### Does not retroactively change the axios analysis

[`example-axios-attack.md`](example-axios-attack.md) named
trusted-publishing-binding **absence** as the signal of interest for
that incident. That call was correct for that incident. TanStack adds a
sibling signal (attesting-workflow-ref **change** in the presence of
trusted publishing), not a replacement.

## Open questions

- Should attestation-content signals (workflow ref, repository,
  environment) be three separate signal-type registrations or one
  composite `attestation_builder_identity`? The npm and PyPI shapes
  differ in field names but carry the same semantic; a single
  cross-ecosystem signal type with ecosystem-specific extraction is
  probably right, but the trade-off against ecosystem-specific
  signal-type granularity is worth recording.
- Where does publisher-side workflow-posture analysis belong in the
  signal-group taxonomy — under publication integrity (it scopes a
  publication act) or under a new "publisher repo posture" group
  parallel to the consumer-side posture surface prt-scan implies? Both
  framings are defensible; the answer affects whether one analyst or two
  consume the data.
- What is the right cadence for re-collecting a package's
  attestation-builder identity? The TanStack window was hours; the
  Socket AI Scanner caught it within six minutes of publish. Signatory's
  current collection model is on-demand-per-analysis, not continuous.
  Continuous re-collection for high-criticality packages is implied by
  [`example-axios-attack.md`](example-axios-attack.md) lesson 16 ("3
  hours is enough") and now reinforced by the six-minute Socket detection
  window. This is an analysis-economics question, not a signal-design
  question.
- Sigstore transparency log as a collection surface: GET-only, public,
  compatible with the WebFetch architectural constraint. Worth its own
  short design note, or fold it into the broader provenance-signal
  evolution?

## Cross-references

- [`example-axios-attack.md`](example-axios-attack.md) — establishes
  trusted-publishing absence as the signal of interest; this entry adds
  the sibling case of trusted-publishing-with-builder-change.
- [`example-litellm-attack.md`](example-litellm-attack.md) — prior
  TeamPCP coverage, stolen-credential variant; TanStack is the
  OIDC-federated-republish variant by the same operator.
- [`example-prtscan-attack.md`](example-prtscan-attack.md) — established
  `pull_request_target` as a consumer-side posture concern; TanStack
  promotes it to a publisher-side concern when paired with OIDC.
- [`2026-05-02-bufferzonecorp-campaign.md`](2026-05-02-bufferzonecorp-campaign.md)
  — campaign-entry template followed by this entry; credential-target
  list, host-class corpus discipline, cross-ecosystem operator URI.
- [`2026-04-21-vercel-contextai-incident.md`](2026-04-21-vercel-contextai-incident.md)
  — "named external incident motivates a new signal axis" template; the
  identity-surface-exposure axis it opened is adjacent to the workflow-
  posture axis this entry opens.
- [`../trust-model.md`](../trust-model.md) §"Signals must be weighted by
  forgery resistance" — "Publication metadata / trusted publishing"
  sits at High in the forgery-resistance table (one tier below
  "Cryptographic signatures (GPG/SSH/OIDC)" at Very High);
  attestation-builder-identity-consistency sits at the same High tier,
  derived from the same publication-metadata layer.
- `internal/signal/types.go` (`build_provenance_attestation`,
  `publish_provenance_continuity`, `attestation_consistency`) — the
  existing signal definitions this entry proposes to extend.
- `internal/signal/registry/npm/collector.go` (the
  `publish_provenance_continuity` block) — site of the proposed
  workflow-ref-transition addition.
- `internal/signal/registry/pypi/fuzz_test.go` — confirms the
  `publisher.{kind,repository,workflow,environment}` shape is already
  in the PyPI attestation-response unmarshaller's reach.
- [`../analysis/signatory-provenance-v1.json`](../analysis/signatory-provenance-v1.json)
  F007 / `MP-GO-HYG-01` — `ci_action_pin_tightness` is already named as
  a signal; the workflow-posture surface this entry proposes generalizes
  the same concern to `pull_request_target` exposure, cache-key trust
  boundaries, and OIDC permission composition.
- [`2025-03-14-tj-actions-changed-files.md`](2025-03-14-tj-actions-changed-files.md)
  — primary-source entry for the runner-memory-scrape primitive
  (CVE-2025-30066). Primitive reused verbatim by TanStack 14 months
  later; use evolved from passive log exfil to active OIDC-token
  republish. The reuse itself is a signal: workflow-shape signatures
  derived from public prior-compromise writeups remain useful
  detection inputs across capability eras.
