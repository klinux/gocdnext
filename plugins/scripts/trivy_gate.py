#!/usr/bin/env python3
"""Compare Trivy results for the gocdnext plugin images against the versioned
baseline of justified exceptions, and apply the gate policy.

Policy (see plugins/SECURITY-SCANNING.md):
  * CRITICAL  -> FAIL, unless covered by a NON-EXPIRED `emergency` exception.
  * expired exception (any)            -> FAIL (must renew or remove).
  * invalid / unparseable baseline     -> FAIL.
  * HIGH not covered by a valid entry  -> observe-mode: WARN; enforce-mode: FAIL.
  * valid exception matching no finding -> WARN (obsolete; remove to avoid
    silent accumulation).

Inputs: a directory of Trivy JSON reports, one per target, named `<target>.json`
(the stem is the logical target name, e.g. `docker.json`, `external:moby-buildkit`
is written as `external-moby-buildkit.json` and mapped back via --alias). Exit 0
on pass, 1 on policy failure, 2 on usage/parse error.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import pathlib
import sys

try:
    import yaml
except ImportError:  # pragma: no cover - the workflow installs pyyaml
    print("trivy_gate: PyYAML is required (pip install pyyaml)", file=sys.stderr)
    sys.exit(2)

REQUIRED_FIELDS = (
    "targets", "trivy_target", "cve", "package", "installed", "origin", "class",
    "justification", "mitigation", "advisory", "upstream_issue", "approved_pr",
    "owner", "approved_at", "expires",
)
VALID_CLASSES = {"emergency", "high-fixable", "upstream-bound", "no-fix"}
# A CRITICAL finding may only be held open by one of these classes.
CRITICAL_CLASSES = {"emergency"}
# Maximum exception lifetime (days) per class, enforced as expires - approved_at.
# This makes the 7/14/90-day policy a HARD check, not just a review convention.
MAX_WINDOW_DAYS = {"emergency": 7, "high-fixable": 14, "upstream-bound": 90, "no-fix": 90}

# An exception's identity — everything that must match a Trivy finding for the
# exception to apply. Narrow on purpose: a different installed version, a
# different binary (Trivy's internal target) or a different package type does NOT
# inherit the exception.
IDENTITY = ("target", "trivy_target", "cve", "package", "installed", "origin")


class BaselineError(Exception):
    """Raised when the baseline file is structurally invalid -> hard FAIL."""


def _parse_date(value: object, where: str) -> dt.date:
    if not isinstance(value, str):
        raise BaselineError(f"{where}: `expires` must be a quoted YYYY-MM-DD string")
    try:
        return dt.date.fromisoformat(value)
    except ValueError as exc:
        raise BaselineError(f"{where}: bad `expires` {value!r}: {exc}") from None


def load_baseline(path: pathlib.Path) -> list[dict]:
    """Parse + validate the baseline. Returns the list of exception dicts, each
    with `targets` normalised to a list and `expires` parsed to a date."""
    try:
        raw = yaml.safe_load(path.read_text()) or {}
    except (OSError, yaml.YAMLError) as exc:
        raise BaselineError(f"cannot read/parse {path}: {exc}") from None
    if not isinstance(raw, dict) or raw.get("version") != 1:
        raise BaselineError("baseline must be a mapping with `version: 1`")
    items = raw.get("exceptions") or []
    if not isinstance(items, list):
        raise BaselineError("`exceptions` must be a list")

    out: list[dict] = []
    seen: set[tuple[str, str, str]] = set()
    for i, item in enumerate(items):
        where = f"exceptions[{i}]"
        if not isinstance(item, dict):
            raise BaselineError(f"{where}: must be a mapping")
        missing = [f for f in REQUIRED_FIELDS if f not in item or item[f] in (None, "")]
        if missing:
            raise BaselineError(f"{where}: missing required field(s): {', '.join(missing)}")
        targets = item["targets"]
        if not isinstance(targets, list) or not all(isinstance(t, str) and t for t in targets):
            raise BaselineError(f"{where}: `targets` must be a non-empty list of strings")
        if item["class"] not in VALID_CLASSES:
            raise BaselineError(
                f"{where}: class {item['class']!r} not in {sorted(VALID_CLASSES)}")
        item = dict(item)
        item["expires_date"] = _parse_date(item["expires"], where)
        item["approved_at_date"] = _parse_date(item["approved_at"], where)
        # Enforce the per-class lifetime: a CRITICAL `emergency` can't be parked
        # until 2030, and an `upstream-bound` can't exceed 90 days without a
        # fresh PR that bumps approved_at.
        span = (item["expires_date"] - item["approved_at_date"]).days
        if span < 0:
            raise BaselineError(f"{where}: expires is before approved_at")
        limit = MAX_WINDOW_DAYS[item["class"]]
        if span > limit:
            raise BaselineError(
                f"{where}: class {item['class']!r} allows <= {limit} days, "
                f"but expires - approved_at = {span} days (renew to shorten)")
        for t in targets:
            key = (t,) + tuple(str(item[f]) for f in IDENTITY[1:])
            if key in seen:
                raise BaselineError(f"{where}: duplicate exception for {key}")
            seen.add(key)
        out.append(item)
    return out


def load_findings(path: pathlib.Path) -> list[dict]:
    """Flatten a Trivy JSON report. Each finding keeps the full identity: the
    internal Trivy target (binary path / image), the package TYPE (origin), the
    installed version, plus cve/package/severity."""
    data = json.loads(path.read_text())
    findings: list[dict] = []
    for result in data.get("Results") or []:
        rtarget = result.get("Target", "")
        rtype = result.get("Type", "")
        for v in result.get("Vulnerabilities") or []:
            findings.append({
                "trivy_target": rtarget,
                "origin": rtype,
                "cve": v.get("VulnerabilityID", ""),
                "package": v.get("PkgName", ""),
                "installed": v.get("InstalledVersion", ""),
                "fixed": v.get("FixedVersion", ""),
                "severity": (v.get("Severity") or "").upper(),
            })
    return findings


def _finding_key(target: str, f: dict) -> tuple:
    """The identity tuple shared by a finding and its exception."""
    return (target, f["trivy_target"], f["cve"], f["package"], f["installed"], f["origin"])


def build_index(baseline: list[dict]) -> dict[tuple, dict]:
    idx: dict[tuple, dict] = {}
    for item in baseline:
        for t in item["targets"]:
            key = (t,) + tuple(str(item[f]) for f in IDENTITY[1:])
            idx[key] = item
    return idx


def evaluate(targets: dict[str, list[dict]], baseline: list[dict], *,
             today: dt.date, high_mode: str) -> dict:
    """Core policy. Returns a report dict with `failures`, `warnings`, `matched`."""
    index = build_index(baseline)
    used_keys: set[tuple] = set()
    failures: list[str] = []
    warnings: list[str] = []
    matched: list[str] = []

    for target in sorted(targets):
        for f in targets[target]:
            sev = f["severity"]
            if sev not in ("CRITICAL", "HIGH"):
                continue
            key = _finding_key(target, f)
            exc = index.get(key)
            tag = (f"{target} {f['cve']} {f['package']} {f['installed']} "
                   f"[{f['origin']}@{f['trivy_target'].split('/')[-1]}] ({sev})")

            if exc is None:
                if sev == "CRITICAL":
                    failures.append(f"CRITICAL with no exception: {tag}")
                elif high_mode == "enforce":
                    failures.append(f"HIGH not in baseline: {tag}")
                else:
                    warnings.append(f"HIGH not in baseline (observe): {tag}")
                continue

            used_keys.add(key)
            if exc["expires_date"] < today:
                failures.append(
                    f"exception EXPIRED {exc['expires']} (renew or remove): {tag}")
            elif sev == "CRITICAL" and exc["class"] not in CRITICAL_CLASSES:
                failures.append(
                    f"CRITICAL needs an `emergency` exception, got {exc['class']!r}: {tag}")
            else:
                matched.append(f"{tag} -> {exc['class']} until {exc['expires']}")

    # Expired entries are a hard fail even if the finding has since vanished —
    # they must be cleaned up. Non-expired entries that matched nothing are
    # obsolete noise and only warn.
    for item in baseline:
        for t in item["targets"]:
            key = (t,) + tuple(str(item[f]) for f in IDENTITY[1:])
            if key in used_keys:
                continue
            if item["expires_date"] < today:
                failures.append(
                    f"exception EXPIRED {item['expires']} (remove): "
                    f"{t} {item['cve']} {item['package']}")
            elif t in targets:
                # Only judge a non-expired entry obsolete when its target was
                # actually scanned this run — a missing scan is a separate FAIL.
                warnings.append(
                    f"obsolete exception — no matching finding, remove: "
                    f"{t} {item['cve']} {item['package']}")

    return {
        "failures": sorted(set(failures)),
        "warnings": sorted(set(warnings)),
        "matched": sorted(set(matched)),
    }


def render(report: dict, *, high_mode: str) -> str:
    lines = ["# Plugin Trivy gate", ""]
    lines.append(f"- mode: HIGH **{high_mode}**, CRITICAL **enforce**")
    lines.append(f"- matched exceptions: {len(report['matched'])}")
    lines.append(f"- warnings: {len(report['warnings'])}")
    lines.append(f"- failures: {len(report['failures'])}")
    lines.append("")
    for head, key in (("## Failures", "failures"), ("## Warnings", "warnings"),
                      ("## Matched exceptions", "matched")):
        if report[key]:
            lines.append(head)
            lines.extend(f"- {line}" for line in report[key])
            lines.append("")
    if not report["failures"]:
        lines.append("**PASS** — no policy violations.")
    else:
        lines.append("**FAIL** — see failures above.")
    return "\n".join(lines) + "\n"


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="gocdnext plugin Trivy gate")
    ap.add_argument("--baseline", required=True, type=pathlib.Path)
    ap.add_argument("--results-dir", required=True, type=pathlib.Path)
    ap.add_argument("--high-mode", choices=("observe", "enforce"), default="observe")
    ap.add_argument("--expect-targets", default="",
                    help="comma-separated targets that MUST have a results file; "
                         "a missing one is a hard FAIL (so a dropped scan can't "
                         "silently hide a CRITICAL)")
    ap.add_argument("--today", default=dt.date.today().isoformat(),
                    help="override today's date (YYYY-MM-DD) — tests only")
    ap.add_argument("--summary-file", type=pathlib.Path,
                    help="append a Markdown summary here (e.g. $GITHUB_STEP_SUMMARY)")
    ap.add_argument("--report-file", type=pathlib.Path,
                    help="write the full JSON report here (CI artifact)")
    args = ap.parse_args(argv)

    try:
        baseline = load_baseline(args.baseline)
    except BaselineError as exc:
        msg = f"FAIL: invalid baseline: {exc}"
        print(msg, file=sys.stderr)
        if args.summary_file:
            args.summary_file.write_text(f"# Plugin Trivy gate\n\n**FAIL** — {exc}\n")
        return 1

    today = dt.date.fromisoformat(args.today)
    targets: dict[str, list[dict]] = {}
    for jf in sorted(args.results_dir.glob("*.json")):
        targets[jf.stem] = load_findings(jf)
    if not targets:
        print(f"FAIL: no Trivy result files in {args.results_dir}", file=sys.stderr)
        return 1

    report = evaluate(targets, baseline, today=today, high_mode=args.high_mode)
    expected = [t.strip() for t in args.expect_targets.split(",") if t.strip()]
    missing = [t for t in expected if t not in targets]
    for t in missing:
        report["failures"].append(f"missing Trivy results for expected target: {t}")
    report["targets_scanned"] = sorted(targets)
    report["targets_missing"] = missing
    text = render(report, high_mode=args.high_mode)
    print(text)
    if args.summary_file:
        with args.summary_file.open("a") as fh:
            fh.write(text)
    if args.report_file:
        args.report_file.write_text(json.dumps(report, indent=2) + "\n")
    return 1 if report["failures"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
