# Security Policy

gocdnext runs untrusted build steps as containers and handles secrets
(registry credentials, cloud tokens, signing keys). We take security reports
seriously and appreciate coordinated disclosure.

## Supported versions

gocdnext is pre-1.0 and ships monthly. Only the **latest released `v0.x`
minor** receives security fixes. There is no backporting to older minors while
we are in the `0.x` line — upgrade to the latest release to receive fixes.

| Version           | Supported          |
| ----------------- | ------------------ |
| latest `v0.x`     | :white_check_mark: |
| any older release | :x:                |

## Reporting a vulnerability

**Please do not open a public issue, discussion, or pull request for a
security vulnerability.** A public report tells attackers about the problem
before a fix exists.

Instead, report privately through **GitHub Security Advisories**:

1. Go to the [Security tab](https://github.com/klinux/gocdnext/security) of the
   repository.
2. Click **Report a vulnerability** to open a private advisory visible only to
   the maintainers.

This keeps the report, the discussion, and the eventual fix coordinated in one
private place, and lets us credit you in the published advisory.

If GitHub private reporting is unavailable to you, email the maintainer at
**klinux@gmail.com** with `[gocdnext security]` in the subject. Encrypt if you
can; otherwise send enough detail for us to reproduce.

### What to include

- Affected component (`server`, `agent`, `cli`, a specific plugin, `web`) and
  version / commit.
- A description of the impact (what an attacker gains: secret disclosure, RCE
  on the agent, privilege escalation across projects/tenants, auth bypass,
  SSRF, etc.).
- Steps to reproduce, a proof of concept, or a failing test — the more concrete,
  the faster we can confirm.
- Any suggested remediation, if you have one.

### What to expect

- **Acknowledgement within 3 business days.**
- An initial assessment (severity, whether we can reproduce) within 7 days.
- We will keep you updated as we work on a fix, agree on a disclosure timeline
  with you, and credit you in the advisory unless you prefer to remain
  anonymous.
- We aim to release a fix and publish the advisory within 90 days of
  confirmation; critical issues are handled faster.

## Scope

In scope: the code in this repository — control-plane server, agent/runner,
CLI, web UI, reference plugins, proto contracts, migrations, and the Helm chart.

Out of scope: vulnerabilities in third-party dependencies (report those
upstream; we will bump once a fix is available), issues that require a
pre-compromised host or a malicious cluster-admin, and findings against a
deployment's own misconfiguration rather than gocdnext's defaults.

## Hardening notes for operators

- Keep secrets in an external backend (Vault / GCP / AWS) rather than plaintext
  where possible; resolved values are masked in logs, but the backend keeps
  them out of the database entirely.
- Restrict who can author pipelines: a pipeline runs arbitrary containers on
  your agents. Treat pipeline-authoring permission as production access.
- Terminate the server behind TLS and verify webhook HMAC secrets are set.
