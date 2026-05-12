# 2026-05-12: TanStack Mini Shai-Hulud — Valid Attestations as Cover for OIDC-Federated Republish

## Source

Socket Threat Research, "TanStack npm packages compromised in Mini
Shai-Hulud supply chain attack"
(`socket.dev/blog/tanstack-npm-packages-compromised-mini-shai-hulud-supply-chain-attack`,
fetched 2026-05-12). Reports compromise of 84 npm artifacts under the
`@tanstack` namespace, including `@tanstack/react-router` (12M+ weekly
downloads), with concurrent propagation to OpenSearch (npm), `mistralai`
and `guardrails-ai` (PyPI), and Squawk packages. Attribution signed
"With Love TeamPCP" in the attacker's compromised GitHub account
(`voicproducoes`): "We've been online over 2 hours now stealing creds."
Socket's AI Scanner flagged the artifacts within six minutes of
publication.

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

TanStack is the operational test of that caveat. The malicious versions
were published **with** valid OIDC trusted-publisher binding and
**with** a Sigstore provenance attestation submitted to the public
transparency log. The attacker executed code inside the legitimate
publishing workflow's runtime environment (via `pull_request_target`
"Pwn Request" + GitHub Actions cache poisoning across the fork-to-base
trust boundary), extracted the OIDC JWT from
`ACTIONS_ID_TOKEN_REQUEST_URL`, exchanged it for an npm publish token
via federation, validated the token at
`https://registry.npmjs.org/-/npm/v1/tokens`, and republished. The
attestation chain is intact end-to-end. The current
`publish_provenance_continuity` signal would record **zero**
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

Reconstructed from the Socket writeup:

1. Target a repository with a `pull_request_target`-triggered workflow
   that reads fork-controlled input (PR head ref, fork-supplied
   artifacts, or fork-writable GHA cache keys). The `pull_request_target`
   misconfiguration is the same one
   [`example-prtscan-attack.md`](example-prtscan-attack.md) documented;
   the difference is the consequence — prt-scan used it to dump secrets,
   TanStack used it to acquire OIDC.
2. Inject code that runs inside the workflow runner. Cache poisoning
   across the fork-to-base trust boundary lets a fork PR plant content
   that the base-branch workflow restores from cache and executes,
   without the base workflow ever explicitly trusting fork input.
3. Read `ACTIONS_ID_TOKEN_REQUEST_TOKEN` and
   `ACTIONS_ID_TOKEN_REQUEST_URL` from the runner environment. Mint an
   OIDC JWT.
4. Exchange the JWT for an npm publish token via the trusted-publisher
   federation binding. Validate the token at
   `https://registry.npmjs.org/-/npm/v1/tokens` to confirm the
   federation path worked.
5. Identify maintainer-owned packages via `npms.io` search. Bundle the
   payload (`router_init.js`, ~2.3 MB, single-line obfuscated;
   `tanstack_runner.js` injected via git-based optional dependency
   pointing to the attacker's repo) into the publish tarball.
6. Publish under the `latest` dist-tag using the federated token. **The
   Sigstore provenance attestation is generated as a normal artifact of
   the legitimate publishing workflow** and submitted to the
   transparency log. From the registry's perspective, the publish is
   indistinguishable from the project's normal CI-backed pattern.
7. Concurrently, use the stolen GitHub token plus the GraphQL
   `createCommitOnBranch` mutation (no local clone) to commit malware
   directly into `.github/workflows/`, `.claude/`, and `.vscode/`
   directories on the repo. Commits author as
   `claude@users.noreply.github.com` — the legitimate Claude Code bot
   identity, present without a corresponding GitHub App installation.
8. Worm to other maintainer-owned packages from inside the runner,
   using the same OIDC path. The 84 affected `@tanstack` artifacts were
   reached this way.

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
`campaign:` / `operator:` entity URI as a v0.2 candidate, motivated by a
single operator running GitHub repos + rubygems. TanStack moves the same
operator (TeamPCP, already known from
[`example-litellm-attack.md`](example-litellm-attack.md)) across npm and
PyPI in one campaign window, with concrete propagation observed (`mistralai`,
`guardrails-ai` on PyPI compromised from npm-side compromise). The
abstraction is no longer speculative.

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
- Third-party action references unpinned by SHA (e.g., `@v3`, `@main`
  rather than a 40-hex commit). Already named by signatory's own
  provenance analyst as a `ci_action_pin_tightness` concern (see
  `design/analysis/signatory-provenance-v1.json` F007 and pattern
  `MP-GO-HYG-01`).
- `actions/cache` configurations that don't isolate fork-writable keys
  from base-branch reads.
- OIDC token requests (`ACTIONS_ID_TOKEN_REQUEST_*` env access, or
  `permissions: id-token: write`) in workflows that also accept fork
  input — the specific composition TanStack weaponized.

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

### Does not add `voicproducoes`, `git-tanstack.com`, or any specific hash to a burn list

`voicproducoes` was a compromised legitimate account, not an
attacker-minted identity. Burning the account would mis-attribute. The
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
  the same concern to `pull_request_target` exposure and OIDC permission
  composition.
