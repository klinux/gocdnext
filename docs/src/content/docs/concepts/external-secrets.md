---
title: External secret backends
description: Resolve pipeline secrets from HashiCorp Vault, GCP Secret Manager, or AWS Secrets Manager by reference, instead of storing the value in gocdnext.
---

By default a project secret is stored **in gocdnext** (encrypted at rest with
`GOCDNEXT_SECRET_KEY`) and you type its value in the UI. If you already run a
secret manager, you can instead register a secret as a **reference** to it —
the value lives in Vault / GCP Secret Manager / AWS Secrets Manager and
gocdnext fetches it at dispatch. Single source of truth, central rotation, and
the value never enters the gocdnext database.

The pipeline is unchanged either way: a job declares `secrets: [DB_PASSWORD]`
and the runner gets `DB_PASSWORD` in its environment, masked in logs.

## The reference model

Every secret entry has a `source`:

| source | what's stored | resolved at dispatch by |
|---|---|---|
| `db` (default) | the encrypted value | decrypting with the server cipher |
| `vault` | a pointer `{path, key}` | reading Vault KV |
| `gcp` | a pointer `{path, key}` | GCP Secret Manager `AccessSecretVersion` |
| `aws` | a pointer `{path, key}` | AWS Secrets Manager `GetSecretValue` |

You register a reference in *Settings → Secrets* (global) or a project's
*Secrets* tab: pick the source, give the path (and key). The list shows the
source and pointer — **never the value**. db-stored and externally-referenced
secrets coexist in the same project; a project secret still shadows a global of
the same name.

```text
DB_PASSWORD   → vault   secret/myapp # PASSWORD
S3_DEPLOY_KEY → aws     prod/deploy  # access_key
LEGACY_TOKEN  → db      ••••• (stored, encrypted)
```

## Configuring a backend

A reference picks a backend that must be **enabled on the server**. Two ways,
and you can mix them:

- **Settings → Secret backends** (admin UI) — enable/configure Vault, GCP, or
  AWS, with a **Test connection** button to validate credentials before you
  rely on them. Config is stored encrypted in the database and **takes effect
  immediately** (no restart) — including rotating a Vault AppRole `secret_id`.
- **Environment variables** (below) — the baseline/default. The UI config
  **overlays** env per backend; delete a UI entry to fall back to env. Env is
  handy for GitOps/bootstrap. The two are the same set of settings.

Any combination of backends can be enabled at once.

## Backends & auth (env baseline)

The env vars below are the baseline the Settings UI overlays.

- **Vault** — `GOCDNEXT_SECRET_VAULT_ENABLED=true` + `_ADDR`. Auth is
  **AppRole** (`_ROLE_ID` + `_SECRET_ID`, the primary), `kubernetes` (the
  pod ServiceAccount → a Vault role, keyless), or a static `token` (dev). KV
  v1 and v2 are auto-handled; a Vault reference needs a `key` (Vault secrets
  are key/value maps). On a `403` the backend re-authenticates once and
  retries.
- **GCP Secret Manager** — `GOCDNEXT_SECRET_GCP_ENABLED=true` + `_PROJECT`.
  Auth via Application Default Credentials (workload identity on GKE, or a key
  file via `GOOGLE_APPLICATION_CREDENTIALS`). `path` = the bare secret id
  (`gh-token`) — a full resource name (`projects/<p>/secrets/gh-token`) is also
  accepted, but `_PROJECT` is a tenancy boundary: a full name targeting a
  *different* project is rejected. `key` = version (`latest` when empty).
- **AWS Secrets Manager** — `GOCDNEXT_SECRET_AWS_ENABLED=true` + `_REGION`.
  Auth via the default credential chain (IRSA on EKS, env locally). `path` =
  secret id/ARN; an empty `key` returns the whole `SecretString`, a `key`
  extracts that field from a JSON secret.

## Security & behaviour

- **Masked like any secret.** A resolved external value enters the job's log
  masks the moment it's injected — the runner redacts it the same as a
  db-stored secret. The value is never logged and never persisted in gocdnext.
- **Fail-closed.** A reference to a backend that isn't configured, a db secret
  with no cipher, or a decrypt/fetch error fails the dispatch loudly (citing
  the secret **name**, never a value). A secret that simply doesn't exist in
  the backend is treated as "not set" — the run fails with
  `secrets not set on project: [...]`, exactly like a missing db secret.
- **Test connection vs least privilege.** The *Test connection* probe checks
  reachability + credentials, which can need broader read access than a job's
  dispatch (dispatch only needs `GetSecretValue` / `AccessSecretVersion` on the
  *referenced* secret). AWS uses STS `GetCallerIdentity` (no Secrets Manager
  permission required). GCP lists secrets, so it needs `secretmanager.secrets.list`
  on the project — a policy that grants only version access will dispatch fine
  but report the probe as unauthorized.
- **Cached briefly, deduped, bounded.** External lookups are cached
  (`GOCDNEXT_SECRET_CACHE_TTL`, default 60s; `0` disables) so a fan-out of jobs
  on the same path hits the backend once; concurrent cold-cache misses on the
  same path are collapsed into a single call (singleflight), and each lookup is
  bounded by `GOCDNEXT_SECRET_FETCH_TIMEOUT` (default 10s; `0` disables) so a
  hung backend can't stall dispatch of other jobs. Set the TTL to `0` if you
  need rotations to take effect instantly. An invalid value for either fails
  fast at boot.

## Migrating from db secrets

Re-register the secret as a reference (same name) and delete the db value —
pipelines that reference it by name keep working unchanged. Rotation then
happens in your secret manager, not in gocdnext.
