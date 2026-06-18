#!/usr/bin/env python3
"""Validate a rendered Gravitee path-based API definition for endpoints
left open by the keyless default plan.

In the path-based model a request method that matches no rule falls
through to the backend, and the default plan is keyless (no API key), so
"forgetting" to restrict methods or to attach an auth policy silently
exposes the backend. Two independent checks, each gated to off/warn/block
by the caller:

  --methods  every rule under every path must declare a non-empty
             `methods` list (a missing/empty list applies to ALL methods
             by default — the footgun).
  --auth     every method a path serves must be covered by an auth policy
             (configurable via --auth-policies). A method handled only by
             a non-terminating policy (e.g. transform-headers) and no auth
             is reachable unauthenticated. `mock` is treated as
             terminating (returns a canned response, never proxies), so a
             mock-only method is NOT flagged. Heuristic — intentionally
             public paths may need warn rather than block.

Reads the definition as JSON (the entrypoint converts via `yq -o=json`)
so only the Python stdlib is needed. Exits 2 if any check at `block`
level reports a finding, else 0; warn-level findings print but pass.
"""
import argparse
import json
import sys

ALL_METHODS = {"CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "TRACE"}
# Policies that terminate the request without proxying to the backend —
# a method handled only by one of these is not "open", so the auth check
# treats it as safe.
TERMINATING_POLICIES = {"mock"}
# Rule keys that are metadata, not a policy.
META_KEYS = {"methods", "description", "enabled", "name"}


def _rule_enabled(rule):
    """A rule with `enabled: false` is not applied by the gateway, so it
    neither restricts methods nor provides auth coverage. Absent = on."""
    return rule.get("enabled", True) is not False


def _rule_methods(rule):
    """The method set a rule applies to. Missing/empty = ALL (the
    default-allow behaviour the methods check exists to catch)."""
    m = rule.get("methods")
    if isinstance(m, list) and m:
        return {str(x).upper() for x in m}
    return set(ALL_METHODS)


def _rule_policies(rule):
    return {k.lower() for k in rule.keys() if k.lower() not in META_KEYS}


def check_methods(paths):
    """Return human-readable findings for paths that leave methods open:
    a non-list value, a path with no ENABLED rules (nothing matches →
    everything proxies to the backend), or an enabled rule without an
    explicit `methods` list (applies to ALL methods)."""
    out = []
    for p, rules in paths.items():
        if not isinstance(rules, list):
            out.append(f"path '{p}' value is not a list of rules")
            continue
        enabled = [r for r in rules if isinstance(r, dict) and _rule_enabled(r)]
        if not enabled:
            out.append(f"path '{p}' has no enabled rules — all methods reach the "
                       f"backend under the keyless plan")
            continue
        for i, rule in enumerate(rules):
            if not isinstance(rule, dict) or not _rule_enabled(rule):
                continue
            m = rule.get("methods")
            if not (isinstance(m, list) and len(m) > 0):
                out.append(f"path '{p}' rule #{i} has no explicit methods — it applies "
                           f"to ALL methods by default, exposing them under the keyless plan")
    return out


def check_auth(paths, auth_policies):
    """Return findings for methods a path serves that no ENABLED auth (or
    terminating) policy covers. Disabled rules count for neither coverage
    nor exposure."""
    safe = set(auth_policies) | TERMINATING_POLICIES
    out = []
    for p, rules in paths.items():
        if not isinstance(rules, list):
            continue  # check_methods already flags a non-list path
        declared, covered = set(), set()
        for rule in rules:
            if not isinstance(rule, dict) or not _rule_enabled(rule):
                continue
            ms = _rule_methods(rule)
            declared |= ms
            if _rule_policies(rule) & safe:
                covered |= ms
        for method in sorted(declared - covered):
            out.append(f"path '{p}' method {method} has no auth policy "
                       f"({'/'.join(sorted(auth_policies))}) — reachable unauthenticated "
                       f"under the keyless plan")
    return out


def main(argv=None):
    ap = argparse.ArgumentParser()
    ap.add_argument("--methods", default="off", choices=["off", "warn", "block"])
    ap.add_argument("--auth", default="off", choices=["off", "warn", "block"])
    ap.add_argument("--auth-policies", default="oauth2,jwt,api-key")
    ap.add_argument("file")
    args = ap.parse_args(argv)

    with open(args.file, encoding="utf-8") as fh:
        defn = json.load(fh)
    paths = defn.get("paths") if isinstance(defn, dict) else None
    if not isinstance(paths, dict):
        # No path-based block (e.g. a flows-only v4 definition) — nothing
        # this check understands; pass rather than false-fail.
        return 0

    fail = False

    def emit(level, msg):
        nonlocal fail
        prefix = "ERROR" if level == "block" else "WARNING"
        sys.stderr.write(f"gocdnext/gravitee: {prefix}: {msg}\n")
        if level == "block":
            fail = True

    if args.methods != "off":
        for msg in check_methods(paths):
            emit(args.methods, msg)

    if args.auth != "off":
        auth_policies = {x.strip().lower() for x in args.auth_policies.split(",") if x.strip()}
        for msg in check_auth(paths, auth_policies):
            emit(args.auth, msg)

    return 2 if fail else 0


if __name__ == "__main__":
    sys.exit(main())
