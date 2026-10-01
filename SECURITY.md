# Security policy

## Reporting a vulnerability

Please report security problems privately through GitHub:
**Security → Report a vulnerability** on this repository
(private vulnerability reporting). Do not open a public issue.

You can expect a first answer within seven days. Please include the version or
image digest you used and the steps to reproduce.

## Supported versions

Only the latest release receives fixes.

## What this tool is trusted with

- A Zitadel token with **read** access to projects, users and user grants.
- A NetBird token that can create groups and change a user's group list. NetBird
  offers no narrower role for this than `admin`, so treat that token like an
  administrator credential.

The tool keeps both tokens in memory only, never logs them, and never logs
response bodies or email addresses. It talks to its two endpoints over HTTPS and
refuses plain HTTP for anything but a loopback address.

## Verifying a release image

Images are built by GitHub Actions and carry a signed build provenance
attestation:

```sh
gh attestation verify oci://ghcr.io/marcobockelbrink/zitadel-netbird-sync:<tag> \
  --owner marcobockelbrink
```
