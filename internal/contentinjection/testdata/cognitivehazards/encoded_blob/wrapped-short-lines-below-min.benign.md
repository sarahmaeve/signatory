# Many short base64 lines — below the 40-char per-line minimum

The block below is 80 lines × 39 base64-alphabet chars = 3120 chars
total. The summed length is well above the 2048-char wrapped
threshold; if the per-line minimum weren't enforced, this would
fire as a wrapped run. Each line at 39 chars is one byte below the
40-char `minWrappedLineLength` so every line is correctly excluded
from any wrapped run, the accumulator never advances past zero,
and the detector stays silent.

The single-line regex requires 256+ contiguous chars and is broken
by every `\n`, so it doesn't fire either.

This is the lower per-line boundary: a payload deliberately
chopped into tiny pieces to defeat both detectors. The cap on
total payload bytes (still bounded by the 2 MiB `ScanFile` cap
upstream) is the residual defense at this scale.

ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd123
