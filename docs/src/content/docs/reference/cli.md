---
title: CLI
description: gocdnext command-line tool — apply pipelines, manage secrets, bootstrap local users. What ships today, with the verbs and flags you'll actually use.
---

The `gocdnext` CLI is a thin client over the server's REST API
plus a couple of break-glass ops commands that talk to Postgres
directly. This page is the authoritative list of what's shipped.

Server-facing commands (`apply`, `secret`) authenticate with an
API token: run `gocdnext login` once per server (interactive),
or set `GOCDNEXT_TOKEN` (CI/bots). Against a deployment with
`auth.enabled=false` no token is needed and the CLI works as-is.

## Install

```bash
go install github.com/klinux/gocdnext/cli/cmd/gocdnext@latest
gocdnext --version
```

No Go toolchain? Download a prebuilt binary from the
[releases](https://github.com/klinux/gocdnext/releases) page — each release
carries `gocdnext_<version>_<os>_<arch>` for linux/darwin/windows on
amd64/arm64, plus a `checksums.txt` and its keyless Sigstore signature:

```bash
v=0.96.0   # the release you're installing
base="https://github.com/klinux/gocdnext/releases/download/v${v}"
curl -LO "${base}/gocdnext_${v}_linux_amd64"
curl -LO "${base}/checksums.txt"

# Optional but recommended: verify the checksums file was signed by the
# release workflow, then verify your binary against it.
curl -LO "${base}/checksums.txt.sig"
curl -LO "${base}/checksums.txt.pem"
cosign verify-blob checksums.txt \
  --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/klinux/gocdnext/.github/workflows/release.yml@refs/tags/v'
sha256sum --ignore-missing -c checksums.txt

install -m0755 "gocdnext_${v}_linux_amd64" /usr/local/bin/gocdnext
gocdnext --version
```

## Top-level shape

```
gocdnext --version
gocdnext login  --server <url>  [flags...]
gocdnext logout --server <url>
gocdnext scaffold [path]  [--dry-run] [--force] [--default-branch <b>]
gocdnext validate [path]
gocdnext run-local [file]
gocdnext apply [path]   --slug <slug>     [flags...]
gocdnext secret set <NAME>  --slug <slug> [flags...]
gocdnext secret list        --slug <slug> [flags...]
gocdnext secret rm  <NAME>  --slug <slug> [flags...]
gocdnext compliance frameworks list           [--server <url>]
gocdnext compliance policies list             [--server <url>]
gocdnext compliance effective-pipeline <slug> [--frameworks a,b] [--server <url>]
gocdnext admin create-user    --email <e> [flags...]
gocdnext admin reset-password --email <e> [flags...]
```

There are no `run`, `runs`, `logs`, `rerun`, `cancel`, `approve`,
or `profiles` subcommands today. Trigger runs, view logs, and
manage profiles via the dashboard or via the HTTP API directly.

## `login` / `logout` — authenticate the CLI

Create an API token in the web UI (*Account → API tokens*, see
[API tokens](/install/api-tokens/)), then:

```bash
# Interactive: silent TTY prompt, like sudo.
gocdnext login --server https://ci.example.com

# Piped / from a file — same contract as `secret set`.
cat token.txt | gocdnext login --server https://ci.example.com
gocdnext login --server https://ci.example.com --from-file ./token.txt
```

The token is **never accepted from a flag** (shell history, `ps`).
It is validated against `GET /api/v1/me` before being saved to
`~/.config/gocdnext/config.json` (mode `0600`), keyed by server URL
— you can stay logged into several servers at once. Every
server-facing command then sends it as `Authorization: Bearer`.

Resolution order: `GOCDNEXT_TOKEN` env var → config file → none.
The env var wins so CI jobs and bots never need a config file:

```bash
GOCDNEXT_TOKEN="$CI_API_TOKEN" gocdnext apply . --slug myapp --server https://ci.example.com
```

`gocdnext logout --server <url>` removes the local copy. It does
**not** revoke the token — do that in the web UI.

A `401` from any command prints a hint pointing back at `login`.

## `scaffold` — generate a starter pipeline set

Statically detects a repo's stack (Node / Go, package manager,
Dockerfile) and writes a starter set of pipelines to `.gocdnext/` —
**build-pr**, **build**, **security**. Deploy is intentionally left to
you (targets, secrets and approval policy are too environment-specific
to guess). Detection only reads manifest files (`package.json`,
`go.mod`, lockfiles); it never runs the repo's code.

```bash
cd my-new-app
gocdnext scaffold                        # detect + write .gocdnext/
gocdnext scaffold --dry-run              # print the files without writing
gocdnext scaffold --force                # overwrite existing .gocdnext/ files
gocdnext scaffold --default-branch main  # branch the build pipeline triggers on
```

Every generated file is validated with the real parser before it's
written, and each is a **starting point** — review it, fill the TODO
placeholders (e.g. the image registry), then `gocdnext validate` and
commit. Node uses the `node@v3` plugin (node version + manager come from
`engines.node` / the lockfile); the image job appears only when a
`Dockerfile` is present. Missing `test`/`build` scripts are reported as
warnings, not guessed.

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

## `compliance` — inspect frameworks, policies, effective pipeline

Read-only inspection of the [compliance pipelines](/concepts/compliance/)
surface. Authoring frameworks and policies stays in the dashboard
(admin-only, separation of duties); the CLI only lists and previews.
All three commands are admin-gated and use the same auth as `apply` /
`secret` (`--server` / `GOCDNEXT_SERVER_URL`, bearer token from
`gocdnext login` or `GOCDNEXT_TOKEN`).

```bash
# Framework catalogue. The first column is the id — feed it to --frameworks.
gocdnext compliance frameworks list

# Policies (metadata: mode, priority, targeting, enabled).
gocdnext compliance policies list

# Effective (post-merge) pipeline for a project. Jobs/stages a policy
# injected are marked [enforced]; the synthetic pipeline [server-managed].
gocdnext compliance effective-pipeline payments
```

Without `--frameworks`, `effective-pipeline` shows what runs today (the
stored effective definition). With `--frameworks <id,id>` it is a
**what-if** recompute for that hypothetical framework set — nothing is
persisted, and (mirroring a real save) it is refused if that governance
couldn't be enforced, e.g. a project with no SCM source:

```bash
# Preview what assigning PCI + SOC2 would enforce, before assigning them.
gocdnext compliance effective-pipeline payments --frameworks 4f1c…,9ab2…
```

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

## `validate` — parse pipelines without a server

```bash
gocdnext validate            # ./.gocdnext/*.yaml (or ./*.yaml)
gocdnext validate path/to/repo
gocdnext validate .gocdnext/ci.yaml
```

Runs every file through the **real server parser** (the same code
the apply path uses — not a re-implementation), one line per file:

```
OK   ci.yaml — pipeline "ci", 3 job(s)
FAIL release.yaml: release.yaml: when.paths: "[unclosed" is not a valid glob
```

Every file is checked even after the first failure; any failure
exits 1. No Docker, no network, no server.

## `run-local` — execute a pipeline on your machine

```bash
gocdnext run-local .gocdnext/ci.yaml
gocdnext run-local ci.yaml --job lint
gocdnext run-local release.yaml --env-file .env.local --event push
```

The `woodpecker exec` of gocdnext: stages in declared order,
`needs`-respecting order inside each stage, matrix expanded one
container per cell (dims ride `GOCDNEXT_MATRIX="K=V,..."`, exactly
like dispatch — never decomposed into individual env vars),
`services:` on a per-run docker network
(reachable by name, exactly like the cluster), one host directory
(`--workspace`, default `.`) mounted at `/workspace` and shared by
every job. Plugin jobs get the same `PLUGIN_*` env transform the
agent applies, and `${{ NAME }}` / `${VAR}` substitution follows
the dispatch contract (strict refs fail loud on unresolved names;
unknown shell vars stay literal). `CI_*` vars are synthesized from
the local git checkout (`GOCDNEXT_LOCAL=true` marks these runs).

| Flag | Meaning |
|---|---|
| `--workspace` | host dir mounted at `/workspace` (default `.`) |
| `--job NAME` | run a single job, skip everything else |
| `--env-file` | `KEY=VALUE` file resolving `secrets:` — a declared secret missing from it fails loud |
| `--event` | synthesized `CI_CAUSE`: `push` / `pull_request` / `manual` |

**Deliberately not simulated:** caches (your local state IS the
cache), the artifact backend (jobs share the mounted workspace,
which covers the common flows), `id_tokens:` (no issuer to mint
from), runner profiles / resources / tags (cluster concerns).
Approval gates are auto-skipped with a loud warning — a real run
parks there.

## Environment

| Var | Used by | Notes |
|---|---|---|
| `GOCDNEXT_SERVER_URL` | `login`, `logout`, `apply`, `secret`, `compliance` | HTTP URL of the server. Defaults to `http://localhost:8153`. |
| `GOCDNEXT_TOKEN` | `apply`, `secret`, `compliance` | API token for Bearer auth. Overrides the config file — use in CI/bots. |
| `GOCDNEXT_DATABASE_URL` | `admin create-user`, `admin reset-password` | Postgres URL the server uses. Write access required. |

## Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| non-zero | error (server returned non-2xx, validation failed, IO error). The CLI prints `error: <msg>` to stderr. |

Errors go to stderr; stdout carries only command output (slug
print-outs, summaries) so pipelines that consume the output can
parse it cleanly.
