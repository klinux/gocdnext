---
title: OIDC id_tokens (keyless cloud auth)
description: "Per-job OIDC JWTs exchanged for cloud credentials via workload identity federation — no long-lived service account keys in CI."
---

The `id_tokens:` block gives a job a short-lived, signed OIDC JWT
as an env var. Cloud providers verify it against the server's
public JWKS and exchange it for real credentials — GCP Workload
Identity Federation, AWS IAM OIDC, Azure federated credentials,
Vault's JWT auth. The long-lived service account key in `secrets:`
disappears; the token lives minutes, names exactly one job run,
and can't be replayed after it expires.

This is the same model as GitHub Actions' `id-token: write` and
GitLab CI's `id_tokens:` — the YAML shape follows GitLab for
migration ergonomics.

## Declaring tokens

```yaml
jobs:
  deploy:
    stage: ship
    image: google/cloud-sdk:slim
    id_tokens:
      GCP_ID_TOKEN:
        aud: https://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/ci/providers/gocdnext
      VAULT_JWT:
        aud: [https://vault.example.com, https://vault-dr.example.com]
    script:
      - gcloud iam workload-identity-pools create-cred-config ... --credential-source-file=<(echo "$GCP_ID_TOKEN")
      - ./deploy.sh
```

- Map key = the env var the JWT is injected as. POSIX charset,
  `CI_`/`GOCDNEXT_` prefixes reserved, no collisions with
  pipeline-level or job-level `variables:` nor the job's
  `secrets:`.
- `aud` is **required** — scalar or list. It must match the
  audience your cloud trust config expects, byte for byte. There
  is no default on purpose: a token whose audience silently
  equals the issuer URL passes misconfigured verifiers.
- Multiple tokens per job are fine (GCP + Vault in one deploy).
- The JWT value is automatically added to the job's log masks —
  it never appears in log streams.

## Server requirements

The issuer turns on when BOTH are configured (no separate flag):

| Requirement | Why |
|---|---|
| `GOCDNEXT_PUBLIC_BASE` (chart: `server.publicBase`) | Becomes the `iss` claim and serves `/.well-known/openid-configuration` + `/.well-known/jwks.json`, which cloud verifiers must reach over the public internet, via **HTTPS** |
| `GOCDNEXT_SECRET_KEY` | The RSA-2048 signing key is generated on first boot and stored sealed (AES-256-GCM) in Postgres |

A job that declares `id_tokens:` while the issuer is disabled
**fails at dispatch** with a configuration error — never a silent
dispatch without the token, never a token with a wrong `iss`.

Server clocks must be NTP-synced: tokens carry a 60s `nbf`
backdate for skew, but a badly drifting clock breaks verification
anyway.

## Claims

| Claim | Value | Notes |
|---|---|---|
| `iss` | the server's public base URL | |
| `sub` | see grammar below | THE policy-matching surface |
| `aud` | as declared | string when one, array when several |
| `exp` | mint + TTL | TTL default 1h (`GOCDNEXT_OIDC_TOKEN_TTL`, clamp 5m–24h) |
| `nbf` / `iat` / `jti` | standard | jti unique per mint; reruns mint fresh |
| `project_slug` `project_id` `pipeline` `pipeline_id` `job` `run_id` `run_counter` `cause` | always present, all strings | |
| `ref` / `ref_type` / `sha` | branch or tag context | omitted when no material; `ref_type` is always present (`"none"` when refless) |
| `matrix_key` | only on matrix-expanded jobs | |
| `pr_number` | only on `pull_request` runs | |

Tokens are minted at **dispatch**, not at the moment your script
uses them — on long builds, exchange the token early (or raise
the TTL) so it hasn't expired by the time `gcloud`/`vault` runs.

## The `sub` grammar (pin your policies here)

```
branch run:   project:{slug}:pipeline:{name}:ref_type:branch:ref:{branch}
tag run:      project:{slug}:pipeline:{name}:ref_type:tag:ref:{tag}
PR run:       project:{slug}:pipeline:{name}:pull_request
no material:  project:{slug}:pipeline:{name}:ref_type:none:ref:none
```

**Pull-request runs carry no ref segment at all.** The PR head
ref name is attacker-controlled — a PR opened from a branch named
`main` must never satisfy a `...ref_type:branch:ref:main` cloud
policy. Because the PR sub is a different shape entirely, every
branch-pinned policy excludes PRs *by construction*; you don't
have to remember to exclude them. If you intentionally want PR
runs to obtain (low-privilege) credentials, write a separate
policy matching the `:pull_request` sub.

`:` is reserved as the grammar separator: pipeline names can't
contain it (rejected at apply), and any residual `:`/`%` in
segments is percent-encoded.

## Cloud trust configuration

### GCP — Workload Identity Federation

```bash
gcloud iam workload-identity-pools providers create-oidc gocdnext \
  --workload-identity-pool=ci --location=global \
  --issuer-uri="https://gocdnext.example.com" \
  --allowed-audiences="https://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/ci/providers/gocdnext" \
  --attribute-mapping="google.subject=assertion.sub,attribute.pipeline=assertion.pipeline,attribute.cause=assertion.cause" \
  --attribute-condition="assertion.sub == 'project:shop:pipeline:deploy:ref_type:branch:ref:main'"
```

Use **equality** for exact branch pins — `startsWith(...'ref:main')`
would also match `main-fix`, `main-evil`, and every other branch
sharing the prefix, silently broadening the trust policy. Reach for
`startsWith` only when you genuinely mean a prefix (e.g. pinning a
whole project: `assertion.sub.startsWith('project:shop:')`).

Then grant the pool identity on the target service account:

```bash
gcloud iam service-accounts add-iam-policy-binding deployer@PROJECT.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser \
  --member="principal://iam.googleapis.com/projects/123/locations/global/workloadIdentityPools/ci/subject/project:shop:pipeline:deploy:ref_type:branch:ref:main"
```

### AWS — IAM OIDC provider

Create the provider with the issuer URL, then a role whose trust
policy pins sub + aud:

```json
{
  "Effect": "Allow",
  "Principal": {"Federated": "arn:aws:iam::ACCOUNT:oidc-provider/gocdnext.example.com"},
  "Action": "sts:AssumeRoleWithWebIdentity",
  "Condition": {
    "StringEquals": {
      "gocdnext.example.com:aud": "sts.amazonaws.com",
      "gocdnext.example.com:sub": "project:shop:pipeline:deploy:ref_type:branch:ref:main"
    }
  }
}
```

### Vault — JWT auth

```bash
vault write auth/jwt/config oidc_discovery_url="https://gocdnext.example.com"
vault write auth/jwt/role/deploy \
  role_type=jwt user_claim=sub \
  bound_audiences="https://vault.example.com" \
  bound_claims='{"sub": "project:shop:pipeline:deploy:ref_type:branch:ref:main"}' \
  policies=deploy ttl=15m
```

### Azure — federated credential

Add a federated credential on the app registration: issuer = the
server URL, subject = the exact `sub` string, audience = your
declared `aud`.

## Key rotation

Keys live in Postgres (sealed by the server's secret key) and
rotate via the admin API:

```bash
# Graceful (default): the old key keeps verifying in the JWKS
# until every in-flight token has expired.
curl -X POST -H "Authorization: Bearer $TOKEN" \
  https://gocdnext.example.com/api/v1/admin/oidc/keys/rotate

# Emergency (key compromise): the old key leaves the JWKS
# IMMEDIATELY. Outstanding tokens stop verifying — that's the
# point. Re-trigger in-flight deploy jobs afterwards.
curl -X POST -H "Authorization: Bearer $TOKEN" \
  -d '{"mode":"emergency"}' \
  https://gocdnext.example.com/api/v1/admin/oidc/keys/rotate
```

Rotation propagation, precisely: the replica that handles the
rotate request swaps keys atomically (no token signed by the old
key is returned after the commit). Other replicas converge via a
Postgres NOTIFY fired inside the rotation transaction — typically
single-digit milliseconds; if a replica's listener happens to be
reconnecting, its 60-second cache TTL is the backstop. The
dominant bound in practice is on the VERIFIER side: clouds may
cache the JWKS for up to 5 minutes (`max-age=300`) — treat that
as the upper bound on emergency-revocation taking effect.
`GET /api/v1/admin/oidc/keys` lists lifecycle metadata (kid +
dates, never material). Every rotation is audit-logged.

## Verifying the setup

```bash
curl https://gocdnext.example.com/.well-known/openid-configuration | jq .
curl https://gocdnext.example.com/.well-known/jwks.json | jq .

# Inside a job: decode the payload (NOT a verification — just a look)
echo "$GCP_ID_TOKEN" | cut -d. -f2 | base64 -d 2>/dev/null | jq .
```
