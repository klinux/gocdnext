#!/usr/bin/env bash
# dev.sh — boots the full local stack for gocdnext.
#   postgres via docker compose
#   server + agent via air (hot reload on .go changes)
#   web via pnpm dev (Next.js already hot reloads)
# Ctrl-C tears everything down; `make stop` does the same remotely.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

# ---- .env (optional; shell-exported vars still win) ------------------
# `set -a` auto-exports every assignment until `set +a`, so variables
# declared in .env reach child processes (server, agent, web, goose)
# without us listing them one by one. Missing file = silent no-op so
# first-time clones still work with the defaults below.
if [[ -f "$REPO_ROOT/.env" ]]; then
  echo "[dev] loading .env"
  set -a
  # shellcheck disable=SC1091
  source "$REPO_ROOT/.env"
  set +a
fi

# ---- defaults (override by exporting or via .env) --------------------

export GOCDNEXT_DATABASE_URL="${GOCDNEXT_DATABASE_URL:-postgres://gocdnext:gocdnext@localhost:5432/gocdnext?sslmode=disable}"
export GOCDNEXT_HTTP_ADDR="${GOCDNEXT_HTTP_ADDR:-:8153}"
export GOCDNEXT_GRPC_ADDR="${GOCDNEXT_GRPC_ADDR:-:8154}"
export GOCDNEXT_LOG_LEVEL="${GOCDNEXT_LOG_LEVEL:-debug}"

export GOCDNEXT_ARTIFACTS_BACKEND="${GOCDNEXT_ARTIFACTS_BACKEND:-filesystem}"
export GOCDNEXT_ARTIFACTS_FS_ROOT="${GOCDNEXT_ARTIFACTS_FS_ROOT:-$REPO_ROOT/.dev/artifacts}"
export GOCDNEXT_ARTIFACTS_PUBLIC_BASE="${GOCDNEXT_ARTIFACTS_PUBLIC_BASE:-http://localhost:8153}"
# Fixed dev key so signed URLs survive server restart — do NOT reuse in prod.
export GOCDNEXT_ARTIFACTS_SIGN_KEY="${GOCDNEXT_ARTIFACTS_SIGN_KEY:-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}"

export GOCDNEXT_SERVER_ADDR="${GOCDNEXT_SERVER_ADDR:-localhost:8154}"
export GOCDNEXT_AGENT_NAME="${GOCDNEXT_AGENT_NAME:-dev}"
export GOCDNEXT_AGENT_TOKEN="${GOCDNEXT_AGENT_TOKEN:-dev-token}"
export GOCDNEXT_AGENT_TAGS="${GOCDNEXT_AGENT_TAGS:-docker}"
export GOCDNEXT_AGENT_CAPACITY="${GOCDNEXT_AGENT_CAPACITY:-2}"

export GOCDNEXT_API_URL="${GOCDNEXT_API_URL:-http://localhost:8153}"

# ---- bootstrap ------------------------------------------------------

# Port collision guard. If 8153 / 8154 / 3000 are already bound, Next.js
# silently hops to another port and the user ends up staring at a stale
# server they forgot to kill. Fail loud instead.
check_port_free() {
  local port="$1" label="$2"
  if ss -ltn "( sport = :$port )" 2>/dev/null | tail -n +2 | grep -q .; then
    echo "[dev] port $port ($label) is already in use." >&2
    echo "[dev] process: $(ss -ltnp "( sport = :$port )" 2>/dev/null | tail -n +2 | head -1)" >&2
    echo "[dev] run \`make stop\`, or kill the offending process, and retry." >&2
    exit 1
  fi
}
check_port_free 8153 "server HTTP"
check_port_free 8154 "server gRPC"
check_port_free 3000 "web"

mkdir -p .dev/artifacts .dev/logs .dev/tmp/server .dev/tmp/agent
PIDS_FILE="$REPO_ROOT/.dev/pids"
: > "$PIDS_FILE"

cleanup() {
  echo
  echo "[dev] stopping..."
  if [[ -f "$PIDS_FILE" ]]; then
    while read -r pid; do
      [[ -z "$pid" ]] && continue
      # Kill the whole process group so air's child binary and
      # pnpm's webpack workers go down too.
      kill -TERM "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
    done < "$PIDS_FILE"
    sleep 1
    while read -r pid; do
      [[ -z "$pid" ]] && continue
      kill -KILL "-$pid" 2>/dev/null || true
    done < "$PIDS_FILE"
    rm -f "$PIDS_FILE"
  fi
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

# 1. Postgres
echo "[dev] starting postgres..."
docker compose up -d postgres >/dev/null

echo -n "[dev] waiting for postgres"
for _ in {1..60}; do
  if docker compose exec -T postgres pg_isready -U gocdnext >/dev/null 2>&1; then
    echo " ok"
    break
  fi
  echo -n "."
  sleep 0.5
done

# 2. Migrations
echo "[dev] running migrations..."
(cd server && goose -dir migrations postgres "$GOCDNEXT_DATABASE_URL" up >/dev/null)

# 3. Seed dev agent (idempotent)
TOKEN_HASH=$(printf '%s' "$GOCDNEXT_AGENT_TOKEN" | sha256sum | awk '{print $1}')
docker compose exec -T postgres \
  psql -U gocdnext -d gocdnext -v ON_ERROR_STOP=1 >/dev/null <<SQL
INSERT INTO agents (name, token_hash)
VALUES ('$GOCDNEXT_AGENT_NAME', '$TOKEN_HASH')
ON CONFLICT (name) DO UPDATE SET token_hash = EXCLUDED.token_hash;
SQL
echo "[dev] seeded agent name=$GOCDNEXT_AGENT_NAME token=$GOCDNEXT_AGENT_TOKEN"

# 4. Start processes. setsid puts each in its own process group so we
# can nuke the whole tree (air -> go binary, pnpm -> next -> webpack).
start() {
  local name="$1"; shift
  local logfile=".dev/logs/$name.log"
  setsid "$@" > "$logfile" 2>&1 < /dev/null &
  local pid=$!
  echo "$pid" >> "$PIDS_FILE"
  echo "[dev] $name pid=$pid log=$logfile"
}

start server bash -c "cd $REPO_ROOT/server && air"
start web    bash -c "cd $REPO_ROOT/web    && pnpm dev"

# Agent dials the server's gRPC port on boot — if it starts before
# air finishes the first build it dies immediately. Wait for the
# HTTP healthz (cheap proxy for "process is up + listeners bound")
# before starting the agent.
echo -n "[dev] waiting for server to be healthy"
for _ in {1..60}; do
  if curl -sf -o /dev/null http://localhost${GOCDNEXT_HTTP_ADDR}/healthz 2>/dev/null; then
    echo " ok"
    break
  fi
  echo -n "."
  sleep 0.5
done
start agent bash -c "cd $REPO_ROOT/agent && air"

echo
echo "[dev] ready:"
echo "  server  http://localhost:8153    (logs: .dev/logs/server.log)"
echo "  grpc    localhost:8154"
echo "  web     http://localhost:3000    (logs: .dev/logs/web.log)"
echo "  artifact root: $GOCDNEXT_ARTIFACTS_FS_ROOT"
echo "  agent   name=$GOCDNEXT_AGENT_NAME  token=$GOCDNEXT_AGENT_TOKEN"
echo
echo "Ctrl-C to stop (or run \`make stop\` from another shell)."
echo

# 5. Block until signal / child dies. `wait -n` returns when any of the
# backgrounded children exit — useful to catch a crash early.
while :; do
  if ! wait -n; then
    echo "[dev] a child process exited; tearing down"
    break
  fi
done
