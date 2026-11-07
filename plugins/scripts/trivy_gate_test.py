#!/usr/bin/env python3
"""Tests for the plugin Trivy gate comparator. Run: python3 trivy_gate_test.py
(needs pyyaml). Covers the policy matrix: CRITICAL coverage, expiry, observe vs
enforce for HIGH, obsolete entries, and baseline validation."""
from __future__ import annotations

import json
import pathlib
import sys
import tempfile
from datetime import date

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import trivy_gate as g  # noqa: E402

TODAY = "2026-11-07"
FUTURE = "2027-02-05"   # TODAY + 90d: not expired, within the upstream-bound window
EMERG = "2026-11-14"    # TODAY + 7d: valid emergency window
PAST = "2026-11-01"     # expired
TD = date.fromisoformat(TODAY)

_fails = 0


def check(name: str, cond: bool, extra: str = "") -> None:
    global _fails
    if cond:
        print(f"PASS: {name}")
    else:
        print(f"FAIL: {name} {extra}")
        _fails = 1


def _exc(expires: str, cls: str = "upstream-bound", *, targets=None,
         cve="CVE-1", package="pkg", installed="v1", origin="gobinary",
         trivy_target="bin", approved_at=None) -> dict:
    return {
        "targets": targets or ["docker"], "trivy_target": trivy_target, "cve": cve,
        "package": package, "installed": installed, "origin": origin, "class": cls,
        "justification": "j", "mitigation": "m", "advisory": "https://a",
        "upstream_issue": "https://u", "approved_pr": 1, "owner": "o",
        # Default approved_at = min(expires, TODAY): <= today (never future) and,
        # for a past `expires`, equal to it (span 0). Window tests pass it
        # explicitly. String ISO dates compare correctly with min().
        "approved_at": approved_at or min(expires, TODAY), "expires": expires,
    }


def _baseline(exceptions: list[dict], unpublished=None) -> list[dict]:
    """Round-trip through load_baseline so entries carry expires_date + validation."""
    import yaml
    doc = {"version": 1, "exceptions": exceptions}
    if unpublished is not None:
        doc["unpublished"] = unpublished
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as fh:
        yaml.safe_dump(doc, fh)
        path = pathlib.Path(fh.name)
    try:
        return g.load_baseline(path, today=TD)
    finally:
        path.unlink()


def _find(cve: str, sev: str, package: str = "pkg", *, installed="v1",
          origin="gobinary", trivy_target="bin") -> dict:
    return {"cve": cve, "package": package, "severity": sev, "fixed": "",
            "installed": installed, "origin": origin, "trivy_target": trivy_target}


def ev(targets, exceptions, *, high_mode="observe", today=TODAY):
    return g.evaluate(targets, _baseline(exceptions),
                      today=date.fromisoformat(today), high_mode=high_mode)


# 1. CRITICAL with no exception -> fail
r = ev({"docker": [_find("CVE-X", "CRITICAL")]}, [])
check("critical no exception fails", any("CRITICAL with no exception" in f for f in r["failures"]))

# 2. CRITICAL with valid emergency exception -> matched, no failure
r = ev({"docker": [_find("CVE-1", "CRITICAL")]}, [_exc(EMERG, "emergency")])
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

# 12. installed-version mismatch: exception for v1 must NOT cover v999
r = ev({"docker": [_find("CVE-1", "HIGH", installed="v999")]},
       [_exc(FUTURE, installed="v1")], high_mode="enforce")
check("installed mismatch not covered", any("HIGH not in baseline" in f for f in r["failures"]), str(r))

# 13. origin/type mismatch: gobinary exception must NOT cover an npm finding
r = ev({"docker": [_find("CVE-1", "HIGH", origin="node-pkg")]},
       [_exc(FUTURE, origin="gobinary")], high_mode="enforce")
check("origin mismatch not covered", any("HIGH not in baseline" in f for f in r["failures"]))

# 14. internal-target mismatch: exception for binA must NOT cover binB
r = ev({"docker": [_find("CVE-1", "HIGH", trivy_target="binB")]},
       [_exc(FUTURE, trivy_target="binA")], high_mode="enforce")
check("trivy_target mismatch not covered", any("HIGH not in baseline" in f for f in r["failures"]))

# 15. full identity match covers it
r = ev({"docker": [_find("CVE-1", "HIGH", installed="v2", origin="gobinary", trivy_target="b")]},
       [_exc(FUTURE, installed="v2", origin="gobinary", trivy_target="b")], high_mode="enforce")
check("full identity match covers", not r["failures"], str(r))

# --- baseline validation (hard FAIL) ---
def expect_invalid(name: str, exceptions, mutate=None, unpublished=None):
    import yaml
    doc = {"version": 1, "exceptions": exceptions}
    if unpublished is not None:
        doc["unpublished"] = unpublished
    if mutate:
        mutate(doc)
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as fh:
        yaml.safe_dump(doc, fh)
        path = pathlib.Path(fh.name)
    try:
        g.load_baseline(path, today=TD)
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
# window enforcement: emergency allows <= 7 days
expect_invalid("emergency over 7 days rejected",
               [_exc("2030-01-01", "emergency", approved_at="2026-01-01")])
# upstream-bound allows <= 90 days
expect_invalid("upstream-bound over 90 days rejected",
               [_exc("2027-06-01", "upstream-bound", approved_at="2026-11-07")])
# expires before approved_at
expect_invalid("expires before approved_at rejected",
               [_exc("2026-01-01", "upstream-bound", approved_at="2026-11-07")])
# approved_at in the FUTURE is rejected (can't park a short window ahead)
expect_invalid("future approved_at rejected",
               [_exc(FUTURE, "emergency", approved_at="2099-01-01")])
# a 7-day emergency is accepted
ok7 = _baseline([_exc("2026-11-08", "emergency", approved_at="2026-11-07")])
check("7-day emergency accepted", len(ok7) == 1)

# --- unpublished block validation ---
UNPUB_OK = "2026-11-20"  # TODAY + 13d, within the 30-day unpublished window

def _unpub(target="someplugin", reason="r", expires=UNPUB_OK, approved_at=None, approved_pr=7):
    e = {"target": target, "reason": reason, "expires": expires,
         "approved_at": approved_at or min(expires, TODAY)}
    if approved_pr is not None:
        e["approved_pr"] = approved_pr
    return e

try:
    _baseline([], unpublished=[_unpub()])  # valid -> no raise
    check("valid unpublished accepted", True)
except g.BaselineError as e:
    check("valid unpublished accepted", False, str(e))
expect_invalid("unpublished missing reason rejected", [],
               unpublished=[{"target": "p", "expires": UNPUB_OK, "approved_at": TODAY, "approved_pr": 1}])
expect_invalid("unpublished missing approved_pr rejected", [],
               unpublished=[_unpub(approved_pr=None)])
expect_invalid("unpublished missing expires rejected", [],
               unpublished=[{"target": "p", "reason": "r", "approved_at": TODAY, "approved_pr": 1}])
expect_invalid("expired unpublished rejected", [],
               unpublished=[_unpub(expires=PAST, approved_at="2026-10-28")])
expect_invalid("over-window unpublished rejected", [],
               unpublished=[_unpub(expires="2027-06-01", approved_at=TODAY)])
expect_invalid("future approved_at unpublished rejected", [],
               unpublished=[_unpub(approved_at="2099-01-01")])
expect_invalid("duplicate unpublished rejected", [], unpublished=[_unpub(), _unpub()])

def unpub_targets(unpublished):
    import yaml
    with tempfile.NamedTemporaryFile("w", suffix=".yaml", delete=False) as fh:
        yaml.safe_dump({"version": 1, "exceptions": [], "unpublished": unpublished}, fh)
        p = pathlib.Path(fh.name)
    try:
        return g.load_unpublished_targets(p, today=TD)
    finally:
        p.unlink()

check("unpublished targets returned", unpub_targets([_unpub(target="foo")]) == {"foo"})

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
            {"Results": [{"Target": f["trivy_target"], "Type": f["origin"],
                          "Vulnerabilities": [
                {"VulnerabilityID": f["cve"], "PkgName": f["package"],
                 "Severity": f["severity"], "FixedVersion": "",
                 "InstalledVersion": f["installed"]}]}
                for f in finds]}))
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

# --- summary stays bounded even with thousands of warnings (GitHub 1 MiB cap) ---
big = {"failures": [f"f{i}" for i in range(5000)],
       "warnings": [f"w{i}" for i in range(5000)],
       "matched": [f"m{i}" for i in range(5000)],
       "by_target": {f"t{i}": {"failures": 1, "warnings": 1, "matched": 1} for i in range(70)}}
summary = g.render(big, high_mode="observe")
import re as _re
check("summary caps failures", len(_re.findall(r"(?m)^- f\d", summary)) <= g.SUMMARY_MAX["failures"])
check("summary caps warnings", len(_re.findall(r"(?m)^- w\d", summary)) <= g.SUMMARY_MAX["warnings"])
check("summary notes truncation", "and 4800 more" in summary or "more (see" in summary)
check("summary well under 1 MiB", len(summary.encode()) < 200_000, f"{len(summary)} bytes")
check("summary has per-target table", "Per target" in summary)

# a single pathological huge field must not blow the byte cap
huge = {"failures": ["x" * 2_000_000], "warnings": [], "matched": [], "by_target": {}}
hs = g.render(huge, high_mode="observe")
check("summary byte-capped", len(hs.encode()) <= g.SUMMARY_BYTES_MAX, f"{len(hs.encode())} bytes")
check("long line clipped", ("x" * g.SUMMARY_LINE_MAXLEN + "…") in hs or "truncated" in hs)

# --- load_findings dedupes a duplicated Trivy row; by_target == canonical ---
dup_v = {"VulnerabilityID": "CVE-DUP", "PkgName": "tar", "InstalledVersion": "1.0",
         "FixedVersion": "1.1", "Severity": "CRITICAL"}
dup_json = {"Results": [{"Target": "bin/x", "Type": "alpine",
                         "Vulnerabilities": [dup_v, dict(dup_v)]}]}  # SAME finding twice
with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
    fh.write(json.dumps(dup_json)); dpath = pathlib.Path(fh.name)
try:
    finds = g.load_findings(dpath)
finally:
    dpath.unlink()
check("load_findings dedupes identical rows", len(finds) == 1, f"got {len(finds)}")
rr = g.evaluate({"pw": finds}, _baseline([]), today=TD, high_mode="observe")
check("by_target matches canonical after dedupe",
      rr["by_target"]["pw"]["failures"] == len(rr["failures"]) == 1, str(rr["by_target"]))

# two findings differing ONLY in FixedVersion must still collapse to one
# (`fixed` is metadata, not identity) so by_target stays == canonical.
v_a = {"VulnerabilityID": "CVE-FIX", "PkgName": "libz", "InstalledVersion": "1.0",
       "FixedVersion": "1.1", "Severity": "CRITICAL"}
v_b = dict(v_a, FixedVersion="1.2")  # same vuln, different reported fix
fv_json = {"Results": [{"Target": "img (debian 12)", "Type": "debian",
                        "Vulnerabilities": [v_a, v_b]}]}
with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as fh:
    fh.write(json.dumps(fv_json)); fpath = pathlib.Path(fh.name)
try:
    fvf = g.load_findings(fpath)
finally:
    fpath.unlink()
check("dedupe ignores FixedVersion", len(fvf) == 1, f"got {len(fvf)}")
rf = g.evaluate({"os": fvf}, _baseline([]), today=TD, high_mode="observe")
check("by_target == canonical across differing FixedVersion",
      rf["by_target"]["os"]["failures"] == len(rf["failures"]) == 1, str(rf["by_target"]))

if _fails:
    print("SOME FAILED")
    sys.exit(1)
print("ALL PASS")
