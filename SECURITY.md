# Security policy

## Report a vulnerability

Report a vulnerability privately with
[GitHub private vulnerability reporting](https://github.com/massdriver-cloud/massdriver-automations/security/advisories/new).
Do not open a public issue.

Include the automation, the version or image digest, and the steps that show the problem. We reply within three
business days and tell you when a fix is released.

## Supported versions

Fixes go into the latest release. Use the newest `ghcr.io/massdriver-cloud/massdriver-automations` image tag.

## Verify an image

Each image has a signed build provenance attestation and an SBOM.

```bash
gh attestation verify oci://ghcr.io/massdriver-cloud/massdriver-automations:<version> \
  --owner massdriver-cloud
```

## What the automations hold

- An automation refuses an event whose Standard Webhooks signature does not verify.
- Credentials come only from environment variables. Config files hold the names of those variables, never values.
- Logs, errors, and trace spans never hold environment variables, config values, tokens, or webhook URLs. A test
  in `pkg/automation` enforces this.
- Images run as a non-root user on a distroless base.
