# Authentication example

Authorization header carries a JWT:

```
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c
```

A JWT is three base64url-encoded segments separated by dots. The
signature segment (after the second dot) is typically ~340 chars
for HS256 — below the 1024-char base64 threshold. The detector must
not fire.
