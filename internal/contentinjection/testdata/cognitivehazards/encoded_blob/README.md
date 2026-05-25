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

Single-line shapes (the original threshold class):

- `long-base64-payload.malicious.md` — base64 run exceeding the
  1024-char single-line threshold. The CamoLeak scale.
- `long-hex-payload.malicious.md` — hex run exceeding the 512-char
  single-line threshold.
- `sha256-hash.benign.md` — 64-char SHA-256 hashes. Must not fire.
- `sha512-hash.benign.md` — 128-char SHA-512 hashes. Must not fire.
- `jwt-signature.benign.md` — JWT-shaped tokens (~340-char base64).
  Must not fire.
- `ed25519-signature.benign.md` — Ed25519 signatures (~88-char
  base64). Must not fire.

Wrapped shapes (the per-line boundary class). The wrapped detector
sums base-N chars across consecutive "pure alphabet" lines (each
at least `minWrappedLineLength = 40` chars after trimming); the
run fires when the summed total clears
`encodedBlobWrappedThreshold = 2048`. The four fixtures below pin
every relevant boundary of that logic:

- `wrapped-base64-block-above-threshold.malicious.md` — 40 lines ×
  64 base64 chars = 2560-char summed run. Above threshold, must
  fire. Pins the canonical wrapped-exfil shape (PGP-armored
  geometry, real payload size).
- `pgp-armored-signature.benign.md` — typical PGP detached
  signature shape: 4 base64 body lines × 64 chars wrapped in the
  standard `-----BEGIN/END-----` markers. Total run ~256 chars,
  below threshold. The false-positive case the 2048-char wrapped
  threshold was calibrated against.
- `wrapped-short-lines-below-min.benign.md` — 80 lines × 39
  chars = 3120 chars total payload (above threshold if it were
  one run), but each line one byte below the 40-char per-line
  minimum. Pins the per-line-min boundary: an attacker chopping
  the payload into 39-char lines does not get a wrapped finding.
- `wrapped-runs-split-by-short-line.benign.md` — two 20-line ×
  64-char sub-runs (each 1280 chars, sub-threshold) separated by
  a 10-char base64 line. The short line correctly flushes the
  accumulator between runs. Pins the run-split logic: a
  short-line interruption must break the wrapped run, not be
  silently elided.
