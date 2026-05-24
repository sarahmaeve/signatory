# encoded_blob corpus

Targets `PrimitiveEncodedBlob`: long base-N (base16 / base32 /
base64) encoded runs in prose.

## Threat model

CamoLeak's exfil payloads encoded credential data as long base64
strings rendered into a markdown response. The encoded blob is
opaque to a human reviewer (looks like noise) but trivially
decodable. The detector targets the length distribution that
distinguishes real exfil payloads from legitimate encoded artifacts
(hashes, signatures).

Detection thresholds:

- base64: 1024+ chars (skips JWT signatures ~340 chars, RSA-2048
  signatures ~344 chars, Ed25519 signatures ~88 chars)
- hex: 512+ chars (skips SHA-256 64-char hashes, SHA-512 128-char
  hashes)
- base32: 256+ chars (rarer in legitimate use, lower threshold OK)

False-negative tradeoff: a real exfil payload under the threshold
escapes detection. The thresholds are deliberately calibrated to
zero out the noise floor at the cost of missing small payloads —
the design doc's "false-negatives preferred over false-positives"
rule applied to this primitive.

## Fixture sources

- `long-base64-payload.malicious.md` — base64 run exceeding the
  1024-char threshold. The CamoLeak scale.
- `long-hex-payload.malicious.md` — hex run exceeding the 512-char
  threshold.
- `sha256-hash.benign.md` — 64-char SHA-256 hashes. Must not fire.
- `sha512-hash.benign.md` — 128-char SHA-512 hashes. Must not fire.
- `jwt-signature.benign.md` — JWT-shaped tokens (~340-char base64).
  Must not fire.
- `ed25519-signature.benign.md` — Ed25519 signatures (~88-char
  base64). Must not fire.
