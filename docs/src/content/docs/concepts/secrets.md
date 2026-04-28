---
title: Secrets
description: Project + global secrets, masking conventions, two backends (DB and Kubernetes), and how the resolver injects them at dispatch time.
---

Secrets in gocdnext are values pipelines need but you don't want
in the YAML or in the run logs — registry tokens, deploy keys,
SMTP passwords, API tokens. The platform encrypts them at rest,
masks them in streamed log lines, and injects them as env vars
into the job containers that ask for them.

## Two scopes

### Project secrets

Live under *Project → Secrets* in the dashboard. Visible only to
operators with maintainer or admin role on that project. Used for
project-specific credentials (this project's deploy key, this
project's notification webhook).

### Global secrets

Live under *Settings → Secrets* (admin-only). Accessible from
**every** pipeline in **every** project. Used for org-wide
credentials (the org's npm registry token, the org's CI Docker
Hub account).

Resolution order: project secret first, global fallback. A project
can override a global secret by registering one with the same
name.

## How they reach the job

```yaml
jobs:
  deploy:
    secrets: [SSH_DEPLOY_KEY, SLACK_WEBHOOK]
    uses: gocdnext/ssh@v1
    with:
      key: ${{ secrets.SSH_DEPLOY_KEY }}
```

Two halves:

1. The `secrets:` array LISTS which secrets the job wants. Only
   listed names get injected — opt-in, not opt-out. Keeps the
   blast radius of a leaked plugin small (it can't `env | grep`
   for every secret).
2. `${{ secrets.NAME }}` in `with:` is the substitution syntax.
   The platform replaces it at dispatch time with the resolved
   value, after which the value is injected as the env var
   `SSH_DEPLOY_KEY` AND any string field referencing
   `${{ secrets.SSH_DEPLOY_KEY }}` is replaced.

Plugins typically prefer the env var path — `gocdnext/ssh`'s
entrypoint reads `PLUGIN_KEY` (from the `key:` input). The
substitution syntax is for when the plugin needs the secret
inline in a config string (rare).

## Masking

Every secret value the resolver produced for a run is registered
with the log streamer's mask list. As log lines arrive, any
substring that matches a registered value is replaced with `***`
before being persisted to `log_lines` AND before being published
to the SSE broker. So:

- Log entry written by the agent: `connecting with token=abc123XYZ`
- What lands in the database: `connecting with token=***`
- What live tail subscribers see: `connecting with token=***`

The masking is byte-faithful (no regex partial matches), so a
secret containing whitespace or tabs is masked as a whole unit.

**Caveats**:
- Encoded transformations defeat masking. If your secret is
  `abc123` and the agent logs it base64'd as `YWJjMTIz`, the
  masker doesn't know they're related.
- Truncation defeats masking. A logged prefix `abc12...` of
  `abc123` is NOT masked, because the byte string doesn't match.

Treat the masker as a defense-in-depth line, not the primary
control. Don't `echo $TOKEN` in scripts; use the env directly.

## Storage backends

### `db` (default)

Secrets are stored encrypted in the platform's Postgres, in the
`secrets` table. AES-256-GCM with a key derived from
`GOCDNEXT_SECRET_KEY` (set via Helm — wired from a managed
Kubernetes Secret).

Pros: zero infra dep beyond Postgres. Self-contained.

Cons: rotating `GOCDNEXT_SECRET_KEY` requires re-encrypting the
table (currently a maintenance window — built-in rotation tool is
on the roadmap).

### `kubernetes`

Secrets become Kubernetes Secret objects in the namespace gocdnext
runs in (or one configured via `GOCDNEXT_SECRET_K8S_NAMESPACE`).
Naming follows a template (default: `gocdnext-secrets-{slug}`).

Pros: integrates with ExternalSecrets / Vault Secret Operator /
sealed-secrets. Org-wide secret management tool of choice
"just works".

Cons: requires RBAC on the namespace; the agent needs read access
to the secret objects.

Switch via Helm:

```yaml
secrets:
  backend: kubernetes
  kubernetes:
    namespace: ""                  # empty = release namespace
    nameTemplate: "gocdnext-secrets-{slug}"
```

The `{slug}` placeholder expands to the project slug. So secrets
for project `myapp` land in Secret `gocdnext-secrets-myapp`.

## Rotating a secret

### From the dashboard

*Project → Secrets → Edit → Save new value*. The new value is
encrypted; subsequent runs use it. In-flight runs that already
resolved the old value continue with that — they're not retroactively
swapped.

### When the value is leaked

1. Update the upstream service (regenerate the GitHub PAT, the
   webhook URL, the deploy key, …).
2. Replace the value in the dashboard.
3. Rotate `GOCDNEXT_SECRET_KEY` if you suspect the platform's
   encryption key was compromised — different attack surface.

## Common pitfalls

- **Secret names collision with env vars**: don't name a secret
  `PATH`, `HOME`, etc. The resolver injects them and overrides
  the OS-defaults; jobs misbehave subtly. Convention is
  upper-case prefixed with the service: `GHCR_TOKEN`,
  `AWS_SECRET_ACCESS_KEY`, `SLACK_WEBHOOK`.
- **Secret in `script:`**: bash arithmetic / interpolation can
  echo the value via expansion (`echo "token=$TOKEN"` in a
  failed assertion). Use `set +x` blocks or trap on errors.
- **Long-form secret in `with:` strings**: PEM-encoded keys with
  newlines work, but the agent has to forward newlines through
  `-e VAR=value` to the container. The resolver handles this
  via a tempfile + `--env-file`. If you see truncated keys, file
  an issue; the resolver is supposed to handle this transparently.
- **Cross-project secret leakage**: project secrets are scoped
  by project_id; the resolver refuses to inject a secret from
  another project even if the pipeline's `secrets:` lists the
  same name. Global secrets are the only cross-project bridge.
