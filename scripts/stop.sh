#!/usr/bin/env bash
# stop.sh — tear down what dev.sh started.
# Kills process groups from .dev/pids, then docker compose stop postgres.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

PIDS_FILE=".dev/pids"

if [[ -f "$PIDS_FILE" ]]; then
  while read -r pid; do
    [[ -z "$pid" ]] && continue
    kill -TERM "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  done < "$PIDS_FILE"
  sleep 1
  while read -r pid; do
    [[ -z "$pid" ]] && continue
    kill -KILL "-$pid" 2>/dev/null || true
  done < "$PIDS_FILE"
  rm -f "$PIDS_FILE"
  echo "[stop] killed server/agent/web"
else
  echo "[stop] no PID file at $PIDS_FILE (nothing to kill)"
fi

docker compose stop postgres >/dev/null 2>&1 && echo "[stop] postgres stopped" || true
