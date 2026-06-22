#!/usr/bin/env bash
# Mock-PATH unit test for the kustomize plugin entrypoint. No bats — a
# plain bash harness that stubs `kustomize` and `kubectl`, then asserts
# the argv each receives and the manifest piped to apply. envsubst is the
# real binary (gettext); the one case that needs it is guarded.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# A real source tree: kustomization + a Deployment with two ${VAR}
# placeholders (quoted, as in real manifests). envsubst runs in place on
# this BEFORE build, so the stubbed `build` cats the (substituted) source
# back — exercising the real order. The Deployment also drives the wait
# workload-list assertion.
mkdir -p "$TMP/overlay"
cat >"$TMP/overlay/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [deployment.yaml]
EOF
cat >"$TMP/overlay/deployment.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      containers:
        - name: web
          env:
            - name: SECRET
              value: "${TESTVAR}"
            - name: KEEP
              value: "${OTHERVAR}"
EOF

# Stub kustomize: log every invocation; `build` cats the path's
# deployment.yaml (= the source after any in-place envsubst). The path is
# the last positional arg (after optional --enable-helm).
cat >"$TMP/kustomize" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "$TMP/kustomize.log"
if [ "\$1" = build ]; then
    for a in "\$@"; do last="\$a"; done
    cat "\$last/deployment.yaml"
fi
exit 0
EOF
chmod +x "$TMP/kustomize"

# Stub kubectl: log argv; capture piped manifests; simulate namespace
# presence and rollout success/failure via env knobs (NS_EXISTS/ROLLOUT_FAIL).
cat >"$TMP/kubectl" <<EOF
#!/usr/bin/env bash
args="\$*"
printf '%s\n' "\$args" >> "$TMP/kubectl.log"
case "\$args" in *"-f -"*) cat > "$TMP/kubectl.stdin" ;; esac
case "\$args" in
    "get namespace"*) [ "\${NS_EXISTS:-0}" = 1 ] && exit 0 || exit 1 ;;
    "rollout status"*) [ "\${ROLLOUT_FAIL:-0}" = 1 ] && exit 1 || exit 0 ;;
esac
exit 0
EOF
chmod +x "$TMP/kubectl"

run() { PATH="$TMP:$PATH" bash "$HERE/entrypoint.sh"; }
reset() { : >"$TMP/kustomize.log"; : >"$TMP/kubectl.log"; : >"$TMP/kubectl.stdin"; }
fail() {
    echo "FAIL: $1"
    echo "--- kustomize.log ---"; cat "$TMP/kustomize.log" 2>/dev/null
    echo "--- kubectl.log ---"; cat "$TMP/kubectl.log" 2>/dev/null
    exit 1
}

# ── 1. backward compat: plain apply renders + applies ──
reset
PLUGIN_PATH="$TMP/overlay" run >/dev/null 2>&1 || fail "basic apply exited nonzero"
grep -q "build .*$TMP/overlay" "$TMP/kustomize.log" || fail "kustomize build not called"
grep -q "apply" "$TMP/kubectl.log" || fail "kubectl apply not called"

# ── 2. missing path / bad action are clean 2s ──
if PLUGIN_PATH="" run >/dev/null 2>&1; then fail "missing path accepted"; fi
if PLUGIN_PATH="$TMP/overlay" PLUGIN_ACTION=nope run >/dev/null 2>&1; then fail "bad action accepted"; fi

# ── 3. images → kustomize edit set image per entry (comma + newline) ──
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_IMAGES="web=img:abc123,jmx=ex:1" run >/dev/null 2>&1 || fail "images run failed"
grep -qx "edit set image web=img:abc123" "$TMP/kustomize.log" || fail "image #1 not set"
grep -qx "edit set image jmx=ex:1" "$TMP/kustomize.log" || fail "image #2 (comma-split) not set"

# ── 4. enable_helm → build --enable-helm ──
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_ENABLE_HELM=true run >/dev/null 2>&1 || fail "enable_helm run failed"
grep -q -- "--enable-helm" "$TMP/kustomize.log" || fail "--enable-helm not passed to build"

# ── 5. validate → server dry-run, never a real apply ──
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_ACTION=validate run >/dev/null 2>&1 || fail "validate run failed"
grep -q -- "--dry-run=server" "$TMP/kubectl.log" || fail "validate didn't server-dry-run"

# ── 6. wait → rollout status on the workload parsed from the manifests,
#       in PLUGIN_NAMESPACE (the manifest has no explicit ns here) ──
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_NAMESPACE=apps PLUGIN_WAIT=true PLUGIN_WAIT_TIMEOUT=90s run >/dev/null 2>&1 \
    || fail "wait run failed"
grep -q "rollout status Deployment/web --namespace apps --timeout=90s" "$TMP/kubectl.log" \
    || fail "rollout status not called for the parsed workload in PLUGIN_NAMESPACE"

# ── 7. wait + failed rollout → job fails ──
reset
if PLUGIN_PATH="$TMP/overlay" PLUGIN_NAMESPACE=apps PLUGIN_WAIT=true ROLLOUT_FAIL=1 run >/dev/null 2>&1; then
    fail "failed rollout did not fail the job"
fi

# ── 8. ensure_namespace creates the ns when absent ──
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_NAMESPACE=apps PLUGIN_ENSURE_NAMESPACE=true NS_EXISTS=0 run >/dev/null 2>&1 \
    || fail "ensure_namespace run failed"
grep -qx "create namespace apps" "$TMP/kubectl.log" || fail "namespace not created when absent"
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_NAMESPACE=apps PLUGIN_ENSURE_NAMESPACE=true NS_EXISTS=1 run >/dev/null 2>&1 || true
grep -qx "create namespace apps" "$TMP/kubectl.log" && fail "namespace re-created when present"

# ── 9. prune requires prune_label ──
if PLUGIN_PATH="$TMP/overlay" PLUGIN_PRUNE=true run >/dev/null 2>&1; then
    fail "prune without prune_label accepted"
fi
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_PRUNE=true PLUGIN_PRUNE_LABEL="app=web" run >/dev/null 2>&1 || fail "prune run failed"
grep -q -- "--prune -l app=web" "$TMP/kubectl.log" || fail "prune flags not passed"

# ── 10. server_side → --server-side on apply ──
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_SERVER_SIDE=true run >/dev/null 2>&1 || fail "server_side run failed"
grep -q -- "--server-side" "$TMP/kubectl.log" || fail "--server-side not passed"

# ── 11. envsubst (restricted) resolves only the named var, BEFORE build,
#        preserving the source quoting (special-char safe). Real envsubst. ──
if command -v envsubst >/dev/null 2>&1; then
    # restore the source (a prior run may have substituted it in place).
    # Two listed vars (comma-split) + one unlisted that must survive.
    cat >"$TMP/overlay/deployment.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: web }
spec:
  template:
    spec:
      containers:
        - name: web
          env:
            - name: SECRET
              value: "${TESTVAR}"
            - name: EXTRA
              value: "${SECONDVAR}"
            - name: KEEP
              value: "${OTHERVAR}"
EOF
    reset
    PLUGIN_PATH="$TMP/overlay" TESTVAR="RESOLVED" SECONDVAR="RESOLVED2" OTHERVAR="NOPE" \
        PLUGIN_ENVSUBST="TESTVAR,SECONDVAR" run >/dev/null 2>&1 || fail "envsubst run failed"
    grep -q 'value: "RESOLVED"' "$TMP/kubectl.stdin" || fail "envsubst didn't resolve TESTVAR (quotes preserved?)"
    grep -q 'value: "RESOLVED2"' "$TMP/kubectl.stdin" || fail "envsubst didn't resolve the 2nd (comma-split) var"
    # the literal ${OTHERVAR} must survive — grepping for it on purpose
    # shellcheck disable=SC2016
    grep -q 'value: "${OTHERVAR}"' "$TMP/kubectl.stdin" || fail "envsubst clobbered an unlisted var"

    # ── 12. envsubst=true substitutes SET vars but PRESERVES unset ones ──
    #        (the finding: a bare envsubst would blank ${UNSETONE} to "")
    cat >"$TMP/overlay/deployment.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: web }
spec:
  template:
    spec:
      containers:
        - name: web
          env:
            - name: A
              value: "${SETVAR}"
            - name: B
              value: "${UNSETONE}"
EOF
    reset
    SETVAR="fromenv" PLUGIN_PATH="$TMP/overlay" PLUGIN_ENVSUBST=true run >/dev/null 2>&1 \
        || fail "envsubst=true run failed"
    grep -q 'value: "fromenv"' "$TMP/kubectl.stdin" || fail "envsubst=true didn't substitute a set var"
    # shellcheck disable=SC2016
    grep -q 'value: "${UNSETONE}"' "$TMP/kubectl.stdin" || fail "envsubst=true blanked an UNSET placeholder"
else
    echo "skip: envsubst not on PATH (covered in the plugin image)"
fi

# ── 13. envsubst restricted with an invalid var name fails loud ──
if PLUGIN_PATH="$TMP/overlay" PLUGIN_ENVSUBST="BAD-NAME" run >/dev/null 2>&1; then
    fail "invalid envsubst var name accepted"
fi

# ── 14. ensure_namespace without namespace fails loud ──
if PLUGIN_PATH="$TMP/overlay" PLUGIN_ENSURE_NAMESPACE=true run >/dev/null 2>&1; then
    fail "ensure_namespace without namespace accepted"
fi

# ── 15. envsubst lists a var that isn't set → fail loud (no silent blank) ──
if PLUGIN_PATH="$TMP/overlay" PLUGIN_ENVSUBST="DEFINITELY_UNSET_VAR" run >/dev/null 2>&1; then
    fail "envsubst listed an unset var without failing"
fi

# ── 16. wait honours the manifest's metadata.namespace over PLUGIN_NAMESPACE ──
cat >"$TMP/overlay/deployment.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: othns
spec:
  template:
    spec:
      containers:
        - name: web
          image: x
EOF
reset
PLUGIN_PATH="$TMP/overlay" PLUGIN_WAIT=true run >/dev/null 2>&1 || fail "wait (manifest ns) run failed"
grep -q "rollout status Deployment/web --namespace othns" "$TMP/kubectl.log" \
    || fail "rollout didn't use the manifest's metadata.namespace"

# ── 17. apply + envsubst must NOT echo the resolved secret to the log
#        (rendered-manifest dump is suppressed), yet kubectl still gets it ──
if command -v envsubst >/dev/null 2>&1; then
    cat >"$TMP/overlay/deployment.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: { name: web }
spec:
  template:
    spec:
      containers:
        - name: web
          env:
            - name: TOKEN
              value: "${MYSECRET}"
EOF
    reset
    secret="s3cr3t-value-xyz"
    out="$(PLUGIN_PATH="$TMP/overlay" MYSECRET="$secret" PLUGIN_ENVSUBST="MYSECRET" run 2>&1)" \
        || fail "apply+envsubst run failed"
    printf '%s' "$out" | grep -q "$secret" && fail "resolved secret leaked to the log"
    printf '%s' "$out" | grep -q "omitted (envsubst enabled" || fail "missing the omitted-render notice"
    grep -q "$secret" "$TMP/kubectl.stdin" || fail "apply didn't receive the resolved manifest"
fi

echo "PASS: kustomize entrypoint"
