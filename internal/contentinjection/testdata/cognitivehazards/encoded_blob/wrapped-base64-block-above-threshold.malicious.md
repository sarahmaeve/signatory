# Wrapped base64 exfil — line-wrapped to evade the single-line detector

The block below is 40 lines × 64 base64-alphabet chars = 2560 chars
total, deliberately wrapped at the standard PGP-armored 64 chars/line
width. The single-line regex (`[A-Za-z0-9+/]{256,}`) sees 40 disjoint
64-char runs, none above the 1024-char threshold; the wrapped detector
sums them across the run and fires because total >= 2048.

This is the boundary the architecture review flagged: an attacker who
reads RFC 4880 wraps their payload to slip past the single-line check.

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
