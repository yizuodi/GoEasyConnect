# Security Policy

## Reporting a Vulnerability

Please do not open a public issue for a suspected vulnerability. Use GitHub's
private vulnerability reporting feature for this repository. Include affected
versions, reproduction steps, impact, and any suggested mitigation. If private
reporting is unavailable, contact the repository owner through the contact
method shown on their GitHub profile and request a private channel.

Please allow a reasonable time for investigation and remediation before public
disclosure. Reports involving exposed credentials should contain only redacted
examples; revoke the credentials before reporting.

## Deployment Guidance

GoEasyConnect provides authenticated remote terminal access and can launch
Claude Code or Codex with the permissions of its service account. Treat it as
an administrative interface:

- Keep `config.json`, databases, logs, and agent profiles out of source control.
- Use a long, unique `auth.password`; the example password is rejected.
- Keep the default `127.0.0.1` binding and publish through an HTTPS reverse
  proxy. Do not expose unencrypted HTTP directly to an untrusted network.
- Run the service as a dedicated, least-privileged user where practical.
- Enable skip-permissions/full-access modes only when their risk is understood.
- Keep GoEasyConnect, Claude Code, Codex, and the host operating system updated.
