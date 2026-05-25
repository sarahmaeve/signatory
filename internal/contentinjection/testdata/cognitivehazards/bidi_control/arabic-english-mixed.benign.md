# Internationalization README

This project supports right-to-left rendering for Arabic and
Hebrew interfaces. Browser-native handling does the work — no
explicit bidi control codepoints are needed in the source.

Example Arabic prose embedded in English: "the welcome message
displays مرحبا بكم في موقعنا on the home page" reads correctly in
any browser thanks to the Unicode bidi algorithm's natural-
direction defaults.

Hebrew likewise: שלום עולם appears right-to-left without any
codepoint annotations.

No U+202A–U+202E and no U+2066–U+2069 codepoints appear in this
file. The detector must produce zero bidi_control findings.
