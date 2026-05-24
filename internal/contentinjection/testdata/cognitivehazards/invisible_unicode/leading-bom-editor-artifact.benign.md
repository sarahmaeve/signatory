# Editor artifact

Some Windows editors prepend a UTF-8 BOM (U+FEFF as bytes EF BB BF)
to text files they save. This is benign: the BOM is at byte 0, no
adversarial intent. The scanner's position-anchored tolerance
recognizes this case.

The rest of this file is plain ASCII so the detector cannot fire on
anything else.
