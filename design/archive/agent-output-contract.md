# Agent Output Contract — tokens for judgment, code for structure

**Status**: Active design principle. Established 2026-04-15 after
the first full pipeline dogfood run demonstrated that LLM agents
reliably produce structurally invalid JSON when asked to emit
exact-schema output.

## The principle

Separate the analysis task (what agents are good at) from the
serialization task (what deterministic code is good at).

- **Agents produce**: structured natural language — conclusions with
  verdict, severity, rationale, and citations in a defined text
  format. This is their strongest modality.
- **Deterministic code produces**: schema-valid JSON, database rows,
  protocol-compliant envelopes. This is what compilers and parsers
  are for.

Never ask an LLM to produce exact-schema JSON when a binary can
construct it from the agent's structured text output. Every
mechanical task that can move from token-spending to deterministic
code should move. Tokens are for judgment; code is for structure.

## Why

The dogfood run proved this empirically:

1. Two analyst agents were asked to produce v1-schema JSON files.
2. Both produced *approximately* correct JSON with structural
   violations (citations missing required `line_start` or `scope`
   fields).
3. Neither agent could run `signatory format-check` to self-validate
   (no Bash access in their tool environment).
4. The orchestrator's instinct was to hand-repair the JSON — which
   is the wrong response. If the pipeline produces invalid output,
   the pipeline design is wrong.

The root cause: we asked agents to do serialization (a mechanical
task) instead of analysis (a judgment task). The agent knew what it
wanted to say; it just couldn't say it in a structurally valid
envelope. That's a tooling problem, not an intelligence problem.

## Implementation path

### v0.1: structured text → CLI converter

Agents emit structured text (markdown with defined sections). A
`signatory build-output` CLI command converts the structured text
to v1-schema JSON. The binary handles all structural constraints
(required fields, citation format, enum validation, ID generation).

The agent's output format is a defined contract:

```markdown
## Conclusion: F001
Severity: medium
Category: variable_interpolation
Design-intent: false
Verdict: Variable interpolation resolves against live os.environ...
Rationale: The parser at line 47 of main.py calls os.environ.get...
Citation: src/dotenv/main.py:47-52 "os.environ.get(key, '')"
Citation: src/dotenv/main.py:89 "re.sub(pattern, replacement, value)"
```

The binary parses this into a `Conclusion` struct, validates
every field, constructs proper `Citation` objects with
`line_start`/`line_end`, and serializes to v1 JSON. If the agent
omits a required field, the binary rejects with a clear error —
not a silent structural violation in a JSON file.

### v0.2: MCP typed tool calls

```
signatory_add_conclusion(
  target: "pkg:pypi/python-dotenv",
  verdict: "...",
  severity: "medium",
  category: "variable_interpolation",
  rationale: "...",
  citations: [{path: "src/dotenv/main.py", line_start: 47, line_end: 52}]
)
```

Schema enforcement at the protocol level. The agent calls typed
tools; the server validates parameters before writing. Structurally
invalid data is impossible because the tool interface won't accept
it. This is the end state.

## What this means for skill/template design

- Handoff templates should describe **what to analyze and how to
  think**, not JSON schemas and field-level serialization rules.
- The agent's output format should be close to natural language
  with lightweight structure markers, not a programming-language
  data format.
- Pipeline orchestrators handle the mechanical steps (format
  conversion, validation, ingestion) — agents handle the judgment
  steps (what's important, what's the severity, what's the
  rationale).
