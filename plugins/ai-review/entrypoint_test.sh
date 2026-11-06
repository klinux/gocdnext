#!/usr/bin/env bash
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

for bad in 0.5.6 .5 0. 01 1.1; do
  out="$(env PLUGIN_PROVIDER=openai OPENAI_API_KEY=dummy PLUGIN_TEMPERATURE="$bad" \
    bash "$HERE/entrypoint.sh" 2>&1)"
  rc=$?
  if [ "$rc" -ne 2 ] || ! printf '%s\n' "$out" | grep -q 'decimal in \[0, 1\]'; then
    echo "FAIL: malformed temperature '$bad' should fail validation with rc=2"
    echo "$out"
    exit 1
  fi
done

# --- pr-comment URL-input guard (curl-config injection) ---------------------
# pr-number/repo are interpolated into the SCM URL written into the curl --config
# that also holds the token. A crafted value must be rejected BEFORE any config
# is written, and curl must never run. Mock curl to a sentinel to prove it.
MOCK="$(mktemp -d)"; mkdir -p "$MOCK/home"
trap 'rm -rf "$MOCK"' EXIT
cat >"$MOCK/curl" <<EOF
#!/usr/bin/env bash
touch "$MOCK/curl-ran"
exit 0
EOF
chmod +x "$MOCK/curl"

run_ai() { # run_ai <pr_number> <repo> <scm> [workdir]
  rm -f "$MOCK/curl-ran"
  ( cd "${4:-$MOCK}" && env PATH="$MOCK:$PATH" HOME="$MOCK/home" \
      PLUGIN_PROVIDER=claude ANTHROPIC_API_KEY=dummy \
      PLUGIN_MODE=pr-comment PLUGIN_SCM_PROVIDER="$3" \
      GITHUB_TOKEN=dummy GITLAB_TOKEN=dummy \
      PLUGIN_PR_NUMBER="$1" PLUGIN_REPO="$2" \
      bash "$HERE/entrypoint.sh" >/dev/null 2>&1 )
}

# Rejected inputs: rc=2 AND curl never invoked.
NL=$'\n'
reject() { # reject <desc> <pr_number> <repo> <scm>
  run_ai "$2" "$3" "$4"; local rc=$?
  if [ "$rc" -ne 2 ]; then echo "FAIL: $1 should exit 2 (got $rc)"; exit 1; fi
  if [ -e "$MOCK/curl-ran" ]; then echo "FAIL: $1 must not invoke curl"; exit 1; fi
}
reject "pr-number quote+newline injection" "1\"${NL}header = \"X-Evil: y" "owner/repo" github
reject "repo quote+newline injection (github)" "1" "owner/repo\"${NL}header = \"X-Evil: y" github
reject "repo newline injection (gitlab)" "1" "grp/proj${NL}url = \"http://evil\"" gitlab
for bad in 0 01 -1 1.0 abc; do reject "pr-number '$bad'" "$bad" owner/repo github; done
reject "github repo no slash" 1 ownerrepo github
reject "github repo extra segment" 1 owner/repo/extra github
reject "gitlab repo single segment" 1 justname gitlab

# Valid inputs pass the guard: they reach the diff stage, which fails in a
# non-git empty dir with rc=1 (NOT the rc=2 guard), and curl stays unused.
accept() { # accept <pr_number> <repo> <scm>
  local d; d="$(mktemp -d)"
  run_ai "$1" "$2" "$3" "$d"; local rc=$?
  rm -rf "$d"
  if [ "$rc" -eq 2 ]; then echo "FAIL: valid pr=$1 repo=$2 ($3) wrongly rejected by guard"; exit 1; fi
  if [ -e "$MOCK/curl-ran" ]; then echo "FAIL: valid pr=$1 repo=$2 ($3) reached curl (diff should fail first)"; exit 1; fi
}
accept 1 owner/repo github
accept 42 my.org/my-repo github
accept 7 group/subgroup/project gitlab
accept 7 12345 gitlab

echo "PASS: ai-review entrypoint"
