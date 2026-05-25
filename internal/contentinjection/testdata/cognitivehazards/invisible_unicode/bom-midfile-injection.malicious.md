# Project guidelines

Standard prose follows. The BOM (U+FEFF) at byte 0 of a UTF-8 file
is a common editor artifact, tolerated by the scanner.

A mid-file ﻿BOM sequence, however, is a position-anchored injection
marker — there is no editor that legitimately places a U+FEFF mid-
file. The detector's position-anchored tolerance only excuses the
leading BOM.
