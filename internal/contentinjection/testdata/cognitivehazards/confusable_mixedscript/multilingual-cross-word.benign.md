# Multilingual documentation

This README demonstrates that legitimate multilingual content
spans multiple scripts but each WORD is single-script.

## English with quoted translations

The Russian translation of "hello" is привет (each word
single-script: English / Cyrillic).

The Greek mathematical notation: Ω-fold cross-validation uses
δ as a step size. (Note: Ω-fold and δ each appear as a single
token; Latin-Greek mixing within those tokens is mathematical
notation, which v0 deliberately does NOT flag — see corpus
README.)

The Japanese word for "documentation" is ドキュメント.

The Chinese characters 中文 mean "Chinese language."

## Words are each single-script

- English: "hello", "world", "documentation"
- Russian: "привет", "мир", "документация"
- Greek: "Ωmega", "δelta" — these legitimately mix in math
- Japanese: "ドキュメント"
- Chinese: "中文"

Each token above contains letters from only one writing system
(except the deliberately-deferred Greek-Latin math case). The
detector must produce zero findings on Latin-Cyrillic or
Latin-Cherokee mixing because none appears.
