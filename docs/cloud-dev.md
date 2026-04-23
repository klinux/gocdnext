# Cloud dev environments

gocdnext ships with ready-to-use config for **GitHub Codespaces** and
**Gitpod** so you can boot the full stack (postgres + server + agent
+ web + plugin images) from a browser and — more importantly — get
a **public URL GitHub webhooks can actually reach** without setting
up smee.io or ngrok.

## Why this matters

gocdnext is webhook-first. The `auto_register_webhook` flow only
does anything useful when the server is reachable from
`api.github.com`. On a laptop behind NAT that means running a
tunnel. Cloud dev platforms solve it natively: every forwarded port
can be flagged public and GitHub sees it.

## Open the stack

### GitHub Codespaces

[![Open in GitHub Codespaces](https://github.com/codespaces/badge.svg)](https://codespaces.new/klinux/gocdnext)

Or from any GitHub page on the repo: `.` key → "Codespaces" → "New".

### Gitpod

[![Open in Gitpod](https://img.shields.io/badge/Gitpod-ready--to--code-908a85?logo=gitpod)](https://gitpod.io/#https://github.com/klinux/gocdnext)

Or prefix any repo/branch URL with `gitpod.io/#` in the browser.

## What happens on first boot

`.devcontainer/post-create.sh` is the shared bootstrap both
platforms run. It:

1. Seeds `.env` from `.env.example` if missing.
2. On Codespaces, rewrites `GOCDNEXT_PUBLIC_BASE` /
   `GOCDNEXT_API_URL` / `NEXT_PUBLIC_API_URL` to the workspace's
   `https://<codespace>-8153.app.github.dev` URL so the control
   plane advertises an externally-reachable address.
3. Installs [`air`](https://github.com/air-verse/air) (Go hot
   reload) and [`goose`](https://github.com/pressly/goose)
   (migrations) via `go install`.
4. Enables `corepack`, pins `pnpm` from `web/package.json`'s
   `packageManager` field, runs `pnpm install --frozen-lockfile`.
5. Runs `make plugins` (best-effort — if the Docker daemon isn't
   ready yet, re-run manually once it is).

First boot takes ~2-3 min on Codespaces (image pull + features),
~30 s on Gitpod if prebuilds are warm.

## Day-to-day

```bash
make dev          # postgres + server + agent + web with hot reload
make plugins      # rebuild plugin images after editing plugins/**
make test         # go test -race ./... across all modules
```

All the same targets you'd use locally — the devcontainer is just a
box that happens to live in someone else's data center.

## Webhook flow

### Codespaces

1. Open **PORTS** in the VS Code terminal strip.
2. Right-click port `8153` → **Port Visibility** → **Public**.
   (Or from a terminal: `gh codespace ports visibility 8153:public`.)
3. The public URL is `https://<codespace>-8153.app.github.dev`.
4. On the project detail page, click **Sync from repo** — the
   GitHub App (if configured) auto-registers a webhook pointing at
   that URL. Future pushes hit it directly.

### Gitpod

`ports[].visibility: public` in `.gitpod.yml` makes port `8153`
public by default. The Gitpod terminal prints the URL on
`make dev` boot — looks like
`https://8153-<workspace>.<region>.gitpod.io`. Use that as the
`GOCDNEXT_PUBLIC_BASE` target when registering webhooks.

## Port map

Both platforms forward these automatically:

| Port | Purpose              | Visibility (Gitpod) | Notes                                      |
|------|----------------------|---------------------|--------------------------------------------|
| 3000 | web (Next.js)        | public              | the UI you click around in                 |
| 8153 | server HTTP + webhooks | public            | what GitHub webhook POSTs target           |
| 8154 | server gRPC          | private             | agent ↔ server stream; no external callers |
| 5432 | postgres             | private             | local-only, seeded by `docker compose`     |

Codespaces starts everything as private by default — you flip
`8153` to public when you want webhooks; `3000` can stay private
and you just click "Open in Browser" from the PORTS tab to preview
the UI.

## Cost / time budgets

- **Codespaces** — 60 core-hours/month free on a personal GitHub
  plan (4-core machines burn at 4×, so about 15 real hours/month).
  Pricing is [here](https://docs.github.com/en/billing/managing-billing-for-github-codespaces).
- **Gitpod** — 50 workspace-hours/month free on the hobby plan.
  Prebuilds don't count against it. [Details](https://www.gitpod.io/pricing).

For lightweight "bang on the webhook flow for 20 min and close it"
sessions either plan's free tier is plenty.

## Troubleshooting

**`docker: command not found` inside the container.** Docker-in-
docker is a devcontainer feature on Codespaces and a runtime
capability on Gitpod — both need a few seconds after the terminal
opens before `docker info` works. If it's persistent, re-run
`make plugins` once the daemon settles; the failure in post-create
is non-fatal.

**GitHub webhook delivers 502 / connection refused.** Port `8153`
isn't set to **Public** yet (Codespaces) or the server process
isn't running (`make dev` not fired, or it crashed — check
`.dev/logs/server.log`).

**`pnpm: not found` after the workspace opens.** Corepack runs in
`post-create.sh`; if you rebuilt the container without rerunning
it, just `corepack enable && (cd web && corepack prepare --activate)`
manually.

**I want to reuse my local clone / dotfiles.** Both platforms
support dotfile repos that get cloned into the workspace on boot —
Codespaces setting is in
[Settings → Codespaces → Dotfiles](https://github.com/settings/codespaces),
Gitpod's is under
[Preferences → Dotfiles](https://gitpod.io/user/preferences).
