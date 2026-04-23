#!/usr/bin/env bash
# Runs once the first time a devcontainer comes up (Codespaces, Gitpod via
# devcontainer support, or local VS Code Dev Containers). Idempotent — safe
# to re-run.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

echo "[post-create] seeding .env from .env.example (if missing)"
if [ ! -f .env ]; then
  cp .env.example .env
fi

# Point the server's public base at the cloud host when we're inside
# GitHub Codespaces so webhook callbacks resolve against the workspace's
# forwarded URL instead of localhost. Codespaces exposes both
# CODESPACE_NAME + GITHUB_CODESPACES_PORT_FORWARDING_DOMAIN; when the
# latter is missing we fall back to a best-guess domain that matches the
# current scheme. No-op outside Codespaces.
if [ -n "${CODESPACE_NAME:-}" ]; then
  domain="${GITHUB_CODESPACES_PORT_FORWARDING_DOMAIN:-app.github.dev}"
  base="https://${CODESPACE_NAME}-8153.${domain}"
  echo "[post-create] rewriting GOCDNEXT_PUBLIC_BASE -> ${base}"
  # sed -i behaves the same on Debian/Ubuntu which the base image is.
  sed -i "s|^GOCDNEXT_PUBLIC_BASE=.*|GOCDNEXT_PUBLIC_BASE=${base}|" .env
  sed -i "s|^GOCDNEXT_API_URL=.*|GOCDNEXT_API_URL=${base}|" .env
  sed -i "s|^NEXT_PUBLIC_API_URL=.*|NEXT_PUBLIC_API_URL=${base}|" .env
fi

# Codespaces / local Dev Containers install Node via the devcontainer
# feature; Gitpod ignores those features and spins the workspace from
# the base image alone. Detect + install on demand so both paths end
# up with Node 22 + corepack on PATH.
if ! command -v node >/dev/null 2>&1; then
  echo "[post-create] Node not found — installing Node.js 22"
  curl -fsSL https://deb.nodesource.com/setup_22.x | sudo -E bash -
  sudo apt-get install -y nodejs
fi

echo "[post-create] installing dev CLIs (air, goose)"
# Use go install so the tools live in $GOPATH/bin (already on PATH in the
# MS go devcontainer). Versions are pinned so first-time boot stays
# reproducible; Renovate can bump these.
go install github.com/air-verse/air@v1.62.0
go install github.com/pressly/goose/v3/cmd/goose@v3.22.1

echo "[post-create] enabling corepack + preparing pnpm"
corepack enable
( cd web && corepack prepare --activate )

echo "[post-create] pnpm install (web)"
( cd web && pnpm install --frozen-lockfile )

echo "[post-create] building plugin images"
# Skip if the docker daemon isn't ready yet (the docker-in-docker feature
# sometimes lags); next `make plugins` picks it up.
if docker info >/dev/null 2>&1; then
  make plugins || echo "[post-create] plugin build failed — run \`make plugins\` manually after the docker daemon settles"
else
  echo "[post-create] docker daemon not ready yet — run \`make plugins\` before triggering ci-web"
fi

cat <<'EOF'

[post-create] done

Next steps:
  • make dev        — boot postgres + server + agent + web with hot reload
  • make plugins    — (re)build plugin images like gocdnext/node

Webhook testing:
  • Codespaces — forwarded port 8153 shows up under PORTS; set visibility
    to Public with `gh codespace ports visibility 8153:public` (or right-
    click the port in VS Code). The .devcontainer already points
    GOCDNEXT_PUBLIC_BASE at your workspace URL so GitHub webhooks land.
  • Gitpod — `visibility: public` in .gitpod.yml handles this
    automatically; the preview panel opens on `make dev`.

EOF
