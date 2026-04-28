---
title: CLI
description: The gocdnext CLI — apply pipelines, run on demand, validate locally.
---

`gocdnext` is the operator + author CLI. It talks to the server's
HTTP API on port 8153 (or whatever `GOCDNEXT_SERVER` points at).
Operator commands need an admin/maintainer session; author
commands work from any role with project access.

## Install

Built into the platform image — `kubectl exec` into a control-
plane pod and you have it. For local dev:

```bash
go install github.com/gocdnext/gocdnext/cli/cmd/gocdnext@latest
```

The CLI is a separate Go module (`cli/`) so its release cadence
can be independent of the server's. Versions track the server's
within the same minor (a v0.2.x CLI works against any v0.2.x
server).

## Common flags

| Flag | Default | Notes |
|---|---|---|
| `--server` | `$GOCDNEXT_SERVER` or `http://localhost:8153` | Server URL |
| `--token` | `$GOCDNEXT_TOKEN` | Bearer token (issued via `Settings → API tokens`) |
| `--output` | `text` | `text \| json` |

## `gocdnext apply`

Reads a directory's `.gocdnext/` folder + applies the project +
pipelines on the server. Idempotent.

```bash
gocdnext apply \
  --slug myapp \
  --name "My App" \
  --description "Production API" \
  --config-repo https://github.com/myorg/myapp \
  .
```

| Flag | Notes |
|---|---|
| `--slug` | URL-friendly identifier; must be unique per server |
| `--name` | Human-readable name |
| `--description` | Optional |
| `--config-repo` | URL of the SCM source for webhook drift |
| `[path]` | Directory containing `.gocdnext/` (default `.`) |

Apply walks every `.yaml` in `.gocdnext/`, parses each into a
pipeline, sends them all in one transactional request. If any
fails to parse, the whole apply fails — partial state is never
committed.

## `gocdnext validate`

Same parser as apply but doesn't talk to the server. Useful in
PR checks:

```bash
gocdnext validate .gocdnext/
```

Exit non-zero on any parse / schema error. Output the offending
file + line + column.

## `gocdnext run`

Manually trigger a pipeline. Useful for `event: [manual]` pipelines
or to retrigger a known pipeline outside the webhook flow.

```bash
gocdnext run --project myapp --pipeline cd
gocdnext run --project myapp --pipeline cd --branch hotfix/abc
gocdnext run --project myapp --pipeline cd --variable env=staging
```

| Flag | Notes |
|---|---|
| `--project` | Project slug |
| `--pipeline` | Pipeline name within the project |
| `--branch` | Branch to run against (default = pipeline's main branch config) |
| `--variable KEY=VALUE` | Repeatable; passes as `CI_*` env to every job |

## `gocdnext runs`

List recent runs for a project / pipeline.

```bash
gocdnext runs --project myapp
gocdnext runs --project myapp --pipeline ci-server --limit 20
gocdnext runs --project myapp --status failed
```

Output is a table:

```
RUN          PIPELINE     STATUS   STARTED              DURATION
#142         ci-server    success  2026-04-28 10:23:11  2m13s
#141         ci-server    failed   2026-04-28 10:18:44  1m52s
...
```

`--output json` returns the same data structured.

## `gocdnext logs`

Tail / dump logs for a specific run or job.

```bash
gocdnext logs --run abc-123-def-456
gocdnext logs --run abc-123-def-456 --job compile
gocdnext logs --run abc-123-def-456 --follow
```

`--follow` opens the SSE stream and prints each line as it
arrives — useful for watching a CI run from a terminal without
the dashboard.

## `gocdnext secret`

Project + global secrets management.

```bash
# Project secrets
gocdnext secret list --project myapp
gocdnext secret set --project myapp NAME=value
gocdnext secret rotate --project myapp NAME=newvalue
gocdnext secret rm --project myapp NAME

# Global secrets (admin only)
gocdnext secret list --global
gocdnext secret set --global NAME=value
```

Values can be piped on stdin (avoids them landing in shell
history):

```bash
read -s GHCR_TOKEN
echo "$GHCR_TOKEN" | gocdnext secret set --project myapp GHCR_TOKEN=-
```

`-` as the value means "read from stdin".

## `gocdnext rerun`

Re-run a previous run, or just one job within it.

```bash
gocdnext rerun --run abc-123-def-456
gocdnext rerun --run abc-123-def-456 --job flaky-test
```

The `--job` form is a single-job rerun: the job is reset to
`queued`, log_lines for the previous attempt are dropped, the
agent re-dispatches. Useful for flake recovery.

## `gocdnext cancel`

Cancel an in-flight run.

```bash
gocdnext cancel --run abc-123-def-456
```

The platform sends SIGKILL to the running container, the agent
reports the exit, the run terminates as `cancelled`. Cancellation
propagates to subsequent stages (they're never dispatched).

## `gocdnext approve`

Approve / reject a pipeline awaiting approval.

```bash
gocdnext approve --run abc-123-def-456 --job promote-prod
gocdnext approve --run abc-123-def-456 --job promote-prod --reject \
  --comment "Smoke test failed in staging"
```

Same effect as clicking *Approve* / *Reject* in the dashboard.
Audit trail records the actor.

## `gocdnext profiles`

Runner profile management (admin).

```bash
gocdnext profiles list
gocdnext profiles get --name gpu
gocdnext profiles apply --file profiles.yaml
```

The `apply` flow upserts profiles from a YAML file — same shape
the chart's `runnerProfiles:` value uses. Useful for keeping
profile definitions in version control alongside infra-as-code.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Generic error (network, parse, validation) |
| `2` | Run failed (when waiting on a triggered run) |
| `3` | Run cancelled |
| `64` | Usage error (bad flags, missing required args) |
| `77` | Permission denied (token's role insufficient) |

## Shell completion

```bash
# bash
source <(gocdnext completion bash)

# zsh
source <(gocdnext completion zsh)

# fish
gocdnext completion fish | source
```

Persisted: drop the output into the standard completion paths
(`/etc/bash_completion.d/gocdnext`, `~/.zsh/_gocdnext`, etc.).
