## `signatory pr-scan`

Deep-scans a GitHub PR's changed files for supply-chain attacks (prompt-injection, exfil hosts, agent-config/persistence writes, AST concerns) and records a `block`/`warn`/`clear` verdict. Refuses — before cloning — any PR whose author identity is burned. Findings pin to the head SHA and cache in the store; `BLOCK` exits non-zero (CI gate). Needs `GITHUB_TOKEN`.

```
# check (default verb) — scan one PR
signatory pr-scan Kong/kong#14838
signatory pr-scan https://github.com/Kong/kong/pull/14838 --json
signatory pr-scan owner/repo#42 --refresh            # bypass cache + burn gate (forensic)
signatory pr-scan owner/repo#42 --config pr-analyzer.yaml

# summary — list captures (burned authors float to top), or show one
signatory pr-scan summary
signatory pr-scan summary owner/repo#42

# report — write the pr-scan.js sidecar that enriches a pr-analyzer report
signatory pr-scan report owner/repo --out ./report
```

Re-running an unchanged PR replays the stored verdict; `--refresh` forces a re-scan.

### Connection to pr-analyzer (pinned module `v0.1.0`, decoupled — no shared data file)
- **`report`**: pr-analyzer *list mode* (`pr-analyzer owner/repo --out ./report`, needs `GITHUB_TOKEN`) writes a self-contained `index.html` that always carries `<script src="pr-scan.js">`. `pr-scan report` writes that `pr-scan.js` **from signatory's own store** (never reads `analyses.json`); the page loads it client-side (file://-safe) and folds deep findings into scanned PRs' drill-downs only. Either tool may run first; `index.html` is never modified.
- **`--config`**: `pr-scan check` parses the org's shared `pr-analyzer.yaml` via pr-analyzer's own `configfile.Load` (honors `codeshape.risky_paths` + language policy), so the parse can't drift.
- Also reuses pr-analyzer's `engineerprofile.ParseCodeowners` (CODEOWNERS read at the base tree).
