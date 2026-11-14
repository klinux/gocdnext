#!/usr/bin/env python3
"""gocdnext/google-play — publish an Android artifact to Google Play.

Runs the Play Developer API edit transaction:

    edits.insert → bundles/apks.upload → [deobfuscationfiles.upload]
    → edits.tracks.update → edits.commit

The transaction is atomic: nothing is published unless `commit` succeeds,
so a failure anywhere before it leaves the Play listing untouched (the
edit is abandoned by Google). Configuration comes from PLUGIN_* env vars
(the entrypoint validated them); the service-account credentials are read
from the file GOOGLE_APPLICATION_CREDENTIALS points at — never from argv.

`publish(service, cfg)` takes the API service as an argument so it can be
driven by a fake in publish_test.py; `main()` builds the real one.
"""
from __future__ import annotations

import os
import sys
from typing import Any

SCOPE = "https://www.googleapis.com/auth/androidpublisher"
VALID_STATUS = {"completed", "draft", "inProgress", "halted"}


def get_service() -> Any:
    """Build the real Androidpublisher service from the SA credentials file."""
    from google.oauth2 import service_account  # imported lazily so the
    from googleapiclient.discovery import build  # module imports under test
    creds = service_account.Credentials.from_service_account_file(
        os.environ["GOOGLE_APPLICATION_CREDENTIALS"], scopes=[SCOPE]
    )
    return build("androidpublisher", "v3", credentials=creds, cache_discovery=False)


def _media(path: str) -> Any:
    from googleapiclient.http import MediaFileUpload
    return MediaFileUpload(path, mimetype="application/octet-stream", resumable=True)


def publish(service: Any, cfg: dict[str, Any]) -> int:
    """Run the edit transaction. Returns the uploaded versionCode."""
    pkg = cfg["package_name"]
    artifact = cfg["artifact"]
    edits = service.edits()

    edit_id = edits.insert(packageName=pkg, body={}).execute()["id"]

    if artifact.endswith(".aab"):
        up = edits.bundles().upload(
            packageName=pkg, editId=edit_id, media_body=_media(artifact)
        ).execute()
    else:  # .apk — the entrypoint already rejected any other extension
        up = edits.apks().upload(
            packageName=pkg, editId=edit_id, media_body=_media(artifact)
        ).execute()
    version_code = int(up["versionCode"])

    mapping = cfg.get("mapping")
    if mapping:
        edits.deobfuscationfiles().upload(
            packageName=pkg,
            editId=edit_id,
            apkVersionCode=version_code,
            deobfuscationFileType="proguard",
            media_body=_media(mapping),
        ).execute()

    release: dict[str, Any] = {
        "versionCodes": [str(version_code)],
        "status": cfg["status"],
    }
    notes = cfg.get("release_notes")
    if notes:
        release["releaseNotes"] = [
            {"language": cfg.get("language", "en-US"), "text": notes}
        ]
    # A staged rollout (inProgress/halted) carries the audience fraction.
    if cfg["status"] in ("inProgress", "halted"):
        release["userFraction"] = cfg["rollout"]

    edits.tracks().update(
        packageName=pkg,
        editId=edit_id,
        track=cfg["track"],
        body={"releases": [release]},
    ).execute()

    edits.commit(packageName=pkg, editId=edit_id).execute()
    return version_code


def _cfg_from_env() -> dict[str, Any]:
    cfg: dict[str, Any] = {
        "package_name": os.environ["PLUGIN_PACKAGE_NAME"],
        "artifact": os.environ["PLUGIN_ARTIFACT"],
        "track": os.environ["PLUGIN_TRACK"],
        "status": os.environ.get("PLUGIN_STATUS", "completed"),
        "release_notes": os.environ.get("PLUGIN_RELEASE_NOTES", ""),
        "language": os.environ.get("PLUGIN_LANGUAGE", "en-US"),
        "mapping": os.environ.get("PLUGIN_MAPPING", ""),
    }
    if cfg["status"] in ("inProgress", "halted"):
        cfg["rollout"] = float(os.environ["PLUGIN_ROLLOUT"])
    return cfg


def main() -> int:
    cfg = _cfg_from_env()
    version_code = publish(get_service(), cfg)
    print(
        f"gocdnext/google-play: published versionCode={version_code} "
        f"to track '{cfg['track']}' (status={cfg['status']})"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
