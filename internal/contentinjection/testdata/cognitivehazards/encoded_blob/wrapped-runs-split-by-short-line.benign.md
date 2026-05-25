# Two wrapped sub-runs separated by a short base64 line

The content below is two 20-line × 64-char wrapped base64 sub-runs
with a 10-char base64 line between them. Each sub-run accumulates
1280 chars — comfortably below the 2048-char wrapped threshold —
and the short line correctly flushes the accumulator between them.
Total payload bytes (2570 chars of base64-alphabet content
overall) would fire if treated as a single run.

This pins the run-split logic: any line below the
`minWrappedLineLength` (or any line with characters outside the
alphabet) must break a wrapped run, not be silently elided.

ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcdEF
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd1234+/ABCDabcd
