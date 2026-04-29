---
title: Container layer cache (S3 / GCS)
description: Wire BuildKit's S3/GCS layer cache through a runner profile so every build inherits the bucket creds without per-job plumbing.
---

Container builds get fast layer reuse without each pipeline carrying
its own AWS keys. The recipe puts the cache config + creds on the
**runner profile** once; every job that lands on that profile picks
them up automatically.

## What it solves

Per-job credential plumbing for layer cache is a foot-gun: bucket
keys leak into pipeline YAML, rotation means editing N projects, and
multi-stage builds repeat the same `secrets:` block. The cache
itself is operator-level config — it shouldn't be project author's
problem.

The runner profile model already carries execution policy
(image, CPU/mem, tags). Adding `env:` and `secrets:` to the same
primitive lets the agent inject them into every plugin container
that runs on that profile. BuildKit's `type=s3` cache backend reads
`AWS_*` from env automatically, so the buildx plugin's
`cache: bucket` shorthand is enough on the project side.

## 1. Configure the profile (admin only)

Open *Settings → Profiles* in the dashboard and create or edit a
profile (call it `fast-builds`):

| Field | Example value |
|---|---|
| **Name** | `fast-builds` |
| **Engine** | `kubernetes` |
| **Default image** | `alpine:3.20` |
| **Tags** | `linux`, `docker` |
| **Env** | `GOCDNEXT_LAYER_CACHE_BUCKET=gocdnext-cache` |
|         | `GOCDNEXT_LAYER_CACHE_REGION=us-east-1` |
|         | `AWS_REGION=us-east-1` |
| **Secrets** | `AWS_ACCESS_KEY_ID=AKIA…` |
|             | `AWS_SECRET_ACCESS_KEY=…` |

Secrets are encrypted at rest with the same AEAD cipher as project
secrets (`GOCDNEXT_SECRET_KEY`). The UI never echoes the values back
once saved — the row shows `••••••• (stored)` and you click
**Replace** to overwrite.

### Reference a global secret instead of pasting the value

Each secret row has a 🔗 button that opens a picker listing every
configured global secret (admin-managed in *Settings → Secrets*).
Click one and the value field becomes `{{secret:NAME}}` — at
dispatch time the server resolves the template against the global
table, so rotating `AWS_ACCESS_KEY_ID` once globally propagates to
every profile that references it. Rows stored as a clean reference
render in the editor as a chip `→ globals.NAME` instead of the
masked placeholder.

Mixed values work too (`prefix-{{secret:DB_PASSWORD}}-suffix` is
honoured at dispatch) — only the chip rendering is skipped because
the row carries literal text alongside the template.

Failure mode is fail-closed: if the referenced global is deleted,
dispatch refuses with a clear error rather than ship an empty env
var into the build.

The IAM key the profile carries should be scoped to the cache bucket
+ prefix only:

```json title="iam-policy.json"
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket"],
    "Resource": [
      "arn:aws:s3:::gocdnext-cache",
      "arn:aws:s3:::gocdnext-cache/*"
    ]
  }]
}
```

For GCS, the equivalent is HMAC keys with bucket-scoped IAM
bindings (works through the BuildKit `type=s3` backend + GCS interop
endpoint). Add `GOCDNEXT_LAYER_CACHE_ENDPOINT=https://storage.googleapis.com`
to the profile env so the plugin emits the right `endpoint_url=`.

## 2. Use the cache in a pipeline

```yaml title=".gocdnext/pipeline.yaml"
jobs:
  build:
    agent:
      profile: fast-builds   # ← inherits env + secrets
    docker: true
    tasks:
      - uses: gocdnext/buildx@v1
        with:
          image: ghcr.io/org/app
          tags: latest
          cache: bucket       # ← reads GOCDNEXT_LAYER_CACHE_*
```

That's it. No `secrets:` list at the job level, no bucket coords in
YAML, no per-project key sharing. Every build that lands on
`fast-builds` writes to and reads from the same S3 cache namespaced
by image (`name=ghcr.io/org/app` becomes the manifest key in the
bucket).

## What the plugin generates under the hood

```bash
docker buildx build \
  --cache-to   type=s3,region=us-east-1,bucket=gocdnext-cache,name=ghcr.io/org/app,mode=max \
  --cache-from type=s3,region=us-east-1,bucket=gocdnext-cache,name=ghcr.io/org/app \
  -t ghcr.io/org/app:latest \
  --push .
```

Override the cache key (e.g. share between two image names):

```yaml
env:
  GOCDNEXT_LAYER_CACHE_NAME: shared-cache-key
```

Override the backend (Azure, GHA cache, etc.) by skipping `cache: bucket`
and writing the spec verbatim:

```yaml
- uses: gocdnext/buildx@v1
  with:
    image: ghcr.io/org/app
    cache-to:   type=azblob,name=org-cache,account_url=https://<acct>.blob.core.windows.net
    cache-from: type=azblob,name=org-cache,account_url=https://<acct>.blob.core.windows.net
    secrets: [AZURE_STORAGE_KEY]
```

## Logs stay clean

The runner echoes every profile secret value into the assignment's
`log_masks` list. The agent replaces matches with `***` before
streaming log lines back to the server, so a stray `printenv` in a
RUN step never leaks the AWS key into stored logs.

## Trade-offs you're accepting

- **Scope is profile-wide.** Every project that runs on
  `fast-builds` shares the bucket creds. For multi-tenant clusters
  where projects don't trust each other, run them on separate
  profiles (one bucket per profile, distinct IAM keys).
- **Static credentials.** The profile holds long-lived AWS keys, not
  STS short-lived tokens. Rotate manually via the UI or by editing
  the profile through the API. STS-style scoping (per-job, per-prefix)
  isn't on the runner profile model — that's a follow-up if a
  multi-tenant deployment ever asks for it.
- **GCS doesn't get per-prefix scoping** even via this recipe. GCP's
  IAM doesn't support inline policies the way AWS does — the bucket
  binding on the SA / HMAC key is the only enforcement axis. Use
  one bucket per project if isolation matters.

## Alternative for projects that already push to a registry

When the build pushes to a registry you already authenticate with,
the simplest cache option uses that same registry — no bucket, no
extra creds:

```yaml
- uses: gocdnext/buildx@v1
  with:
    image: ghcr.io/org/app
    cache: registry   # writes ghcr.io/org/app:buildcache
```

Slower than S3 above ~10 GB of layers but trivial to set up; pick
this when you don't already operate a cache bucket.
