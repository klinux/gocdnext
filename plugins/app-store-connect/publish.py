#!/usr/bin/env python3
"""Upload an IPA through App Store Connect's Build Upload API.

The public `publish(service, cfg)` orchestration is deliberately service-
injectable: publish_test.py drives the complete upload/processing/TestFlight
transaction without Apple credentials or network. The real service creates a
fresh ES256 JWT for each API request and never sends it to the pre-signed blob
upload URLs.
"""
from __future__ import annotations

import hashlib
import os
import plistlib
import re
import sys
import time
import zipfile
from pathlib import Path
from typing import Any, Callable
from urllib.parse import quote, urlparse

API_BASE = "https://api.appstoreconnect.apple.com"
INFO_PLIST = re.compile(r"^Payload/[^/]+\.app/Info\.plist$")


class _FileSlice:
    """Read exactly one Apple-provided byte range without buffering the IPA."""

    def __init__(self, path: str, offset: int, length: int):
        self._file = open(path, "rb")
        self._file.seek(offset)
        self._length = length
        self._remaining = length

    def __len__(self) -> int:
        return self._length

    def read(self, size: int = -1) -> bytes:
        if self._remaining == 0:
            return b""
        if size < 0 or size > self._remaining:
            size = self._remaining
        chunk = self._file.read(size)
        self._remaining -= len(chunk)
        return chunk

    def close(self) -> None:
        self._file.close()

    def __enter__(self) -> "_FileSlice":
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


class AppStoreConnectService:
    def __init__(self, key_id: str, issuer_id: str, key_path: str):
        import jwt
        import requests

        self._jwt = jwt
        self._requests = requests
        self._session = requests.Session()
        self._key_id = key_id
        self._issuer_id = issuer_id
        self._key_path = key_path

    def _token(self) -> str:
        now = int(time.time())
        with open(self._key_path, "rb") as key_file:
            key = key_file.read()
        return self._jwt.encode(
            {"iss": self._issuer_id, "iat": now, "exp": now + 1_199, "aud": "appstoreconnect-v1"},
            key,
            algorithm="ES256",
            headers={"kid": self._key_id, "typ": "JWT"},
        )

    @staticmethod
    def _error_detail(response: Any) -> str:
        try:
            errors = response.json().get("errors", [])
        except (ValueError, AttributeError):
            errors = []
        details = []
        for error in errors[:5]:
            text = error.get("detail") or error.get("title") or error.get("code")
            if text:
                details.append(str(text).replace("\n", " ")[:500])
        return "; ".join(details) or "no structured error detail"

    def _request(
        self,
        method: str,
        path_or_url: str,
        *,
        expected: tuple[int, ...],
        body: dict[str, Any] | None = None,
        params: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        if path_or_url.startswith("https://"):
            if not path_or_url.startswith(f"{API_BASE}/"):
                raise RuntimeError("App Store Connect pagination returned an unexpected host")
            url = path_or_url
        else:
            url = f"{API_BASE}{path_or_url}"
        response = self._session.request(
            method,
            url,
            headers={
                "Authorization": f"Bearer {self._token()}",
                "Content-Type": "application/json",
            },
            json=body,
            params=params,
            timeout=(20, 90),
        )
        if response.status_code not in expected:
            raise RuntimeError(
                f"App Store Connect API {method} returned HTTP {response.status_code}: "
                f"{self._error_detail(response)}"
            )
        if response.status_code == 204:
            return {}
        try:
            return response.json()
        except ValueError as exc:
            raise RuntimeError("App Store Connect API returned invalid JSON") from exc

    def create_build_upload(
        self, app_id: str, short_version: str, build_version: str, platform: str
    ) -> dict[str, Any]:
        body = {
            "data": {
                "type": "buildUploads",
                "attributes": {
                    "cfBundleShortVersionString": short_version,
                    "cfBundleVersion": build_version,
                    "platform": platform,
                },
                "relationships": {
                    "app": {"data": {"type": "apps", "id": app_id}}
                },
            }
        }
        return self._request("POST", "/v1/buildUploads", expected=(201,), body=body)["data"]

    def create_upload_file(
        self, build_upload_id: str, file_name: str, file_size: int
    ) -> dict[str, Any]:
        body = {
            "data": {
                "type": "buildUploadFiles",
                "attributes": {
                    "assetType": "ASSET",
                    "fileName": file_name,
                    "fileSize": file_size,
                    "uti": "com.apple.ipa",
                },
                "relationships": {
                    "buildUpload": {
                        "data": {"type": "buildUploads", "id": build_upload_id}
                    }
                },
            }
        }
        return self._request("POST", "/v1/buildUploadFiles", expected=(201,), body=body)["data"]

    def upload_part(self, ipa_path: str, operation: dict[str, Any], part: int) -> None:
        method = str(operation.get("method", "")).upper()
        url = str(operation.get("url", ""))
        offset = int(operation["offset"])
        length = int(operation["length"])
        if method != "PUT" or urlparse(url).scheme != "https":
            raise RuntimeError(f"upload part {part} has an unsafe method or URL")
        headers = {
            str(header["name"]): str(header["value"])
            for header in operation.get("requestHeaders", [])
        }

        last_error = ""
        for attempt in range(1, 4):
            try:
                with _FileSlice(ipa_path, offset, length) as data:
                    response = self._session.request(
                        method, url, headers=headers, data=data, timeout=(30, 900)
                    )
                if 200 <= response.status_code < 300:
                    return
                last_error = f"HTTP {response.status_code}"
                if response.status_code < 500 and response.status_code != 429:
                    break
            except self._requests.RequestException as exc:
                last_error = type(exc).__name__
            if attempt < 3:
                time.sleep(2 ** (attempt - 1))
        # Never include the pre-signed URL: its query string grants upload access.
        raise RuntimeError(f"upload part {part} failed after retries ({last_error})")

    def commit_upload_file(self, upload_file_id: str, sha256: str) -> dict[str, Any]:
        body = {
            "data": {
                "type": "buildUploadFiles",
                "id": upload_file_id,
                "attributes": {
                    "uploaded": True,
                    "sourceFileChecksums": {
                        "file": {"hash": sha256, "algorithm": "SHA_256"}
                    },
                },
            }
        }
        return self._request(
            "PATCH",
            f"/v1/buildUploadFiles/{quote(upload_file_id, safe='')}",
            expected=(200,),
            body=body,
        )["data"]

    def delete_build_upload(self, build_upload_id: str) -> None:
        self._request(
            "DELETE",
            f"/v1/buildUploads/{quote(build_upload_id, safe='')}",
            expected=(204,),
        )

    def get_build_upload(self, build_upload_id: str) -> dict[str, Any]:
        return self._request(
            "GET",
            f"/v1/buildUploads/{quote(build_upload_id, safe='')}",
            expected=(200,),
            params={"fields[buildUploads]": "state,build", "include": "build"},
        )["data"]

    def list_beta_groups(self, app_id: str) -> list[dict[str, Any]]:
        url = f"/v1/apps/{quote(app_id, safe='')}/betaGroups"
        params: dict[str, str] | None = {"fields[betaGroups]": "name", "limit": "200"}
        groups: list[dict[str, Any]] = []
        while url:
            page = self._request("GET", url, expected=(200,), params=params)
            groups.extend(page.get("data", []))
            url = page.get("links", {}).get("next") or ""
            params = None
        return groups

    def create_beta_localization(self, build_id: str, locale: str, notes: str) -> None:
        body = {
            "data": {
                "type": "betaBuildLocalizations",
                "attributes": {"locale": locale, "whatsNew": notes},
                "relationships": {
                    "build": {"data": {"type": "builds", "id": build_id}}
                },
            }
        }
        self._request("POST", "/v1/betaBuildLocalizations", expected=(201,), body=body)

    def add_build_to_group(self, build_id: str, group_id: str) -> None:
        body = {"data": [{"type": "builds", "id": build_id}]}
        self._request(
            "POST",
            f"/v1/betaGroups/{quote(group_id, safe='')}/relationships/builds",
            expected=(204,),
            body=body,
        )


def inspect_ipa(path: str) -> dict[str, str]:
    try:
        with zipfile.ZipFile(path) as archive:
            candidates = [name for name in archive.namelist() if INFO_PLIST.match(name)]
            if len(candidates) != 1:
                raise ValueError(
                    f"IPA must contain exactly one top-level app Info.plist (found {len(candidates)})"
                )
            plist_entry = archive.getinfo(candidates[0])
            if plist_entry.file_size > 1024 * 1024:
                raise ValueError("IPA app Info.plist exceeds the 1 MiB safety limit")
            info = plistlib.loads(archive.read(plist_entry))
    except (zipfile.BadZipFile, KeyError, plistlib.InvalidFileException) as exc:
        raise ValueError("ipa is not a valid archive with a readable app Info.plist") from exc

    values = {
        "bundle_id": str(info.get("CFBundleIdentifier", "")).strip(),
        "short_version": str(info.get("CFBundleShortVersionString", "")).strip(),
        "build_version": str(info.get("CFBundleVersion", "")).strip(),
    }
    missing = [name for name, value in values.items() if not value]
    if missing:
        raise ValueError(f"IPA Info.plist is missing: {', '.join(missing)}")
    return values


def _sha256(path: str) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _validated_operations(operations: list[dict[str, Any]], file_size: int) -> list[dict[str, Any]]:
    ordered = sorted(operations, key=lambda item: int(item.get("offset", -1)))
    expected_offset = 0
    for operation in ordered:
        offset = int(operation.get("offset", -1))
        length = int(operation.get("length", 0))
        if offset != expected_offset or length < 1 or offset + length > file_size:
            raise RuntimeError("App Store Connect returned invalid or non-contiguous upload ranges")
        expected_offset += length
    if expected_offset != file_size:
        raise RuntimeError("App Store Connect upload ranges do not cover the complete IPA")
    return ordered


def _build_id(upload: dict[str, Any]) -> str:
    return str(
        upload.get("relationships", {}).get("build", {}).get("data", {}).get("id", "")
    )


def _state_error(upload: dict[str, Any]) -> str:
    state = upload.get("attributes", {}).get("state", {})
    details = []
    for item in state.get("errors", []):
        text = item.get("description") or item.get("code")
        if text:
            details.append(str(text).replace("\n", " ")[:500])
    return "; ".join(details) or "Apple returned no processing detail"


def _resolve_groups(service: Any, app_id: str, requested: list[str]) -> list[tuple[str, str]]:
    if not requested:
        return []
    by_name: dict[str, list[str]] = {}
    for group in service.list_beta_groups(app_id):
        name = str(group.get("attributes", {}).get("name", ""))
        if name:
            by_name.setdefault(name, []).append(str(group["id"]))
    missing = [name for name in requested if name not in by_name]
    ambiguous = [name for name in requested if len(by_name.get(name, [])) > 1]
    if missing:
        raise RuntimeError(f"TestFlight beta group(s) not found: {', '.join(missing)}")
    if ambiguous:
        raise RuntimeError(f"TestFlight beta group name(s) are ambiguous: {', '.join(ambiguous)}")
    return [(name, by_name[name][0]) for name in requested]


def publish(
    service: Any,
    cfg: dict[str, Any],
    *,
    sleep: Callable[[float], None] = time.sleep,
    monotonic: Callable[[], float] = time.monotonic,
) -> dict[str, str]:
    ipa_path = cfg["ipa"]
    metadata = inspect_ipa(ipa_path)
    file_size = os.path.getsize(ipa_path)

    build_upload = service.create_build_upload(
        cfg["app_id"], metadata["short_version"], metadata["build_version"], cfg["platform"]
    )
    build_upload_id = str(build_upload["id"])
    try:
        upload_file = service.create_upload_file(build_upload_id, Path(ipa_path).name, file_size)
        upload_file_id = str(upload_file["id"])
        operations = _validated_operations(
            upload_file.get("attributes", {}).get("uploadOperations", []), file_size
        )
        for index, operation in enumerate(operations, start=1):
            service.upload_part(ipa_path, operation, index)
        service.commit_upload_file(upload_file_id, _sha256(ipa_path))
    except Exception:
        # A pre-commit failure leaves a reusable reservation behind. Remove it
        # best-effort, but never mask the original upload/validation error.
        try:
            service.delete_build_upload(build_upload_id)
        except Exception:
            pass
        raise

    result = {
        "build_upload_id": build_upload_id,
        "bundle_id": metadata["bundle_id"],
        "short_version": metadata["short_version"],
        "build_version": metadata["build_version"],
    }
    if cfg["track"] == "upload":
        return result

    deadline = monotonic() + cfg["wait_timeout"]
    build_id = ""
    while True:
        current = service.get_build_upload(build_upload_id)
        state = str(current.get("attributes", {}).get("state", {}).get("state", ""))
        if state == "FAILED":
            raise RuntimeError(f"Apple failed to process the build: {_state_error(current)}")
        if state == "COMPLETE":
            build_id = _build_id(current)
            if not build_id:
                raise RuntimeError("Apple marked the build upload COMPLETE without a build relationship")
            break
        if state not in {"AWAITING_UPLOAD", "PROCESSING"}:
            raise RuntimeError(f"Apple returned unknown build upload state: {state or '<empty>'}")
        now = monotonic()
        if now >= deadline:
            raise RuntimeError(
                f"timed out after {cfg['wait_timeout']}s waiting for Apple build processing"
            )
        sleep(min(cfg["poll_interval"], max(0.0, deadline - now)))

    groups = _resolve_groups(service, cfg["app_id"], cfg["beta_groups"])
    if cfg["release_notes"]:
        service.create_beta_localization(build_id, cfg["language"], cfg["release_notes"])
    for _, group_id in groups:
        service.add_build_to_group(build_id, group_id)
    result["build_id"] = build_id
    result["beta_groups"] = ",".join(name for name, _ in groups)
    return result


def _groups_from_env(raw: str) -> list[str]:
    if not raw:
        return []
    groups = [part.strip() for part in raw.split(",")]
    if any(not group for group in groups):
        raise ValueError("beta-groups contains an empty group name")
    if len(set(groups)) != len(groups):
        raise ValueError("beta-groups contains a duplicate group name")
    return groups


def _cfg_from_env() -> dict[str, Any]:
    return {
        "app_id": os.environ["PLUGIN_APP_ID"],
        "ipa": os.environ["PLUGIN_IPA"],
        "platform": os.environ.get("PLUGIN_PLATFORM", "IOS"),
        "track": os.environ.get("PLUGIN_TRACK", "upload"),
        "beta_groups": _groups_from_env(os.environ.get("PLUGIN_BETA_GROUPS", "")),
        "release_notes": os.environ.get("PLUGIN_RELEASE_NOTES", ""),
        "language": os.environ.get("PLUGIN_LANGUAGE", "en-US"),
        "wait_timeout": int(os.environ.get("PLUGIN_WAIT_TIMEOUT", "1800")),
        "poll_interval": 15.0,
    }


def main() -> int:
    try:
        cfg = _cfg_from_env()
        service = AppStoreConnectService(
            os.environ["PLUGIN_API_KEY_ID"],
            os.environ["PLUGIN_API_ISSUER_ID"],
            os.environ["ASC_KEY_PATH"],
        )
        result = publish(service, cfg)
        if cfg["track"] == "testflight":
            groups = result.get("beta_groups") or "no explicit groups"
            print(
                "gocdnext/app-store-connect: processed "
                f"{result['bundle_id']} {result['short_version']} ({result['build_version']}); "
                f"build={result['build_id']}; TestFlight={groups}"
            )
        else:
            print(
                "gocdnext/app-store-connect: accepted "
                f"{result['bundle_id']} {result['short_version']} ({result['build_version']}); "
                f"build-upload={result['build_upload_id']}; Apple processing continues asynchronously"
            )
        return 0
    except (KeyError, OSError, RuntimeError, ValueError) as exc:
        print(f"gocdnext/app-store-connect: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
