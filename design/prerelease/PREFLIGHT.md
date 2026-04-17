# Preflight Checklist: Flipping Signatory Public

Checklist for the one-time transition from a private GitHub repo to a public
one. Applies when v0.1 is ready and the project is ready to be developed in
public for real.

See also:
- [`../ANTIPATTERNS.md`](../ANTIPATTERNS.md) — why fully-public development is
  load-bearing for signatory's own trust posture
- [`../trust-model.md`](../trust-model.md) §"Commit Activity Patterns" — the
  tenure and activity signals that visible history preserves

## What flipping preserves

Making a GitHub repo public is a visibility change, not a content change.
Preserved as-is:

- Full commit history, all branches, all tags
- Author and committer identities on every commit
- PRs, issues, releases, wiki content
- Stars and watchers (previously visible only to collaborators)
- Accumulated tenure — the strongest signal the flip makes visible

This is the point. Signatory's trust model rewards long history, continuous
activity, and identity consistency. Flipping public exposes all of that at
once, which is the desired outcome.

## What becomes permanent the instant you flip

History is public and indexed within minutes. Attackers run continuous
scrapers on the GitHub events firehose specifically harvesting secrets from
newly-public repos. Anything problematic in history after the flip requires
history rewriting, coordination with anyone who cloned, and loud
communication — significantly messier than scrubbing before.

Rule: **scrub first, flip second.** Never the other way around.

## Preflight checks

### 1. Full-history secret scan

Scan the entire object history, not just HEAD. Secrets deleted in a later
commit still exist in git objects and will be indexed.

```
gitleaks detect --source . --log-opts="--all"
trufflehog git file://. --since-commit=<first-commit>
```

Any hit, however old, must be scrubbed before the flip. Candidates:
committed `.env` files, API keys, tokens, SSH private keys, `.npmrc` or
`.pypirc` with auth tokens, database connection strings, signed-URL
parameters.

This is itself a nice dogfood exercise for signatory's threat model —
the adversary reading a newly-public repo is the canonical attacker signatory
exists to surface.

### 2. Author and committer email audit

```
git log --all --format='%ae%n%ce' | sort -u
```

Decide whether each address is one intended to be publicly associated with
the project. Personal emails committed with the intent of being private
cannot be reversed after the flip without rewriting history.

Options for addresses that shouldn't be public:
- Rewrite history with `git filter-repo --email-callback` before flipping
- Replace with GitHub `noreply` addresses
- Accept publication (for addresses that are fine in retrospect)

### 3. Commit message review

```
git log --all --format='%B'
```

Scan for content written under the assumption of privacy: client names,
internal URLs, references to people by name without permission, debugging
rants, credentials mentioned in prose.

### 4. Binary blob audit

```
git rev-list --objects --all \
  | git cat-file --batch-check='%(objecttype) %(objectname) %(objectsize) %(rest)' \
  | awk '$1=="blob" && $3 > 1048576' \
  | sort -k3 -n
```

Anything over ~1MB in history is worth eyeballing. Per
[`../threat-landscape/2026-04-14-openai-tac-gpt54-cyber.md`](../threat-landscape/2026-04-14-openai-tac-gpt54-cyber.md),
binary blobs are no-signal-to-negative by default and signatory should hold
itself to the same standard. Unexplained binaries in history are a signal
against signatory itself.

### 5. Issues and PRs

If the private repo has issues or PRs with internal discussion, they become
public too. Review comments for anything not intended for publication.

### 6. Branches

All branches become visible. Bursty experimentation is normal and should
stay — it is part of the honest development record. But specifically:

- Delete abandoned `spike/*` or `experiment/*` branches with half-finished
  thinking that would be misleading without context
- Delete any branch containing scrubbed material (even if scrubbed from
  main)
- Keep feature branches, in-progress work, and anything with real design
  reasoning

### 7. GitHub Actions logs

Actions secrets themselves stay encrypted and server-side. But any workflow
run log that printed a secret value is part of the public record.

Clear old Actions run logs if there is any doubt:
- Settings → Actions → clear run history, or
- `gh run list` + `gh run delete` for specific suspicious runs

### 8. `.gitignore` sanity check

This doesn't fix historical commits, but prevents future accidents:
- Local SQLite databases (`*.db`, `*.sqlite`)
- Analysis output directories that may contain PII
- `.env`, `.env.local`, any credential files
- IDE-specific local config that might contain paths or keys
- OS artifacts (`.DS_Store`, `Thumbs.db`)

## Scrub procedure (if any check fails)

Doing this work before the flip means the bad commits never exist on the
public side at all. Doing it after means everyone who cloned between the
flip and the cleanup has a copy of whatever was being removed.

1. Clone the repo fresh to a scratch directory.
2. Run scans against the fresh clone. This is faster than scanning the
   working copy and matches exactly what the public would see.
3. Use `git filter-repo` (preferred) or BFG Repo-Cleaner to rewrite
   history. `filter-branch` is deprecated and should be avoided.
4. Force-push the scrubbed history to the private remote.
5. Verify by fresh-cloning again and re-running scans.
6. Rotate any credentials that were ever exposed, even if only in history.
   Scrubbing removes the string from the repo; it does not un-leak the
   secret. Treat any secret that was ever committed as burned.
7. Only then flip public.

## Post-flip

Within minutes to hours of flipping:

- GitHub secret scanning runs across the full history. Any misses from the
  manual scan will surface as alerts.
- Dependabot and security alerts become active.
- archive.org, Software Heritage, and similar mirrors begin snapshotting.
- Automated scrapers (benign and hostile) begin indexing.

This is expected and fine — it is the baseline condition of running a
public OSS project, and the condition signatory's trust model assumes for
every other project it evaluates.

## The quick-answer version

If private development has been disciplined — no committed credentials,
intentional commit emails, reasonable commit messages — the flip is a
one-click operation that publishes a clean, credible history with
accumulated tenure intact. That history is signatory's strongest launch
artifact: a visible demonstration that the project passes the signals its
own framework values.

If there is any doubt about historical hygiene, scrub before flipping, not
after.
