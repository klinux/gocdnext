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

# `result_class` (Trivy's os-pkgs/lang-pkgs) drives OS-vs-binary handling.
# `trivy_target` is required for binary/library (lang-pkgs) findings — it pins
# WHICH binary — but OMITTED for OS (os-pkgs) findings (see IDENTITY_*).
REQUIRED_FIELDS = (
    "targets", "cve", "package", "installed", "origin", "result_class", "class",
    "justification", "mitigation", "advisory", "upstream_issue", "approved_pr",
    "owner", "approved_at", "expires",
)
VALID_CLASSES = {"emergency", "high-fixable", "upstream-bound", "no-fix", "os-no-fix"}
# Classes that may hold a CRITICAL open. `emergency` is the only cover for a
# CRITICAL that HAS a fix or is non-OS; `os-no-fix` is a narrow, signed-off cover
# for an OS-package CRITICAL with NO upstream fix (see the os-no-fix checks).
CRITICAL_CLASSES = {"emergency", "os-no-fix"}
# Maximum exception lifetime (days) per class, enforced as expires - approved_at.
# This makes the 7/14/90-day policy a HARD check, not just a review convention.
MAX_WINDOW_DAYS = {"emergency": 7, "high-fixable": 14, "upstream-bound": 90,
                   "no-fix": 90, "os-no-fix": 90}
# An `unpublished` entry removes a whole plugin from the expected inventory, so
# it gets a SHORT window — it must be revisited (publish or re-justify) soon.
UNPUBLISHED_MAX_DAYS = 30
UNPUBLISHED_FIELDS = ("target", "reason", "approved_at", "approved_pr", "expires")

# OS vs binary/library is taken straight from Trivy's own report CLASS — `os-pkgs`
# for distro packages, `lang-pkgs` for everything compiled/bundled (gobinary, npm,
# jar, …) — rather than a parallel allowlist of distro names that drifts from the
# scanner. For OS findings Trivy's `Target` is the image+distro string
# ("<ref> (debian 12.12)"), which changes with the ref and every distro
# point-release and is identical for every OS finding, so pinning it adds churn
# without discriminating (the installed version already pins the CVE). So OS
# exceptions OMIT `trivy_target`; binary/library exceptions keep it REQUIRED so a
# different binary can't inherit one.
CLASS_OS = "os-pkgs"
CLASS_LANG = "lang-pkgs"
VALID_CLASSES_RESULT = {CLASS_OS, CLASS_LANG}


def _is_os(item: dict) -> bool:
    return item.get("result_class") == CLASS_OS


# An exception's identity — everything that must match a Trivy finding. Narrow on
# purpose: a different installed version, package type, or report class never
# inherits one. `result_class` is ALWAYS in the identity (so a finding can't match
# an exception of the other class, and an unknown-class finding matches nothing);
# `trivy_target` is in it for binary/library findings (so a different binary can't
# inherit) and dropped for OS findings (see above).
IDENTITY_BIN = ("target", "trivy_target", "cve", "package", "installed", "origin", "result_class")
IDENTITY_OS = ("target", "cve", "package", "installed", "origin", "result_class")


def _identity_fields(item: dict) -> tuple:
    return IDENTITY_OS if _is_os(item) else IDENTITY_BIN


class BaselineError(Exception):
    """Raised when the baseline file is structurally invalid -> hard FAIL."""


def _parse_date(value: object, where: str) -> dt.date:
    if not isinstance(value, str):
        raise BaselineError(f"{where}: `expires` must be a quoted YYYY-MM-DD string")
    try:
        return dt.date.fromisoformat(value)
    except ValueError as exc:
        raise BaselineError(f"{where}: bad `expires` {value!r}: {exc}") from None


def _today_utc() -> dt.date:
    return dt.datetime.now(dt.timezone.utc).date()


def _read_raw(path: pathlib.Path) -> dict:
    try:
        raw = yaml.safe_load(path.read_text()) or {}
    except (OSError, yaml.YAMLError) as exc:
        raise BaselineError(f"cannot read/parse {path}: {exc}") from None
    if not isinstance(raw, dict) or raw.get("version") != 1:
        raise BaselineError("baseline must be a mapping with `version: 1`")
    return raw


def load_unpublished_targets(path: pathlib.Path, today: dt.date | None = None) -> set[str]:
    """Validated set of plugin targets that intentionally have no :v1 yet.
    Dropping a whole plugin from the gate is high-impact, so each entry needs
    target + reason + approved_pr + approved_at (not future) + a non-expired
    `expires` within a SHORT window (<= UNPUBLISHED_MAX_DAYS) — it can't suppress
    a target indefinitely and must be revisited soon."""
    today = today or _today_utc()
    raw = _read_raw(path)
    items = raw.get("unpublished") or []
    if not isinstance(items, list):
        raise BaselineError("`unpublished` must be a list")
    targets: set[str] = set()
    for i, item in enumerate(items):
        where = f"unpublished[{i}]"
        if not isinstance(item, dict):
            raise BaselineError(f"{where}: must be a mapping")
        for f in UNPUBLISHED_FIELDS:
            if not item.get(f):
                raise BaselineError(f"{where}: missing required field {f!r}")
        approved = _parse_date(item["approved_at"], where)
        exp = _parse_date(item["expires"], where)
        if approved > today:
            raise BaselineError(f"{where}: approved_at {item['approved_at']} is in the future")
        if exp < today:
            raise BaselineError(
                f"{where}: unpublished entry for {item['target']!r} expired "
                f"{item['expires']} — publish it or re-justify")
        span = (exp - approved).days
        if span < 0:
            raise BaselineError(f"{where}: expires is before approved_at")
        if span > UNPUBLISHED_MAX_DAYS:
            raise BaselineError(
                f"{where}: unpublished window is {span} days, max is "
                f"{UNPUBLISHED_MAX_DAYS} (publish the plugin or renew by PR)")
        if item["target"] in targets:
            raise BaselineError(f"{where}: duplicate unpublished target {item['target']!r}")
        targets.add(item["target"])
    return targets


def load_baseline(path: pathlib.Path, today: dt.date | None = None) -> list[dict]:
    """Parse + validate the baseline. Returns the list of exception dicts, each
    with `targets` normalised to a list and `expires`/`approved_at` parsed to
    dates. Also validates the `unpublished` block. `today` (UTC by default) gates
    the approved_at-not-in-the-future check and unpublished expiry."""
    today = today or _today_utc()
    raw = _read_raw(path)
    load_unpublished_targets(path, today)  # validate the unpublished block too
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
        if item["result_class"] not in VALID_CLASSES_RESULT:
            raise BaselineError(
                f"{where}: result_class {item['result_class']!r} not in "
                f"{sorted(VALID_CLASSES_RESULT)}")
        # trivy_target: required (non-empty) for lang-pkgs (pins WHICH binary),
        # and must be ABSENT for os-pkgs (the Trivy target there is a volatile
        # image+distro string; the installed version already pins the CVE). Check
        # PRESENCE, not truthiness — an explicit empty/null on an OS entry is still
        # a mistake worth rejecting.
        if _is_os(item):
            if "trivy_target" in item:
                raise BaselineError(
                    f"{where}: os-pkgs exception must OMIT trivy_target entirely "
                    f"(it's a volatile image+distro string)")
        elif not item.get("trivy_target"):
            raise BaselineError(
                f"{where}: lang-pkgs exception requires a non-empty trivy_target")
        # os-no-fix: a signed-off cover for an OS-package CRITICAL with NO upstream
        # fix. Narrow by construction — OS-only, the fix field must be an explicit
        # empty string (the premise is "no fix"; the gate lapses it the moment
        # Trivy reports one), and a named risk acceptance is mandatory.
        if item["class"] == "os-no-fix":
            if not _is_os(item):
                raise BaselineError(
                    f"{where}: class os-no-fix is only for os-pkgs findings "
                    f"(result_class={item['result_class']!r})")
            if item.get("fixed_version", "__MISSING__") != "":
                raise BaselineError(
                    f"{where}: class os-no-fix requires an explicit `fixed_version: \"\"` "
                    f"(the premise is no upstream fix)")
            rab = item.get("risk_accepted_by")
            if not (isinstance(rab, str) and rab.strip()):
                raise BaselineError(
                    f"{where}: class os-no-fix requires `risk_accepted_by` as a "
                    f"non-empty string sign-off (got {rab!r})")
        item = dict(item)
        item["expires_date"] = _parse_date(item["expires"], where)
        item["approved_at_date"] = _parse_date(item["approved_at"], where)
        # approved_at in the FUTURE would let a short window (e.g. 7-day
        # emergency) sit far ahead and hold a CRITICAL open today — reject it.
        if item["approved_at_date"] > today:
            raise BaselineError(
                f"{where}: approved_at {item['approved_at']} is in the future")
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
        fields = _identity_fields(item)
        for t in targets:
            key = (t,) + tuple(str(item[f]) for f in fields[1:])
            if key in seen:
                raise BaselineError(f"{where}: duplicate exception for {key}")
            seen.add(key)
        out.append(item)
    return out


def load_findings(path: pathlib.Path) -> list[dict]:
    """Flatten a Trivy JSON report. Each finding keeps the full identity: the
    internal Trivy target (binary path / image), the package TYPE (origin), the
    installed version, plus cve/package/severity.

    Trivy occasionally emits the SAME finding twice (e.g. a package listed under
    more than one source within one Result). We dedupe by the full finding
    identity so a duplicate row is counted once everywhere — otherwise the
    per-target aggregate (incremented per finding) would over-count relative to
    the canonical lists (which dedupe), as seen for playwright tar/CVE-2026-59873."""
    data = json.loads(path.read_text())
    findings: list[dict] = []
    seen: set[tuple] = set()
    for result in data.get("Results") or []:
        rtarget = result.get("Target", "")
        rtype = result.get("Type", "")
        rclass = result.get("Class", "")
        for v in result.get("Vulnerabilities") or []:
            f = {
                "trivy_target": rtarget,
                "origin": rtype,
                "result_class": rclass,
                "cve": v.get("VulnerabilityID", ""),
                "package": v.get("PkgName", ""),
                "installed": v.get("InstalledVersion", ""),
                "fixed": v.get("FixedVersion", ""),
                "severity": (v.get("Severity") or "").upper(),
            }
            # Dedupe on the finding's full identity + severity (NOT `fixed`, which
            # is metadata absent from the canonical string — including it would let
            # two findings differing only in FixedVersion survive as two by_target
            # bumps while the canonical list collapses them to one). `result_class`
            # is included so two otherwise-identical rows of different class don't
            # collapse.
            fkey = (f["trivy_target"], f["origin"], f["result_class"], f["cve"],
                    f["package"], f["installed"], f["severity"])
            if fkey in seen:
                continue
            seen.add(fkey)
            findings.append(f)
    return findings


def _finding_key(target: str, f: dict) -> tuple:
    """The identity tuple shared by a finding and its exception. OS findings drop
    the volatile trivy_target; binary/library findings keep it."""
    return (target,) + tuple(f[k] for k in _identity_fields(f)[1:])


def build_index(baseline: list[dict]) -> dict[tuple, dict]:
    idx: dict[tuple, dict] = {}
    for item in baseline:
        fields = _identity_fields(item)
        for t in item["targets"]:
            key = (t,) + tuple(str(item[f]) for f in fields[1:])
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

    by_target: dict[str, dict[str, int]] = {}

    def bump(t: str, kind: str) -> None:
        by_target.setdefault(t, {"failures": 0, "warnings": 0, "matched": 0})[kind] += 1

    for target in sorted(targets):
        for f in targets[target]:
            sev = f["severity"]
            if sev not in ("CRITICAL", "HIGH"):
                continue
            tag = (f"{target} {f['cve']} {f['package']} {f['installed']} "
                   f"[{f['origin']}@{f['trivy_target'].split('/')[-1]}] ({sev})")

            # Fail closed on an unrecognised report Class: a vuln finding is
            # os-pkgs or lang-pkgs, so a missing/unknown Class is an anomaly, not a
            # free pass. Never let it fall through to the lang-pkgs branch where it
            # could inherit an exception.
            if f.get("result_class") not in VALID_CLASSES_RESULT:
                failures.append(
                    f"unknown Trivy Class {f.get('result_class')!r}: {tag}")
                bump(target, "failures")
                continue

            key = _finding_key(target, f)
            exc = index.get(key)

            if exc is None:
                if sev == "CRITICAL":
                    failures.append(f"CRITICAL with no exception: {tag}"); bump(target, "failures")
                elif high_mode == "enforce":
                    failures.append(f"HIGH not in baseline: {tag}"); bump(target, "failures")
                else:
                    warnings.append(f"HIGH not in baseline (observe): {tag}"); bump(target, "warnings")
                continue

            used_keys.add(key)
            if exc["expires_date"] < today:
                failures.append(
                    f"exception EXPIRED {exc['expires']} (renew or remove): {tag}"); bump(target, "failures")
            elif exc["class"] == "os-no-fix" and sev != "CRITICAL":
                # os-no-fix is a CRITICAL-only cover; a HIGH of the same identity
                # must use no-fix/upstream-bound, not inherit this.
                failures.append(
                    f"os-no-fix is CRITICAL-only; this finding is {sev} — "
                    f"use a no-fix/upstream-bound exception: {tag}"); bump(target, "failures")
            elif exc["class"] == "os-no-fix" and f.get("fixed"):
                # The os-no-fix premise is "no upstream fix". Trivy now reports one
                # ({fixed}), so the exception lapses immediately — patch it.
                failures.append(
                    f"os-no-fix exception but a fix is now available ({f['fixed']}) — "
                    f"patch it: {tag}"); bump(target, "failures")
            elif sev == "CRITICAL" and exc["class"] not in CRITICAL_CLASSES:
                failures.append(
                    f"CRITICAL needs an `emergency`/`os-no-fix` exception, got "
                    f"{exc['class']!r}: {tag}"); bump(target, "failures")
            else:
                matched.append(f"{tag} -> {exc['class']} until {exc['expires']}"); bump(target, "matched")

    # Expired entries are a hard fail even if the finding has since vanished —
    # they must be cleaned up. Non-expired entries that matched nothing are
    # obsolete noise and only warn.
    for item in baseline:
        fields = _identity_fields(item)
        for t in item["targets"]:
            key = (t,) + tuple(str(item[f]) for f in fields[1:])
            if key in used_keys:
                continue
            if item["expires_date"] < today:
                failures.append(
                    f"exception EXPIRED {item['expires']} (remove): "
                    f"{t} {item['cve']} {item['package']}"); bump(t, "failures")
            elif t in targets:
                # Only judge a non-expired entry obsolete when its target was
                # actually scanned this run — a missing scan is a separate FAIL.
                warnings.append(
                    f"obsolete exception — no matching finding, remove: "
                    f"{t} {item['cve']} {item['package']}"); bump(t, "warnings")

    return {
        "failures": sorted(set(failures)),
        "warnings": sorted(set(warnings)),
        "matched": sorted(set(matched)),
        "by_target": by_target,
    }


# Cap how many individual lines go into the Markdown summary. GitHub's step
# summary is capped at 1 MiB; thousands of observe-mode HIGH warnings blew past
# it and aborted the upload. The full, unabridged lists always live in the JSON
# report artifact; the summary shows per-target aggregates + the first N lines.
SUMMARY_MAX = {"failures": 200, "warnings": 50, "matched": 20}
# Belt-and-suspenders byte caps: a single pathological field (a huge package
# name or tag) can't blow the GitHub 1 MiB step-summary limit. Lines are clipped
# individually and the whole summary is hard-capped well under 1 MiB.
SUMMARY_LINE_MAXLEN = 500
SUMMARY_BYTES_MAX = 900_000


def _clip(s: str) -> str:
    return s if len(s) <= SUMMARY_LINE_MAXLEN else s[:SUMMARY_LINE_MAXLEN] + "…"


def render(report: dict, *, high_mode: str) -> str:
    lines = ["# Plugin Trivy gate", ""]
    lines.append(f"- mode: HIGH **{high_mode}**, CRITICAL **enforce**")
    for key in ("failures", "warnings", "matched"):
        lines.append(f"- {key}: {len(report[key])}")
    lines.append("")

    bt = report.get("by_target") or {}
    if bt:
        lines.append("## Per target (failures / warnings / matched)")
        lines.append("")
        lines.append("| target | failures | warnings | matched |")
        lines.append("| --- | ---: | ---: | ---: |")
        for t in sorted(bt):
            c = bt[t]
            lines.append(f"| {t} | {c['failures']} | {c['warnings']} | {c['matched']} |")
        lines.append("")

    for head, key in (("## Failures", "failures"), ("## Warnings", "warnings"),
                      ("## Matched exceptions", "matched")):
        items = report[key]
        if not items:
            continue
        cap = SUMMARY_MAX[key]
        lines.append(head)
        lines.extend(f"- {_clip(line)}" for line in items[:cap])
        if len(items) > cap:
            lines.append(f"- … and {len(items) - cap} more (see the gate-report artifact)")
        lines.append("")

    lines.append("**PASS** — no policy violations." if not report["failures"]
                 else "**FAIL** — see failures above (full detail in the artifact).")
    text = "\n".join(lines) + "\n"

    # Absolute byte cap: even with per-line clipping + count caps, guarantee the
    # summary fits GitHub's 1 MiB limit so the job can never fail just on size.
    if len(text.encode()) > SUMMARY_BYTES_MAX:
        notice = "\n\n**summary truncated to fit the size limit — see the gate-report artifact.**\n"
        budget = SUMMARY_BYTES_MAX - len(notice.encode())
        text = text.encode()[:budget].decode("utf-8", "ignore") + notice
    return text


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="gocdnext plugin Trivy gate")
    ap.add_argument("--baseline", required=True, type=pathlib.Path)
    ap.add_argument("--results-dir", required=True, type=pathlib.Path)
    ap.add_argument("--high-mode", choices=("observe", "enforce"), default="observe")
    ap.add_argument("--expect-targets", default="",
                    help="comma-separated targets that MUST have a results file; "
                         "a missing one is a hard FAIL (so a dropped scan can't "
                         "silently hide a CRITICAL)")
    ap.add_argument("--today", default=_today_utc().isoformat(),
                    help="override today's date (YYYY-MM-DD, UTC) — tests only")
    ap.add_argument("--summary-file", type=pathlib.Path,
                    help="append a Markdown summary here (e.g. $GITHUB_STEP_SUMMARY)")
    ap.add_argument("--report-file", type=pathlib.Path,
                    help="write the full JSON report here (CI artifact)")
    args = ap.parse_args(argv)

    today = dt.date.fromisoformat(args.today)
    try:
        baseline = load_baseline(args.baseline, today=today)
    except BaselineError as exc:
        msg = f"FAIL: invalid baseline: {exc}"
        print(msg, file=sys.stderr)
        if args.summary_file:
            args.summary_file.write_text(f"# Plugin Trivy gate\n\n**FAIL** — {exc}\n")
        return 1

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
