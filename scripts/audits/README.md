# Operational audit queries

One-shot SQL queries an operator can run against a gocdnext
database to detect classes of stuck / suspicious state. Each
script is read-only; fixing the underlying issue (e.g. canceling
a stuck run) goes through the normal API paths so the cascade +
cleanup logic runs.

## How to run

```bash
# Local dev
psql "$DATABASE_URL" -f scripts/audits/stuck_runs_cyclic_needs.sql

# Production / k8s
kubectl exec -it <postgres-pod> -- psql -U gocdnext -d gocdnext \
    -f /tmp/stuck_runs_cyclic_needs.sql
```

## Available audits

| Script | Purpose |
|---|---|
| `stuck_runs_cyclic_needs.sql` | Runs stuck `queued` because of a cyclic `needs:` snapshot baked in before v0.4.36's parser-side cycle detection. See [issue #6](https://github.com/klinux/gocdnext/issues/6). |

## When to add one

A new audit script belongs here when:

- It detects a class of state that the runtime can produce but
  can't self-heal (cyclic deps, orphaned cache rows, ...).
- The action is operator-discretion, not automatic — auto-recovery
  belongs in a server-side reaper, not here.
- The query is cheap enough to run on a live primary without
  meaningful contention (single-digit ms on a healthy cluster).

Don't add scripts for state the runtime owns. If the answer is
"the server should clean this up automatically", file an issue
and build it as a reaper.
