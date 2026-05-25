# widget-parser

[![CI](https://img.shields.io/github/actions/workflow/status/owner/repo/test.yml?branch=main)](https://github.com/owner/repo/actions)
[![License](https://img.shields.io/github/license/owner/repo)](LICENSE)

A small library for parsing widget configuration files.

## Features

- "Smart quotes" support (typographic punctuation)
- Multi-language: English, 日本語, Ελληνικά, العربية
- Emoji-friendly: 🎉 🚀 ✨

## Install

```
go get example.com/widget-parser
```

## API token format

Tokens are JWTs like:
```
eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c
```

## Checksums

Verify releases with SHA-256:

| File | Hash |
|------|------|
| widget-parser-linux-amd64.tar.gz | 8d969eef6ecad3c29a3a629280e686cf0c3f5d5a86aff3ca12020c923adc6c92 |

## License

MIT.
