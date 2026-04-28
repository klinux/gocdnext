---
title: Authentication
description: Wire SSO via GitHub, Google, Keycloak, or any OIDC provider. Uses the same Helm-friendly secret pattern across all four.
---

gocdnext ships with four built-in identity providers behind a
single `auth.*` block in the Helm chart. All four follow the same
shape — `clientID` inline, `clientSecret` either inline or from
an existing Kubernetes Secret. Auth is **off by default**; flip
`auth.enabled=true` to gate /auth and protected routes.

## Common settings

```yaml
auth:
  enabled: true
  # First-login auto-promotion. Anyone whose login email is on this
  # list gets the admin role; everyone else lands as viewer (or
  # whatever the role-default is in your `auth_providers` admin
  # config).
  adminEmails: ["alice@example.com"]
  # Restrict logins to these email domains. Empty list = any
  # domain accepted.
  allowedDomains: ["example.com"]
```

`adminEmails` is one-shot — once a user exists, role changes happen
through the *Settings → Users* admin UI, not via this list.
`allowedDomains` is checked on every login.

## GitHub

Useful when your org already manages identity in GitHub. The
provider is OAuth-only (no GitHub App).

### 1. Create the OAuth app

In GitHub: *Settings → Developer settings → OAuth Apps → New OAuth App*.

| Field | Value |
|---|---|
| Application name | gocdnext |
| Homepage URL | `https://ci.example.com/` |
| Authorization callback URL | `https://ci.example.com/api/v1/auth/oauth/github/callback` |

Save → note the **Client ID** + generate a **Client secret**.

### 2. Wire it via Helm

Inline secret (small deploys):

```yaml
auth:
  enabled: true
  github:
    clientID: Iv1.abc123def456
    clientSecret:
      value: <secret-from-github>
```

Or via an externally-managed Secret (recommended for production):

```bash
kubectl -n gocdnext create secret generic gocdnext-github-oauth \
  --from-literal=AUTH_GITHUB_CLIENT_SECRET='<secret-from-github>'
```

```yaml
auth:
  enabled: true
  github:
    clientID: Iv1.abc123def456
    clientSecret:
      existingSecret: gocdnext-github-oauth
```

The chart wires `GOCDNEXT_AUTH_GITHUB_CLIENT_SECRET` from the Secret's
`AUTH_GITHUB_CLIENT_SECRET` key automatically.

### 3. GitHub Enterprise

Add `apiBase:` pointing at your GHE API host:

```yaml
auth:
  github:
    apiBase: https://github.example.com/api/v3
    ...
```

## Google

Production-friendly when your org runs Google Workspace.

### 1. Create OAuth credentials

In Google Cloud Console: *APIs & Services → Credentials → Create
Credentials → OAuth client ID*.

| Field | Value |
|---|---|
| Application type | Web application |
| Authorized redirect URI | `https://ci.example.com/api/v1/auth/oauth/google/callback` |

Save → note the **Client ID** + **Client secret**.

If your org enforces it, also configure the *OAuth consent screen*
with your domain.

### 2. Wire it

```yaml
auth:
  enabled: true
  allowedDomains: ["example.com"]   # restrict to your Workspace domain
  google:
    clientID: 123456789.apps.googleusercontent.com
    clientSecret:
      existingSecret: gocdnext-google-oauth
    # `issuer:` is optional; the binary defaults to
    # https://accounts.google.com which is right for ~99% of
    # deployments. Override only when running against a custom
    # Google identity broker.
```

`allowedDomains` is the gate that locks the deployment to your
Workspace — without it, ANY Google user can sign in.

## Keycloak

For enterprise IDPs running Keycloak (RH SSO).

### 1. Create the realm client

In Keycloak admin: *Realms → <your realm> → Clients → Create*.

| Field | Value |
|---|---|
| Client ID | `gocdnext` |
| Client Protocol | `openid-connect` |
| Access Type | `confidential` |
| Valid Redirect URIs | `https://ci.example.com/api/v1/auth/oauth/keycloak/callback` |

Save → grab **Credentials → Secret**.

### 2. Wire it

```yaml
auth:
  enabled: true
  keycloak:
    clientID: gocdnext
    clientSecret:
      existingSecret: gocdnext-keycloak-oauth
    issuer: https://kc.example.com/realms/internal
```

The `issuer:` is what tells the OIDC client where the realm's
`.well-known/openid-configuration` lives. Format:
`<keycloak-base>/realms/<realm-name>`.

If your Keycloak runs behind a custom path (`/auth/`), include it:
`https://idp.example.com/auth/realms/internal`.

## Generic OIDC

Last resort for any provider that speaks OIDC discovery. Auth0,
Okta, Azure AD, Authentik, Ory Hydra — all fit here.

### 1. Configure on the IdP side

Whatever the provider's UI is: create an OIDC application, add
the callback URL `https://ci.example.com/api/v1/auth/oauth/oidc/callback`,
note the client ID + secret + issuer URL.

The issuer URL is whatever the provider tells you to use for
discovery. Usually `https://idp.example.com` (no trailing slash).
The platform fetches `<issuer>/.well-known/openid-configuration`
on boot.

### 2. Wire it

```yaml
auth:
  enabled: true
  oidc:
    clientID: gocdnext
    clientSecret:
      existingSecret: gocdnext-oidc-oauth
    issuer: https://idp.example.com
    name: "Corp SSO"          # button label in the login UI
```

`name:` is what appears on the login button — leave empty to
default to "OIDC".

## Multiple providers at once

The blocks are independent — set as many as you want and the login
UI shows one button per provider:

```yaml
auth:
  enabled: true
  google:
    clientID: ...
    clientSecret: { existingSecret: gocdnext-google-oauth }
  keycloak:
    clientID: gocdnext
    issuer: https://kc.example.com/realms/internal
    clientSecret: { existingSecret: gocdnext-keycloak-oauth }
```

Login page renders both buttons. A user signs in with whichever
matches their identity; user records are keyed by `(provider, sub)`
so the same person logging in via two providers shows up as two
users — that's a deliberate audit-trail choice.

## After login

- First user with an `adminEmails` match becomes admin
  automatically.
- Other users land as viewers; promote via *Settings → Users → Edit*.
- Sessions are HMAC-signed cookies, 24h TTL by default.
  `GOCDNEXT_SECRET_KEY` (also wired via the chart) signs them —
  rotate it to invalidate every active session.
- Logout flushes the session locally; the IdP's session is left
  alone (no cross-provider single-logout).

## Testing without a real IdP

`auth.enabled=false` keeps every route open. Useful for local
dev. Don't ever flip this in production — anyone on the network
becomes an admin.

For local OIDC testing, [Authentik](https://goauthentik.io/) and
[Dex](https://dexidp.io/) both run as a single container and
implement enough OIDC to exercise the platform.
