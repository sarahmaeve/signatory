# Release checksums

Verify the release artifacts against these SHA-256 hashes:

| File | SHA-256 |
|------|---------|
| signatory-linux-amd64.tar.gz | 8d969eef6ecad3c29a3a629280e686cf0c3f5d5a86aff3ca12020c923adc6c92 |
| signatory-linux-arm64.tar.gz | e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 |
| signatory-darwin-amd64.tar.gz | a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3 |
| signatory-darwin-arm64.tar.gz | f96c0d5e9b5a01c8ce0e6c1a9c2c8c2e7f5a8e9b2d1c4a3b6e8f7d5c9a3e2b1d0 |

Each hash is 64 hex chars — well below the 512-char threshold the
encoded_blob detector uses. The detector must not fire.
