# SSH key fingerprint example

The user's public-key fingerprint:

```
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDfM9oNxV5K8MqQwWrXyZbGc7nUfTLkPRhjE6Vd2YwoB user@host
```

Ed25519 public keys encode to ~68 base64 chars and signatures to
~88 chars. Both are below the 1024-char base64 threshold. The
detector must not fire.
