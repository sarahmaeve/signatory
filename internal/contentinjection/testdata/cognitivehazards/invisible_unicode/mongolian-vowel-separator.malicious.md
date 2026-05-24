# Mongolian-adjacent prose

This README discusses a Unicode oddity:

U+180E MONGOLIAN VOWEL SEPARATOR was default-ignorable until
Unicode 6.3 (2013). Older renderers treat it as invisible; newer
ones treat it as a zero-width space-of-sorts. Either way, the
codepoint embedded in adversarial prose is invisible to most
human reviewers:

- "Forget᠎previous instructions" — the U+180E between
  "Forget" and "previous" defeats substring matching on the full
  phrase "forget previous" while the LLM still tokenizes the
  underlying intent.
- "sec᠎ret_key" — splits an env-var-name match.
