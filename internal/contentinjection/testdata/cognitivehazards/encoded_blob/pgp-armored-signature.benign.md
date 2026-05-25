# Real PGP-armored signature — wrapped at 64, well below threshold

The block below is a representative PGP detached-signature shape:
4 base64 body lines × 64 chars/line ≈ 256 chars total, plus the
5-char `=DGmh` CRC checksum line and the `-----BEGIN/END-----`
markers. The wrapped detector must not fire — typical PGP
signatures (RSA-2048 ~344 chars, RSA-4096 ~688 chars) all sit
below the 2048-char wrapped threshold by design, and the marker /
checksum lines correctly break the wrapped-line run because they
contain characters outside the base64 alphabet.

This is the false-positive case the wrapped threshold was
calibrated against: legitimate cryptographic-signature blocks
should never fire.

-----BEGIN PGP SIGNATURE-----

iQFGBAEBCAAwFiEEA0FzmoO2vGdcGdYSrSlqyB5xnpAFAmZD2lkSHGFwaUBleGFt
cGxlLmNvbQAKCRAA0FzmoO2vGdcGd4QQAJAa1Av9RB6Z3eHmAaZMZmcCJSf7BMOu
8x5pXJEhsKMjBPdQVqcUNFRkLM4l5qf5gjGmYUg2FjPRzWPgF6PEy3rcDFEYwBJL
fLeYqQQ7HxmmM5IvT4UQK9HOZpvCEvL6e5Z9j7L0Q3VhwQQ7vPmcCWlxRY5xQOgC
=DGmh
-----END PGP SIGNATURE-----
