#!/usr/bin/env python3
"""In-image tests for the Build Upload and TestFlight transaction."""
from __future__ import annotations

import hashlib
import os
import plistlib
import tempfile
import zipfile
from typing import Any

import publish


failures = 0


def check(condition: bool, message: str) -> None:
    global failures
    if condition:
        print(f"ok: {message}")
    else:
        print(f"FAIL: {message}")
        failures += 1


def make_ipa() -> str:
    handle, path = tempfile.mkstemp(suffix=".ipa")
    os.close(handle)
    info = {
        "CFBundleIdentifier": "com.example.mobile",
        "CFBundleShortVersionString": "3.2.1",
        "CFBundleVersion": "87",
    }
    with zipfile.ZipFile(path, "w") as archive:
        archive.writestr("Payload/Mobile.app/Info.plist", plistlib.dumps(info))
        archive.writestr("Payload/Mobile.app/Mobile", b"signed-binary-fixture")
    return path


class FakeService:
    def __init__(self, states: list[dict[str, Any]] | None = None, bad_ranges: bool = False):
        self.log: list[tuple[str, Any]] = []
        self.states = list(states or [])
        self.bad_ranges = bad_ranges

    def create_build_upload(self, app_id: str, short: str, build: str, platform: str):
        self.log.append(("create", (app_id, short, build, platform)))
        return {"id": "upload-1"}

    def create_upload_file(self, upload_id: str, name: str, size: int):
        self.log.append(("reserve", (upload_id, name, size)))
        half = size // 2
        second_offset = half + (1 if self.bad_ranges else 0)
        return {
            "id": "file-1",
            "attributes": {
                "uploadOperations": [
                    {"method": "PUT", "url": "https://blob.invalid/2", "offset": second_offset, "length": size - half, "requestHeaders": []},
                    {"method": "PUT", "url": "https://blob.invalid/1", "offset": 0, "length": half, "requestHeaders": []},
                ]
            },
        }

    def upload_part(self, path: str, operation: dict[str, Any], part: int):
        with open(path, "rb") as source:
            source.seek(operation["offset"])
            body = source.read(operation["length"])
        self.log.append((f"part-{part}", (operation["offset"], body)))

    def commit_upload_file(self, file_id: str, sha256: str):
        self.log.append(("commit", (file_id, sha256)))
        return {"id": file_id}

    def delete_build_upload(self, upload_id: str):
        self.log.append(("delete", upload_id))

    def get_build_upload(self, upload_id: str):
        self.log.append(("poll", upload_id))
        return self.states.pop(0)

    def list_beta_groups(self, app_id: str):
        self.log.append(("groups", app_id))
        return [
            {"id": "group-qa", "attributes": {"name": "Internal QA"}},
            {"id": "group-beta", "attributes": {"name": "Beta"}},
        ]

    def create_beta_localization(self, build_id: str, locale: str, notes: str):
        self.log.append(("notes", (build_id, locale, notes)))

    def add_build_to_group(self, build_id: str, group_id: str):
        self.log.append(("attach", (build_id, group_id)))


def state(name: str, *, build_id: str = "", error: str = "") -> dict[str, Any]:
    item: dict[str, Any] = {
        "attributes": {
            "state": {
                "state": name,
                "errors": ([{"code": "INVALID", "description": error}] if error else []),
            }
        },
        "relationships": {},
    }
    if build_id:
        item["relationships"] = {"build": {"data": {"type": "builds", "id": build_id}}}
    return item


def config(path: str, track: str = "upload") -> dict[str, Any]:
    return {
        "app_id": "123456789",
        "ipa": path,
        "platform": "IOS",
        "track": track,
        "beta_groups": [],
        "release_notes": "",
        "language": "en-US",
        "wait_timeout": 1800,
        "poll_interval": 0,
    }


class UploadResponse:
    status_code = 200


class UploadSession:
    def __init__(self):
        self.body = b""
        self.headers: dict[str, str] = {}

    def request(self, method: str, url: str, *, headers: dict[str, str], data: Any, timeout: Any):
        self.headers = headers
        while chunk := data.read(3):
            self.body += chunk
        return UploadResponse()


class RequestsStub:
    class RequestException(Exception):
        pass


class APIResponse:
    def __init__(self, status_code: int, payload: dict[str, Any] | None = None):
        self.status_code = status_code
        self._payload = payload or {}

    def json(self):
        return self._payload


class APISession:
    def __init__(self, responses: list[APIResponse]):
        self.responses = responses
        self.calls: list[dict[str, Any]] = []

    def request(self, method: str, url: str, **kwargs: Any):
        self.calls.append({"method": method, "url": url, **kwargs})
        return self.responses.pop(0)


def main() -> int:
    ipa = make_ipa()
    metadata = publish.inspect_ipa(ipa)
    check(
        metadata == {"bundle_id": "com.example.mobile", "short_version": "3.2.1", "build_version": "87"},
        "reads bundle/version/build from the IPA Info.plist",
    )

    # Upload-only: reserve, upload every range in offset order, checksum, stop.
    service = FakeService()
    result = publish.publish(service, config(ipa))
    names = [name for name, _ in service.log]
    check(names == ["create", "reserve", "part-1", "part-2", "commit"], f"upload transaction order is exact (got {names})")
    check(service.log[0][1] == ("123456789", "3.2.1", "87", "IOS"), "build upload uses IPA metadata and app id")
    uploaded = service.log[2][1][1] + service.log[3][1][1]
    check(uploaded == open(ipa, "rb").read(), "multipart operations cover the exact IPA bytes in offset order")
    want_hash = hashlib.sha256(open(ipa, "rb").read()).hexdigest()
    check(service.log[-1][1] == ("file-1", want_hash), "commit carries SHA-256 of the complete IPA")
    check("build_id" not in result and result["build_upload_id"] == "upload-1", "upload track returns after commit without pretending processing completed")

    # TestFlight: wait for processing, create notes, then attach exact groups.
    service2 = FakeService([state("PROCESSING"), state("COMPLETE", build_id="build-9")])
    cfg = config(ipa, "testflight")
    cfg.update({"beta_groups": ["Internal QA", "Beta"], "release_notes": "hello testers", "language": "pt-BR"})
    result2 = publish.publish(service2, cfg, sleep=lambda _: None, monotonic=lambda: 0.0)
    names2 = [name for name, _ in service2.log]
    check(names2[-6:] == ["poll", "poll", "groups", "notes", "attach", "attach"], f"TestFlight waits then localizes and assigns groups (got {names2})")
    check(("notes", ("build-9", "pt-BR", "hello testers")) in service2.log, "release notes target the processed build and locale")
    check(service2.log[-2:] == [("attach", ("build-9", "group-qa")), ("attach", ("build-9", "group-beta"))], "resolved group IDs receive the processed build")
    check(result2["build_id"] == "build-9" and result2["beta_groups"] == "Internal QA,Beta", "TestFlight result identifies build and groups")

    # Processing failures carry Apple's precise, non-secret state detail.
    service3 = FakeService([state("FAILED", error="invalid provisioning profile")])
    try:
        publish.publish(service3, config(ipa, "testflight"), sleep=lambda _: None, monotonic=lambda: 0.0)
        failed = "no error"
    except RuntimeError as exc:
        failed = str(exc)
    check("invalid provisioning profile" in failed, "Apple processing failure is fail-loud with state detail")

    # Bad multipart geometry must fail before sending any blob bytes.
    service4 = FakeService(bad_ranges=True)
    try:
        publish.publish(service4, config(ipa))
        bad_ranges = "no error"
    except RuntimeError as exc:
        bad_ranges = str(exc)
    check(
        "non-contiguous" in bad_ranges
        and all(not n.startswith("part-") for n, _ in service4.log)
        and service4.log[-1] == ("delete", "upload-1"),
        "gapped upload ranges fail before transfer and remove the reservation",
    )

    # Exercise the real streaming slice: no full-file buffer and no ASC JWT on
    # Apple's pre-signed blob URL.
    real = object.__new__(publish.AppStoreConnectService)
    real._session = UploadSession()
    real._requests = RequestsStub
    raw = open(ipa, "rb").read()
    real.upload_part(
        ipa,
        {"method": "PUT", "url": "https://blob.invalid/signed", "offset": 5, "length": 11, "requestHeaders": [{"name": "X-Upload", "value": "yes"}]},
        1,
    )
    check(real._session.body == raw[5:16], "real uploader streams exactly the requested byte range")
    check(real._session.headers == {"X-Upload": "yes"} and "Authorization" not in real._session.headers, "blob upload uses only Apple-provided headers, never the JWT")

    # Pin the production JSON:API request shapes from Apple's 4.1+ OpenAPI
    # schema, rather than testing only the service-injected orchestration.
    api = object.__new__(publish.AppStoreConnectService)
    api._token = lambda: "signed-jwt"
    api._session = APISession(
        [
            APIResponse(201, {"data": {"id": "up-1"}}),
            APIResponse(201, {"data": {"id": "file-1"}}),
            APIResponse(200, {"data": {"id": "file-1"}}),
            APIResponse(201, {"data": {"id": "loc-1"}}),
            APIResponse(204),
            APIResponse(204),
        ]
    )
    api.create_build_upload("123", "3.2.1", "87", "IOS")
    api.create_upload_file("up-1", "Mobile.ipa", 456)
    api.commit_upload_file("file-1", "abc123")
    api.create_beta_localization("build-9", "en-US", "notes")
    api.add_build_to_group("build-9", "group-qa")
    api.delete_build_upload("up-abandoned")
    calls = api._session.calls
    check(
        calls[0]["json"]["data"] == {
            "type": "buildUploads",
            "attributes": {"cfBundleShortVersionString": "3.2.1", "cfBundleVersion": "87", "platform": "IOS"},
            "relationships": {"app": {"data": {"type": "apps", "id": "123"}}},
        },
        "production create request matches Apple's BuildUpload JSON:API shape",
    )
    check(
        calls[1]["json"]["data"]["attributes"] == {"assetType": "ASSET", "fileName": "Mobile.ipa", "fileSize": 456, "uti": "com.apple.ipa"}
        and calls[1]["json"]["data"]["relationships"]["buildUpload"]["data"]["id"] == "up-1",
        "production reservation declares IPA asset and build-upload relationship",
    )
    check(
        calls[2]["json"]["data"]["attributes"] == {
            "uploaded": True,
            "sourceFileChecksums": {"file": {"hash": "abc123", "algorithm": "SHA_256"}},
        },
        "production commit sets uploaded and the SHA-256 file checksum",
    )
    check(
        calls[3]["json"]["data"]["relationships"]["build"]["data"]["id"] == "build-9"
        and calls[4]["json"] == {"data": [{"type": "builds", "id": "build-9"}]},
        "production TestFlight requests target the processed build",
    )
    check(
        all(call["headers"]["Authorization"] == "Bearer signed-jwt" for call in calls),
        "App Store Connect API calls carry JWT authorization",
    )
    check(
        calls[5]["method"] == "DELETE"
        and calls[5]["url"].endswith("/v1/buildUploads/up-abandoned"),
        "production cleanup deletes an abandoned pre-commit build upload",
    )

    # Real ES256 generation from a PKCS#8 .p8: header, issuer, audience and
    # maximum lifetime are verified without contacting Apple.
    import jwt
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import ec

    private_key = ec.generate_private_key(ec.SECP256R1())
    pem = private_key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    key_file = tempfile.NamedTemporaryFile(suffix=".p8", delete=False)
    key_file.write(pem)
    key_file.close()
    auth = publish.AppStoreConnectService("KEY123", "issuer-uuid", key_file.name)
    token = auth._token()
    header = jwt.get_unverified_header(token)
    claims = jwt.decode(token, private_key.public_key(), algorithms=["ES256"], audience="appstoreconnect-v1")
    check(header["kid"] == "KEY123" and header["alg"] == "ES256", "JWT uses the team key id and ES256")
    check(claims["iss"] == "issuer-uuid" and claims["exp"] - claims["iat"] == 1_199, "JWT pins issuer, audience and sub-20-minute lifetime")

    print("----")
    if failures:
        print(f"FAILED: {failures} check(s)")
        return 1
    print("PASS: app-store-connect publish unit")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
