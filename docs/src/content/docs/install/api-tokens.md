---
title: API tokens & service accounts
description: Per-user tokens for the CLI + service accounts for machine-to-machine automation. Both authenticate via Bearer header against the same /api/v1/* surface.
---

gocdnext exposes its REST API through two parallel identity flows:

- **Per-user API tokens** — minted by an authenticated user, scoped
  to that user's role. Use for the CLI on a developer laptop or
  short-lived scripts. Issued at `/settings/api-tokens` in the
  dashboard.
- **Service accounts** — machine identities that don't belong to
  any one user. Independent role; survive when the creator leaves.
  Each SA holds N tokens for rotation. Issued at
  `/admin/service-accounts` (admin-only).

Both flows produce a single token format (`gnk_<base32>` for users,
`gnk_sa_<base32>` for service accounts) and both authenticate via
`Authorization: Bearer <token>`. The platform's bearer middleware
checks the header before falling back to the session cookie path.

## Per-user tokens

### Create

1. Sign in to the dashboard.
2. *Settings → API tokens → New token*.
3. Pick a name (purpose-tagged works best — `laptop`, `ci-script`).
4. Optionally set an expiry.
5. **Copy the plaintext immediately** — it's shown once. The
   platform stores SHA-256 of the body; if you lose the
   plaintext, mint a new one.

### Use

```bash
export GOCDNEXT_TOKEN=gnk_...
gocdnext apply --slug myapp .

# OR direct REST:
curl -H "Authorization: Bearer gnk_..." \
  https://ci.example.com/api/v1/projects
```

### Revoke

*Settings → API tokens → Trash icon next to the token*. Effective
immediately — every subsequent request with that token gets 401.

Tokens carry your role at the time of authentication. If your role
changes, every token reflects the new role on its next use — no
rotation needed.

## Service accounts

### When to use

- CI orchestrating gocdnext (Argo, Tekton, Jenkins, GitHub
  Actions calling `gocdnext run`).
- Deploy bots that don't have a human identity.
- Synthetic monitoring that polls `/healthz` from outside the
  cluster.
- Scripts that need to outlive the engineer who wrote them.

### Create

1. *Admin → Service accounts → New service account*.
2. Name (e.g. `ci-bot`, `terraform-deploy`).
3. Description (what does it do, who owns it).
4. Role — pick the lowest that does the job. **Maintainer covers
   most CI use cases** (apply pipelines, trigger runs, manage
   project secrets). Admin only when the SA actually needs to
   touch users / global secrets / runner profiles.
5. Click *Create*.

The SA exists but has zero tokens. Click *New token* on the row
to mint one. Same show-once dialog as the per-user flow.

### Multiple tokens per SA

Rotation without downtime:

1. Create token #2 (`primary-rotation-2026q2`).
2. Update every consumer to use token #2.
3. Verify nothing's still hitting token #1 (`Last used` field
   stops advancing).
4. Revoke token #1.

This is the canonical zero-downtime pattern; the multi-token
shape exists specifically to support it.

### Disable vs delete

- **Disable** stops the SA from authenticating. Existing tokens
  are kept; can be re-enabled later. Use this for "we suspect
  this is compromised but want to investigate before destroying
  evidence".
- **Delete** removes the SA + cascades to all its tokens. Use
  for "this thing is retired, drop it".

### Audit

Every token use writes to `audit_events` with the actor labelled
`<sa-name>@service-account` so SA activity is grep-able vs human
activity. *Admin → Audit log* surfaces this.

## Bearer header semantics

The middleware checks Bearer **before** the session cookie. So:

- A request with both a valid Bearer token AND a session cookie
  authenticates as the Bearer subject.
- A request with an invalid Bearer (revoked, expired, malformed)
  falls through to the cookie path.
- A request with only a cookie works as it always did.

Never put both a user token and a session cookie on the same
request unless you mean it — the Bearer wins, but the audit log
shows the Bearer subject as the actor, which can confuse
operators reading the trail later.

## Token format reference

| Form | Example | Where it comes from |
|---|---|---|
| User | `gnk_abcd1234...` | `/settings/api-tokens` |
| SA   | `gnk_sa_efgh5678...` | `/admin/service-accounts` |

The `gnk_` / `gnk_sa_` prefix lets log scanners + secret scrapers
identify gocdnext tokens specifically. If a token leaks to a
public log, the prefix is what surfaces it for revocation.

The body is 32 bytes of `crypto/rand`, base32-encoded. Total
length: ~57 chars (user) or ~60 chars (SA). The first 8 chars of
the body are stored in the clear as the token's `prefix` field —
that's what the audit log shows ("the token starting with
`abcd1234` was used by alice"), but it's not enough to
reconstruct the secret.

The plaintext is **never persisted**. The platform stores
SHA-256(body). Lookup is by hash; if you lose the plaintext,
there's no recovery path — mint a new one.

## When auth is disabled (`auth.enabled=false`)

Bearer middleware short-circuits when auth is globally off — the
API stays open. Tokens still validate but they're not required.
This is useful for local dev (`make dev` ships with auth off);
never run production this way.

## Common pitfalls

- **Pasting the token into shell history**: use `read -s` +
  `gocdnext secret set --project myapp X=-` (with `-` reading
  from stdin) to avoid leaving the token in `~/.bash_history`.
- **Mixing user + SA tokens in one workflow**: a user token tied
  to a maintainer who then leaves the company breaks every
  pipeline that referenced it. Use SA tokens for anything
  durable.
- **Long-lived tokens without expiry**: the platform allows
  no-expiry tokens but treats them as a soft warning. Set
  expiry on tokens that don't need to live forever — even one
  year is dramatically better than no expiry for the leak case.
- **SA `admin` role for convenience**: only escalate to admin
  when the SA actually needs to touch admin-only routes.
  Maintainer covers ~95% of CI automation. The audit log
  reflects every action; an over-privileged SA's mistakes are
  more painful to triage.
