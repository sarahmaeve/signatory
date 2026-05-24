# tag_block corpus

Targets `PrimitiveTagBlock`: the Unicode tag block U+E0000–U+E007F.

## Threat model

The tag block mirrors printable ASCII (the standard maps ASCII X to
U+E0000 + X, so "A" becomes U+E0041, "Z" becomes U+E005A, space
becomes U+E0020). Tag characters render as invisible in virtually
every editor, but most LLM tokenizers process them as ordinary
characters. The result is a near-invisible side channel — text
that humans cannot see but the LLM ingests as instructions.

Riley Goodside's original 2024 PoC demonstrated triggering a
DALL-E image generation request by pasting tag-encoded instructions
that appeared as ordinary visible prose. Wunderwuzzi documented
the encoding at
[embracethered.com/blog/posts/2024/hiding-and-finding-text-with-unicode-tags/](https://embracethered.com/blog/posts/2024/hiding-and-finding-text-with-unicode-tags/).

The legitimate use space is approximately zero — the tag block has
no defensible purpose in repository content.

## Fixture sources

- `riley-goodside-style-encoded.malicious.md` — embeds the canonical
  demonstration phrase ("Welcome to the Matrix!" or similar) as
  tag-block characters after innocuous visible prose. Models the
  PoC shape Wunderwuzzi documents.
- `mixed-ascii-tags.malicious.md` — visible prose with embedded
  tag-block characters that spell out a directive the LLM would
  tokenize.
- `no-tags.benign.md` — pure ASCII control, must not fire.
- `plain-emoji-and-cjk.benign.md` — multi-byte UTF-8 from the
  ordinary ranges only; no tag-block codepoints. Must not fire.
