#!/usr/bin/env python3
"""Tests for the plugin Trivy gate comparator. Run: python3 trivy_gate_test.py
(needs pyyaml). Covers the policy matrix: CRITICAL coverage, expiry, observe vs
enforce for HIGH, obsolete entries, and baseline validation."""
from __future__ import annotations

import json
import pathlib
import sys
import tempfile

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import trivy_gate as g  # noqa: E402

TODAY = "2026-11-07"
FUTURE = "2027-02-05"
PAST = "2026-11-01"

_fails = 0


def check(name: str, cond: bool, extra: str = "") -> None:
    global _fails
    if cond:
        print(f"PASS: {name}")
    else:
        print(f"FAIL: {name} {extra}")
        _fails = 1


def _exc(expires: str, cls: str = "upstream-bound", *, targets=None,
         cve="CVE-1", package="pkg") -> dict:
    return {
        "targets": targets or ["docker"], "cve": cve, "package": package,
        "origin": "gobinary", "class": cls, "justification": "j", "mitigation": "m",
        "advisory": "https://a", "upstream_issue": "https://u", "approved_pr": 1,
        "owner": "o", "expires": expires,
    }


def _baseline(exceptions: list[dict]) -> list[dict]:
    """Round-trip through load_baseline so entries carry expires_date + validation."""
    import yaml
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as fh:
        yaml.safe_dump({"version": 1, "exceptions": exceptions}, fh)
        path = pathlib.Path(fh.name)
    try:
        return g.load_baseline(path)
    finally:
        path.unlink()


def _find(cve: str, sev: str, package: str = "pkg") -> dict:
    return {"cve": cve, "package": package, "severity": sev, "fixed": "", "installed": "x"}


def ev(targets, exceptions, *, high_mode="observe", today=TODAY):
    return g.evaluate(targets, _baseline(exceptions), today=__import__("datetime").date.fromisoformat(today), high_mode=high_mode)


# 1. CRITICAL with no exception -> fail
r = ev({"docker": [_find("CVE-X", "CRITICAL")]}, [])
check("critical no exception fails", any("CRITICAL with no exception" in f for f in r["failures"]))

# 2. CRITICAL with valid emergency exception -> matched, no failure
r = ev({"docker": [_find("CVE-1", "CRITICAL")]}, [_exc(FUTURE, "emergency")])
check("critical + valid emergency passes", not r["failures"], str(r["failures"]))
check("critical + emergency is matched", len(r["matched"]) == 1)

# 3. CRITICAL with non-emergency exception -> fail
r = ev({"docker": [_find("CVE-1", "CRITICAL")]}, [_exc(FUTURE, "upstream-bound")])
check("critical + non-emergency fails", any("needs an `emergency`" in f for f in r["failures"]))

# 4. HIGH not in baseline, observe -> warn only
r = ev({"docker": [_find("CVE-9", "HIGH")]}, [], high_mode="observe")
check("high unbaselined observe: no fail", not r["failures"])
check("high unbaselined observe: warns", any("HIGH not in baseline" in w for w in r["warnings"]))

# 5. HIGH not in baseline, enforce -> fail
r = ev({"docker": [_find("CVE-9", "HIGH")]}, [], high_mode="enforce")
check("high unbaselined enforce: fails", any("HIGH not in baseline" in f for f in r["failures"]))

# 6. HIGH with valid exception -> matched
r = ev({"docker": [_find("CVE-1", "HIGH")]}, [_exc(FUTURE)], high_mode="enforce")
check("high + valid exception passes", not r["failures"], str(r["failures"]))

# 7. expired exception matching a finding -> fail
r = ev({"docker": [_find("CVE-1", "HIGH")]}, [_exc(PAST)])
check("expired + finding fails", any("EXPIRED" in f for f in r["failures"]))

# 8. expired exception, finding gone -> still fail (cleanup)
r = ev({"docker": []}, [_exc(PAST)])
check("expired + no finding fails", any("EXPIRED" in f for f in r["failures"]))

# 9. obsolete (valid, no finding) -> warn, pass
r = ev({"docker": []}, [_exc(FUTURE)])
check("obsolete valid: no fail", not r["failures"])
check("obsolete valid: warns", any("obsolete exception" in w for w in r["warnings"]))

# 10. multi-target exception covers each listed target
r = ev({"docker": [_find("CVE-1", "HIGH")], "buildx": [_find("CVE-1", "HIGH")]},
       [_exc(FUTURE, targets=["docker", "buildx"])], high_mode="enforce")
check("multi-target exception covers all", not r["failures"] and len(r["matched"]) == 2, str(r))

# 11. CRITICAL + emergency but EXPIRED -> fail (expiry wins)
r = ev({"docker": [_find("CVE-1", "CRITICAL")]}, [_exc(PAST, "emergency")])
check("expired emergency still fails critical", any("EXPIRED" in f for f in r["failures"]))

# --- baseline validation (hard FAIL) ---
def expect_invalid(name: str, exceptions, mutate=None):
    import yaml
    doc = {"version": 1, "exceptions": exceptions}
    if mutate:
        mutate(doc)
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as fh:
        yaml.safe_dump(doc, fh)
        path = pathlib.Path(fh.name)
    try:
        g.load_baseline(path)
        check(name, False, "expected BaselineError")
    except g.BaselineError:
        check(name, True)
    finally:
        path.unlink()

bad = _exc(FUTURE)
del bad["justification"]
expect_invalid("missing field rejected", [bad])
expect_invalid("wrong version rejected", [_exc(FUTURE)], mutate=lambda d: d.update(version=2))
expect_invalid("bad class rejected", [_exc(FUTURE, "whatever")])
expect_invalid("bad date rejected", [{**_exc(FUTURE), "expires": "not-a-date"}])
expect_invalid("duplicate key rejected", [_exc(FUTURE), _exc(FUTURE)])

# a well-formed baseline loads and carries expires_date
ok = _baseline([_exc(FUTURE)])
check("valid baseline loads", len(ok) == 1 and hasattr(ok[0]["expires_date"], "year"))

# --- end-to-end main(): exit codes + summary written ---
def run_main(findings_by_target, exceptions, high_mode="observe"):
    import yaml
    d = pathlib.Path(tempfile.mkdtemp())
    (d / "baseline.yaml").write_text(yaml.safe_dump({"version": 1, "exceptions": exceptions}))
    res = d / "results"
    res.mkdir()
    for tgt, finds in findings_by_target.items():
        (res / f"{tgt}.json").write_text(json.dumps(
            {"Results": [{"Target": tgt, "Vulnerabilities": [
                {"VulnerabilityID": f["cve"], "PkgName": f["package"],
                 "Severity": f["severity"], "FixedVersion": "", "InstalledVersion": "x"}
                for f in finds]}]}))
    summary = d / "summary.md"
    rc = g.main(["--baseline", str(d / "baseline.yaml"), "--results-dir", str(res),
                 "--high-mode", high_mode, "--today", TODAY,
                 "--summary-file", str(summary), "--report-file", str(d / "r.json")])
    return rc, summary.read_text()

rc, summ = run_main({"docker": [_find("CVE-1", "HIGH")]}, [_exc(FUTURE)])
check("main exit 0 on clean", rc == 0 and "PASS" in summ, f"rc={rc}")
rc, _ = run_main({"docker": [_find("CVE-Z", "CRITICAL")]}, [])
check("main exit 1 on critical", rc == 1, f"rc={rc}")


def run_main_expect(findings_by_target, exceptions, expect):
    import yaml
    d = pathlib.Path(tempfile.mkdtemp())
    (d / "baseline.yaml").write_text(yaml.safe_dump({"version": 1, "exceptions": exceptions}))
    res = d / "results"; res.mkdir()
    for tgt, finds in findings_by_target.items():
        (res / f"{tgt}.json").write_text(json.dumps(
            {"Results": [{"Vulnerabilities": [
                {"VulnerabilityID": f["cve"], "PkgName": f["package"],
                 "Severity": f["severity"]} for f in finds]}]}))
    return g.main(["--baseline", str(d / "baseline.yaml"), "--results-dir", str(res),
                   "--today", TODAY, "--expect-targets", expect])

# a target that was expected but produced no results file -> hard FAIL
rc = run_main_expect({"docker": [_find("CVE-1", "HIGH")]}, [_exc(FUTURE)], "docker,buildx")
check("missing expected target fails", rc == 1, f"rc={rc}")
rc = run_main_expect({"docker": [_find("CVE-1", "HIGH")], "buildx": []},
                     [_exc(FUTURE)], "docker,buildx")
check("all expected targets present passes", rc == 0, f"rc={rc}")

if _fails:
    print("SOME FAILED")
    sys.exit(1)
print("ALL PASS")
