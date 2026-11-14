#!/usr/bin/env python3
"""Unit test for publish.py's Play edit transaction, driven by a fake API
service (no network, no credentials). Run INSIDE the plugin image by
smoke_test.sh, where google-api-python-client is installed:

    PYTHONPATH=/opt/gplay python3 publish_test.py

Proves the edit→upload→(deobfuscation)→tracks.update→commit sequence and
that versionCode, track, status, rollout and release notes are threaded
through correctly — the logic a `--check`/dry-run could never exercise.
"""
from __future__ import annotations

import sys
import tempfile
from typing import Any

import publish


class _Call:
    def __init__(self, log: list, name: str, result: Any, kwargs: dict):
        self._log, self._name, self._result, self._kwargs = log, name, result, kwargs

    def execute(self) -> Any:
        self._log.append((self._name, self._kwargs))
        return self._result


class _Uploader:
    """A distinct resource per upload kind so a bundle-vs-apk mixup is caught:
    bundles() and apks() must NOT resolve to the same upload path."""

    def __init__(self, log: list, name: str, result: Any):
        self._log, self._name, self._result = log, name, result

    def upload(self, **k):
        return _Call(self._log, self._name, self._result, k)


class FakeEdits:
    """Records every API call; returns canned responses."""

    def __init__(self, log: list):
        self.log = log

    def insert(self, **k):
        return _Call(self.log, "insert", {"id": "edit-1"}, k)

    def bundles(self):
        return _Uploader(self.log, "bundle_upload", {"versionCode": 42})

    def apks(self):
        return _Uploader(self.log, "apk_upload", {"versionCode": 42})

    def deobfuscationfiles(self):
        return _Uploader(self.log, "deobf_upload", {})

    def tracks(self):
        return self

    def update(self, **k):
        return _Call(self.log, "update", {}, k)

    def commit(self, **k):
        return _Call(self.log, "commit", {}, k)


class FakeService:
    def __init__(self, log: list):
        self._edits = FakeEdits(log)

    def edits(self):
        return self._edits


def _names(log):
    return [n for n, _ in log]


def _kwargs(log, name):
    for n, k in log:
        if n == name:
            return k
    raise AssertionError(f"no {name} call recorded; got {_names(log)}")


failures = 0


def check(cond: bool, msg: str) -> None:
    global failures
    if cond:
        print(f"ok: {msg}")
    else:
        print(f"FAIL: {msg}")
        failures += 1


def main() -> int:
    # A real tiny file so MediaFileUpload (used inside publish) is happy.
    aab = tempfile.NamedTemporaryFile(suffix=".aab", delete=False)
    aab.write(b"dummy-bundle")
    aab.close()
    mapping = tempfile.NamedTemporaryFile(suffix=".txt", delete=False)
    mapping.write(b"a -> b")
    mapping.close()

    # --- staged rollout AAB with mapping + release notes ---
    log: list = []
    vc = publish.publish(
        FakeService(log),
        {
            "package_name": "com.example.app",
            "artifact": aab.name,
            "track": "production",
            "status": "inProgress",
            "rollout": 0.1,
            "release_notes": "hello testers",
            "language": "en-US",
            "mapping": mapping.name,
        },
    )
    check(vc == 42, "returns the uploaded versionCode")
    check(
        _names(log) == ["insert", "bundle_upload", "deobf_upload", "update", "commit"],
        f"AAB transaction is insert→bundles.upload→deobf→update→commit (got {_names(log)})",
    )
    up = _kwargs(log, "bundle_upload")
    check("media_body" in up and up["editId"] == "edit-1", "bundle upload uses the opened edit")
    tr = _kwargs(log, "update")
    rel = tr["body"]["releases"][0]
    check(tr["track"] == "production", "track.update targets the requested track")
    check(rel["versionCodes"] == ["42"], "release carries the uploaded versionCode")
    check(rel["status"] == "inProgress", "release status is threaded through")
    check(rel.get("userFraction") == 0.1, "staged rollout sets userFraction")
    check(rel["releaseNotes"][0]["text"] == "hello testers", "release notes are threaded through")
    deobf = _kwargs(log, "deobf_upload")
    check(
        deobf.get("deobfuscationFileType") == "proguard" and deobf.get("apkVersionCode") == 42,
        "mapping uploaded as proguard deobfuscation for the versionCode",
    )

    # --- completed full rollout, no mapping, no rollout fraction ---
    log2: list = []
    publish.publish(
        FakeService(log2),
        {
            "package_name": "com.example.app",
            "artifact": aab.name,
            "track": "internal",
            "status": "completed",
        },
    )
    check(
        _names(log2) == ["insert", "bundle_upload", "update", "commit"],
        f"no mapping → no deobfuscation upload (got {_names(log2)})",
    )
    rel2 = _kwargs(log2, "update")["body"]["releases"][0]
    check("userFraction" not in rel2, "completed release carries no userFraction")

    # --- APK goes through apks().upload, not bundles() ---
    apk = tempfile.NamedTemporaryFile(suffix=".apk", delete=False)
    apk.write(b"dummy-apk")
    apk.close()
    log3: list = []
    publish.publish(
        FakeService(log3),
        {"package_name": "com.example.app", "artifact": apk.name, "track": "alpha", "status": "draft"},
    )
    check(
        _names(log3) == ["insert", "apk_upload", "update", "commit"],
        f"APK goes through apks().upload (not bundles) and commits (got {_names(log3)})",
    )

    print("----")
    if failures:
        print(f"FAILED: {failures} check(s)")
        return 1
    print("PASS: google-play publish unit")
    return 0


if __name__ == "__main__":
    sys.exit(main())
