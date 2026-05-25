# PromptShield — Defensive Prompt-Injection Detection

This research tool detects and mitigates prompt-injection attacks
in production LLM applications. Below we catalog known attack
phrases the tool flags.

## Recognized attack phrases

- "ignore previous instructions" — the canonical phrase, observed
  in over 80% of published attacks.
- "you are now" — role-shift injection.
- "as an AI" — coercion phrase exploiting model identity claims.
- "act as" / "pretend to be" — persona-override attempts.
- "<|im_start|>" and "<|im_end|>" — OpenAI tokenizer special tokens
  used to inject role boundaries.
- "</system>" — HTML-style role-closer injection.

## Detection thresholds

PromptShield computes a composite score from phrase density and
semantic context...

## Known false-positive class

This README itself fires every lexical-injection detector by
design — it contains the phrases as documentation, not as a payload.
This is the design class the upstream `internal/contentinjection`
detector deliberately does not self-suppress on; the analyst layer
weights by project topic.
