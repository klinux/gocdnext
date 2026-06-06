---
title: CLI
description: gocdnext command-line tool — apply pipelines, manage secrets, bootstrap local users. What ships today, with the verbs and flags you'll actually use.
---

The `gocdnext` CLI is a thin client over the server's REST API
plus a couple of break-glass ops commands that talk to Postgres
directly. This page is the authoritative list of what's shipped.

The CLI does **not** yet implement Bearer-token authentication.
Use it against a deployment with `auth.enabled=false`, or via a
local in-cluster connection, or in a dev environment. Wider
production use waits on the planned token plumbing.

## Install

```bash
go install github.com/klinux/gocdnext/cli/cmd/gocdnext@latest
gocdnext --version
```

Or download a prebuilt binary from the [release](https://github.com/klinux/gocdnext/releases) page.

## Top-level shape

```
gocdnext --version
gocdnext validate [path]
gocdnext run-local [file]
gocdnext apply [path]   --slug <slug>     [flags...]
gocdnext secret set <NAME>  --slug <slug> [flags...]
gocdnext secret list        --slug <slug> [flags...]
gocdnext secret rm  <NAME>  --slug <slug> [flags...]
gocdnext admin create-user    --email <e> [flags...]
gocdnext admin reset-password --email <e> [flags...]
```

There are no `run`, `runs`, `logs`, `rerun`, `cancel`, `approve`,
or `profiles` subcommands today. Trigger runs, view logs, and
manage profiles via the dashboard or via the HTTP API directly.

## `apply` — upload pipelines

Reads `.gocdnext/` under `[path]` and POSTs the parsed definitions
to `/api/v1/projects/apply`.

```bash
gocdnext apply . \
  --slug myapp \
  --name "My App" \
  --description "Frontend + API" \
  --config-repo https://github.com/myorg/myapp \
  --server https://ci.example.com \
  --scm-url https://github.com/myorg/myapp \
  --scm-provider github \
  --scm-default-branch main \
  --scm-webhook-secret "$(pwgen -s 32 1)"
```

| Flag | Required | Notes |
|---|---|---|
| `--slug` | yes | project slug (must be URL-safe) |
| `--name` | no | display name; defaults to the slug |
| `--description` | no | free-text description |
| `--config-repo` | no | URL of the repo the pipelines live in |
| `--server` | no | `GOCDNEXT_SERVER_URL` env or `http://localhost:8153` |
| `--scm-url` | no | repo URL of the SCM source (pairs with `--scm-provider`) |
| `--scm-provider` | no | `github` \| `gitlab` \| `bitbucket` |
| `--scm-default-branch` | no | repo default branch (e.g. `main`) |
| `--scm-webhook-secret` | no | HMAC secret for the webhook |

Output: a per-pipeline added/changed/removed summary.

## `secret set/list/rm` — project secrets

```bash
# Set: value comes from stdin (piped) or interactive prompt — never a flag.
echo "$(pass aws/ci-deploy)" | gocdnext secret set --slug myapp AWS_ACCESS_KEY_ID
# OR
gocdnext secret set --slug myapp AWS_ACCESS_KEY_ID --from-file ./key.txt

# List names + last-updated timestamps (values stay encrypted).
gocdnext secret list --slug myapp

# Remove.
gocdnext secret rm --slug myapp AWS_ACCESS_KEY_ID
```

`--slug` identifies the project (NOT `--project`). The value is
deliberately accepted only from stdin, file, or interactive TTY
prompt — never from a flag — so secrets don't leak via shell
history or `ps auxww`.

Global (cross-project) secrets are managed from the dashboard at
`/admin/secrets`. There is no CLI flow for global secrets today.

## `admin create-user` — bootstrap local user

Break-glass: writes directly to the Postgres `users` table. Use
to bootstrap the first admin before an OIDC provider is wired or
to recover when SSO is broken.

```bash
echo 'choose-a-strong-password' | gocdnext admin create-user \
  --email alice@example.com \
  --name "Alice" \
  --role admin \
  --database-url postgres://gocdnext:pw@db.internal:5432/gocdnext
```

| Flag | Required | Notes |
|---|---|---|
| `--email` | yes | login email |
| `--name` | no | display name (defaults to local-part) |
| `--role` | no | `admin` \| `user` \| `viewer` (default `admin`) |
| `--database-url` | no | `GOCDNEXT_DATABASE_URL` env |

Password from stdin / `--from-file` / silent TTY prompt — same
contract as `secret set`. Re-running with the same email rotates
the password + role + name.

## `admin reset-password` — rotate a password

```bash
echo 'new-password' | gocdnext admin reset-password \
  --email alice@example.com \
  --database-url postgres://gocdnext:pw@db.internal:5432/gocdnext
```

Same flags as `create-user` minus `--name` / `--role`.

## `validate` and `run-local` — stubs

Both commands exist as Cobra placeholders today and print `TODO`
on invocation. They will land:

- `validate [path]` — parse + apply-time validate `.gocdnext/`
  without touching the server.
- `run-local [file]` — execute a single pipeline against the local
  Docker daemon for fast iteration.

Watch the repository roadmap if these are blocking.

## Environment

| Var | Used by | Notes |
|---|---|---|
| `GOCDNEXT_SERVER_URL` | `apply`, `secret` | HTTP URL of the server. Defaults to `http://localhost:8153`. |
| `GOCDNEXT_DATABASE_URL` | `admin create-user`, `admin reset-password` | Postgres URL the server uses. Write access required. |

There is no CLI-side token env yet. Pair Bearer-token usage with
`curl` directly against `/api/v1/*` for authenticated calls until
the CLI grows token plumbing.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| non-zero | error (server returned non-2xx, validation failed, IO error). The CLI prints `error: <msg>` to stderr. |

Errors go to stderr; stdout carries only command output (slug
print-outs, summaries) so pipelines that consume the output can
parse it cleanly.
