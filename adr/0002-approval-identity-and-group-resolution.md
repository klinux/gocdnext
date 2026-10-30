# ADR 0002: Approval identity and group resolution behind an external platform / IdP

- **Status:** Accepted — phase 1 implemented (#172)
- **Date:** 2026-10-29
- **Scope:** who may decide an approval gate (job gates and ADR-0001 rollout gates),
  and where that identity and its group membership come from. Approver *policy*
  ownership is out of scope (see Open questions).

## Context

gocdnext is being embedded in an internal developer platform that fronts it for users.
The approval gate depends on two answers that the embedding makes ambiguous: **who is
the acting identity**, and **where group membership comes from**.

Before this ADR (`store/approvals.go`, `store/rollout_gate_decide.go`, two copies of the
same check):

- `approvers:` matched by **string** — display label or email. Under OIDC the `name`
  claim wins as the label (#51), and an embedding platform is one more source of
  display names that can drift.
- `approver_groups:` matched by **user id** against the local `group_members` table —
  the robust path.
- A service-account API token authenticates as a synthetic user whose id is a
  `service_accounts` row. On the job gate it could pass the allow-list (empty list, or
  an SA named like a listed approver) and then fail the vote's `users(id)` FK as a 500.

The failure mode to design against: **a platform approving on behalf of users with a
service token.** Quorum counts the service, the audit names the service, and the one
question a gate exists to answer — *which human authorised this* — has no answer. Its
twin is a **caller-asserted identity** (an `X-On-Behalf-Of` header): anyone holding the
platform's token could approve as anyone.

## Decision

1. **Approvals are human-only.** A service-account identity is refused on both engines
   (`store.ErrApproverNotHuman` → 403), including on an open gate whose empty allow-list
   means "any authenticated user". The check lives in the shared store authorizer and is
   repeated in the rollout handler — same invariant at every layer that decides.

2. **The human's own identity reaches gocdnext; nobody asserts it for them.**
   - Default: **shared OIDC**. The platform and gocdnext trust the same issuer, so the
     user's own session reaches gocdnext. Configuration, not a feature — OIDC login
     already exists. Votes keep a real user id, quorum works, the audit names the human.
   - Where the platform must proxy the API: **token exchange (RFC 8693)**. The platform
     exchanges its token for one the IdP mints *for the user*; the identity assertion is
     signed by the IdP, not by the proxy. gocdnext never accepts an identity header.
     Accepting IdP-signed bearer JWTs is a follow-up (below); until then the proxy path
     is not supported for approvals.

3. **Approvers can be named by a stable subject.** An `approvers:` entry of the form
   `sub:<provider>:<subject>` matches the user whose `users.provider` / `users.external_id`
   (the IdP `sub`, the GitHub numeric id) equal those parts — immune to rename and email
   change. Rules:
   - A `sub:` entry matches **only** by subject, never by label/email. Otherwise a user
     who can set their display name to `sub:oidc:<victim>` would satisfy the victim's
     entry.
   - Validated at parse (`domain.ValidateApproverRef`: both parts present, bounded,
     no whitespace/control characters) and matched exactly at decision time. Errors cite
     the list index, never the entry.
   - A fixed `sub:` prefix rather than a bare `<provider>:` because provider names are
     operator-chosen and unconstrained; the prefix keeps the form unambiguous against
     any plain name or email. The provider part cannot contain `:`; the subject can
     (URN-style subs).
   - Plain entries keep matching name or email — no migration of existing pipelines.

4. **Group membership is resolved at vote time; the local table stays.**
   `store.ApproverGroupSource` is an optional external source (IdP, platform) consulted
   at the moment of decision, so a membership revoked after login isn't honoured from a
   login-time token snapshot. Token claims stay fine for UI/navigation; authority is
   resolved at the decision. Order, cheapest first:
   1. listed in `approvers:`;
   2. member of a listed group in the local `group_members` table;
   3. member of a listed group per the external source.

   The source is called **before the decision transaction opens** — never with a pooled
   connection pinned or the gate's row lock held — and only when its answer can matter
   (the gate lists groups, the voter isn't named in `approvers:`, the local table didn't
   grant). The allow-list it uses is a pre-transaction read; the locked read re-evaluates
   with the resolved names as membership data only, and both gate configs are immutable
   snapshots.

   The local table is **additive**, not a fallback: standalone and air-gapped installs
   need it, and a local grant is an explicit decision by a gocdnext admin, not a mirror.
   Platforms that own teams should resolve through the source and **not** mirror into
   `group_members` — a mirror is exactly the stale copy this design avoids.

5. **Resolution failure fails closed.** If the external source errors, the vote is
   refused (`store.ErrApproverGroupsUnavailable` → 503, retryable) and nothing is
   recorded. It never falls back to a possibly-stale copy. This couples approvals that
   *need* the external source to the platform's availability; votes satisfied by
   `approvers:` or the local table don't call the source at all.

6. **Approval authority stays independent of role.** The authorizer never reads the
   user's role. The rollout approve/reject route remains viewer-level, authorised by the
   allow-list alone (ADR-0001 Phase 2).

## Open questions — resolution

- **Does the platform own approver policy, or only identity?** This ADR covers identity
  and membership only; `approvers:` / `approver_groups:` stay inline in the pipeline.
  If the platform comes to own *policy*, a gate referencing an external decision is a
  separate ADR — it changes what a gate is, not who is acting.
- **Resolution failure:** fail closed (decision 5).
- **Rollout gates:** both engines share one authorizer (`store.authorizeApprover`), so
  identity rules cannot drift between them, and decision 6 keeps the
  approval-vs-operational-authority separation.

## Consequences

- One authorizer for both engines; the job gate's service-account 500 becomes a 403.
- `sub:` entries are opaque in YAML. The UI lists them verbatim; showing the resolved
  user is a UI follow-up.
- Quorum distinctness is per `users` row. One human signed in through two providers is
  two rows — true before this ADR for groups too; linking identities across providers
  is out of scope.

## Follow-ups (not in phase 1)

- **An `ApproverGroupSource` implementation** (platform HTTP / SCIM / IdP admin API),
  wired from config, with its own timeout, metrics, and no positive-answer caching
  beyond the decision. It still adds latency to every vote that needs it, so it must
  bound that latency (timeout, circuit breaker).
- **IdP-signed bearer tokens (RFC 8693 output)** accepted by the auth middleware,
  verified against the issuer + audience and mapped to users by `(provider, sub)`.
- **UI rendering of `sub:` approvers** as the resolved user's name.
