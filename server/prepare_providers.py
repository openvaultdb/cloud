#!/usr/bin/env python3
"""Fetch and verify the immutable provider inventory for Cloud Run fixtures."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
import re
import tempfile
import urllib.error
import urllib.request
from pathlib import Path, PurePosixPath
from typing import Any, Callable
from urllib.parse import quote, urlsplit

import prepare_fixture
from publisher_selection import parse_publisher


SHA256_PATTERN = re.compile(r"^[0-9a-f]{64}$")
COMMIT_PATTERN = re.compile(r"^[0-9a-f]{40}$")
DATABASE_ID_PATTERN = re.compile(r"^[a-z][a-z0-9-]{0,62}$")
MAX_ARTIFACT_BYTES = 25 * 1024 * 1024
MAX_ENCODED_ARTIFACT_BYTES = 512 * 1024 * 1024
MAX_DECODED_ARTIFACT_BYTES = 2 * 1024 * 1024 * 1024
RAW_GITHUB_ORIGIN = "https://raw.githubusercontent.com"
REPOSITORY_PATTERN = re.compile(r"^[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$")
PROVIDER_FIELDS = {"id", "repository", "revision", "recordKeyFormat", "requirePublishedQuery", "smokeRecordsets", "emptyRecordsets", "blobSmokeFields", "foreignKeySmoke", "files", "corsOrigins", "storageEngine", "nativeFiles", "metadataFiles"}
PREPARATION_FIELDS = {"servingAdapter", "readProfile", "publisherManifest"}


def safe_relative_path(value: Any, context: str) -> str:
    if not isinstance(value, str) or not value or any(character in value for character in "\\?#%") or any(ord(character) < 32 or ord(character) == 127 for character in value):
        raise ValueError(f"{context} must be a non-empty safe POSIX relative path")
    # Inspect the original segments: PurePosixPath would silently collapse ./ or //.
    if any(part in ("", ".", "..") for part in value.split("/")):
        raise ValueError(f"{context} must not escape the provider root")
    path = PurePosixPath(value)
    if path.is_absolute():
        raise ValueError(f"{context} must not escape the provider root")
    return value


def read_json(data: bytes, context: str) -> dict[str, Any]:
    try:
        def unique_object(pairs):
            value = {}
            for key, item in pairs:
                if key in value:
                    raise ValueError(f"{context} repeats JSON member {key!r}")
                value[key] = item
            return value
        value = json.loads(data, object_pairs_hook=unique_object)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError(f"{context} is not valid UTF-8 JSON: {error}") from error
    if not isinstance(value, dict):
        raise ValueError(f"{context} must contain a JSON object")
    return value



def canonical_descriptor_members(document: Any, required: dict[str, type], optional: dict[str, type] | None = None) -> dict[str, Any]:
    if not isinstance(document, dict):
        raise ValueError("descriptor binding must be an object")
    expected = {**required, **(optional or {})}
    for name, value in document.items():
        if name not in expected:
            if any(name.casefold() == canonical.casefold() for canonical in expected):
                raise ValueError("descriptor binding uses a case alias")
            continue  # Preserve legitimate unrelated descriptor schema metadata.
        if type(value) is not expected[name]:
            raise ValueError("descriptor binding has a null or wrong-type member")
    if set(required) - set(document):
        raise ValueError("descriptor binding lacks a canonical member")
    return document


def validate_descriptor_members(document: dict[str, Any]) -> None:
    canonical_descriptor_members(document, {**{name: str for name in ("format", "localId", "id", "homepage", "serverId", "serverDbBaseUrl", "apiUrl")}, "deployment": dict, "capabilities": dict, "recordsets": list})
    for recordset in document["recordsets"]:
        canonical_descriptor_members(recordset, {"name": str, "columns": list}, {"kind": str})
        for column in recordset["columns"]:
            canonical_descriptor_members(column, {"name": str, "type": str})


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def verify_blob(data: bytes, expected_hash: str, expected_bytes: int | None, context: str) -> None:
    if sha256(data) != expected_hash:
        raise ValueError(f"{context} SHA-256 does not match its immutable inventory pin")
    if expected_bytes is not None and len(data) != expected_bytes:
        raise ValueError(f"{context} is {len(data)} bytes, expected {expected_bytes}")


def validate_sqlite_export(database_id: str, exports: list[dict[str, Any]], artifact: dict[str, Any]) -> None:
    if not isinstance(exports, list):
        raise ValueError(f"{database_id} provider contract exports must be an array")
    artifact_path = artifact["path"]
    sqlite_exports = [
        entry
        for entry in exports
        if entry.get("path") == artifact_path and entry.get("format", "sqlite" if artifact_path.endswith(".sqlite") else None) == "sqlite"
    ]
    if len(sqlite_exports) != 1:
        raise ValueError(f"{database_id} contract must declare its SQLite artifact exactly once")

    exported = sqlite_exports[0]
    compression = artifact.get("compression")
    if compression is None:
        if exported.get("compression") not in (None, ""):
            raise ValueError(f"{database_id} contract SQLite compression disagrees with its immutable provider pin")
        if exported.get("sha256") not in (None, artifact["sha256"]):
            raise ValueError(f"{database_id} contract SQLite hash disagrees with its immutable provider pin")
        if exported.get("bytes") != artifact["bytes"]:
            raise ValueError(f"{database_id} contract SQLite size disagrees with its immutable provider pin")
        return

    if compression != "gzip" or exported.get("compression") != compression:
        raise ValueError(f"{database_id} contract SQLite compression disagrees with its immutable provider pin")
    if exported.get("encodedPath") != artifact.get("encodedPath"):
        raise ValueError(f"{database_id} contract SQLite encoded path disagrees with its immutable provider pin")
    if exported.get("sha256") != artifact.get("sha256") or exported.get("bytes") != artifact.get("bytes"):
        raise ValueError(f"{database_id} contract SQLite encoded hash or size disagrees with its immutable provider pin")
    if exported.get("decodedSha256") != artifact.get("decodedSha256") or exported.get("decodedBytes") != artifact.get("decodedBytes"):
        raise ValueError(f"{database_id} contract SQLite decoded hash or size disagrees with its immutable provider pin")

    expected_chunks = artifact.get("chunks", [])
    exported_chunks = exported.get("chunks", [])
    if not isinstance(exported_chunks, list) or not isinstance(expected_chunks, list):
        raise ValueError(f"{database_id} contract SQLite chunks must be arrays")
    normalized_expected = [{key: chunk.get(key) for key in ("path", "bytes", "sha256")} for chunk in expected_chunks]
    normalized_exported = [{key: chunk.get(key) for key in ("path", "bytes", "sha256")} for chunk in exported_chunks if isinstance(chunk, dict)]
    if len(normalized_exported) != len(exported_chunks) or normalized_exported != normalized_expected:
        raise ValueError(f"{database_id} contract SQLite chunks disagree with its immutable provider pin")


def load_inventory(path: Path) -> list[dict[str, Any]]:
    return validate_inventory(read_json(path.read_bytes(), str(path)))


def validate_inventory(document: dict[str, Any]) -> list[dict[str, Any]]:
    version = document.get("version")
    if type(version) is not int or version not in (1, 2) or set(document) != {"version", "databases"} or not isinstance(document.get("databases"), list):
        raise ValueError("provider inventory must use version 1 or 2 with only a databases array")
    providers = document["databases"]
    seen_ids: set[str] = set()
    for provider in providers:
        if not isinstance(provider, dict):
            raise ValueError("each provider inventory entry must be a JSON object")
        allowed_fields = PROVIDER_FIELDS | (PREPARATION_FIELDS if version == 2 else set())
        if set(provider) - allowed_fields:
            raise ValueError(f"provider inventory version {version} has unknown entry fields: {sorted(set(provider) - allowed_fields)}")
        for field, accepted in (("servingAdapter", "separate-id/1"), ("readProfile", "bounded-immutable/1")):
            if field in provider and provider[field] != accepted:
                raise ValueError(f"unsupported {field}: {provider[field]!r}")
        bounded = provider.get("readProfile") == "bounded-immutable/1"
        if bounded:
            if provider.get("servingAdapter") != "separate-id/1":
                raise ValueError("bounded-immutable/1 requires separate-id/1")
            pin = provider.get("publisherManifest")
            if not isinstance(pin, dict) or set(pin) != {"path", "sha256", "bytes"}:
                raise ValueError("bounded-immutable/1 requires closed publisherManifest path/sha256/bytes")
            safe_relative_path(pin["path"], "publisherManifest path")
            if not isinstance(pin["sha256"], str) or not SHA256_PATTERN.fullmatch(pin["sha256"]) or type(pin["bytes"]) is not int or not 0 < pin["bytes"] <= 2 * 1024 * 1024:
                raise ValueError("invalid publisherManifest identity")
        elif "publisherManifest" in provider:
            raise ValueError("publisherManifest requires bounded-immutable/1")
        database_id = provider.get("id")
        if not isinstance(database_id, str) or not DATABASE_ID_PATTERN.fullmatch(database_id):
            raise ValueError(f"invalid provider database id: {database_id!r}")
        if database_id in seen_ids:
            raise ValueError(f"duplicate provider database id: {database_id}")
        seen_ids.add(database_id)
        repository = provider.get("repository")
        if not isinstance(repository, str) or not REPOSITORY_PATTERN.fullmatch(repository):
            raise ValueError(f"invalid GitHub repository for {database_id}")
        if not isinstance(provider.get("revision"), str) or not COMMIT_PATTERN.fullmatch(provider["revision"]):
            raise ValueError(f"{database_id} revision must be an immutable full Git commit SHA")
        if provider.get("recordKeyFormat") not in ("natural", "legacy"):
            raise ValueError(f"{database_id} recordKeyFormat must be natural or legacy")
        if "requirePublishedQuery" in provider and not isinstance(provider["requirePublishedQuery"], bool):
            raise ValueError(f"{database_id} requirePublishedQuery must be a boolean")
        smoke_recordsets = provider.get("smokeRecordsets")
        if smoke_recordsets is not None and (not isinstance(smoke_recordsets, list) or not smoke_recordsets or any(not isinstance(name, str) or not name for name in smoke_recordsets) or len(set(smoke_recordsets)) != len(smoke_recordsets)):
            raise ValueError(f"{database_id} smokeRecordsets must be a non-empty list of unique recordset names")
        empty_recordsets = provider.get("emptyRecordsets", [])
        if not isinstance(empty_recordsets, list) or any(not isinstance(name, str) or not name for name in empty_recordsets) or len(set(empty_recordsets)) != len(empty_recordsets):
            raise ValueError(f"{database_id} emptyRecordsets must be a list of unique recordset names")
        if set(smoke_recordsets or ()) & set(empty_recordsets):
            raise ValueError(f"{database_id} recordsets cannot be both queried and expected empty")
        blob_smoke_fields = provider.get("blobSmokeFields", {})
        if not isinstance(blob_smoke_fields, dict) or any(
            not isinstance(recordset, str) or not recordset
            or not isinstance(columns, list) or not columns
            or any(not isinstance(column, str) or not column for column in columns)
            or len(set(columns)) != len(columns)
            for recordset, columns in blob_smoke_fields.items()
        ):
            raise ValueError(f"{database_id} blobSmokeFields must map recordset names to unique column-name lists")
        if set(blob_smoke_fields) - set(smoke_recordsets or ()):
            raise ValueError(f"{database_id} BLOB smoke recordsets must also be queried")
        foreign_key_smoke = provider.get("foreignKeySmoke", [])
        if not isinstance(foreign_key_smoke, list):
            raise ValueError(f"{database_id} foreignKeySmoke must be a list")
        seen_relationships: set[tuple[str, str, str, str]] = set()
        for relationship in foreign_key_smoke:
            if not isinstance(relationship, dict) or set(relationship) != {"sourceRecordset", "sourceField", "targetRecordset", "targetField"}:
                raise ValueError(f"{database_id} foreignKeySmoke entries must name sourceRecordset/sourceField and targetRecordset/targetField")
            source_recordset, source_field = relationship["sourceRecordset"], relationship["sourceField"]
            target_recordset, target_field = relationship["targetRecordset"], relationship["targetField"]
            if any(not isinstance(value, str) or not value.strip() for value in (source_recordset, source_field, target_recordset, target_field)):
                raise ValueError(f"{database_id} foreignKeySmoke entries must use non-empty native names")
            if source_recordset not in (smoke_recordsets or ()) or target_recordset not in (smoke_recordsets or ()):
                raise ValueError(f"{database_id} foreignKeySmoke source and target recordsets must both be queried")
            identity = (source_recordset, source_field, target_recordset, target_field)
            if identity in seen_relationships:
                raise ValueError(f"{database_id} repeats foreignKeySmoke relationship {identity!r}")
            seen_relationships.add(identity)
        origins = provider.get("corsOrigins")
        if not isinstance(origins, list) or not origins:
            raise ValueError(f"{database_id} must declare CORS origins")
        for origin in origins:
            if not isinstance(origin, str) or origin != f"https://{urlsplit(origin).netloc}" or urlsplit(origin).scheme != "https":
                raise ValueError(f"{database_id} CORS origins must be HTTPS origins")
        if len(origins) != len(set(origins)):
            raise ValueError(f"{database_id} has duplicate CORS origins")
        storage_engine = provider.get("storageEngine", "sqlite")
        if storage_engine not in ("sqlite", "ingitdb"):
            raise ValueError(f"{database_id} has unsupported storageEngine")
        files = provider.get("files")
        expected_files = {"manifest", "contract", "checksums", "databaseManifest", "license"} | ({"artifact"} if storage_engine == "sqlite" else set())
        if not isinstance(files, dict) or set(files) != expected_files:
            raise ValueError(f"{database_id} must pin exactly its required provider files")
        for file_key, descriptor in files.items():
            if not isinstance(descriptor, dict):
                raise ValueError(f"{database_id} {file_key} pin must be an object")
            fields = {"path", "sha256", "bytes"} | ({"compression", "encodedPath", "decodedBytes", "decodedSha256", "chunks"} if file_key == "artifact" else set())
            if set(descriptor) - fields:
                raise ValueError(f"{database_id} {file_key} pin has unknown fields")
            if "bytes" in descriptor and file_key != "artifact" and (type(descriptor["bytes"]) is not int or not 0 < descriptor["bytes"] <= MAX_ARTIFACT_BYTES):
                raise ValueError(f"{database_id} {file_key} pin has invalid bytes")
            safe_relative_path(descriptor.get("path"), f"{database_id} {file_key} path")
            if not isinstance(descriptor.get("sha256"), str) or not SHA256_PATTERN.fullmatch(descriptor["sha256"]):
                raise ValueError(f"{database_id} {file_key} must pin a SHA-256")
        if storage_engine == "ingitdb":
            native = provider.get("nativeFiles")
            metadata = provider.get("metadataFiles")
            if not isinstance(native, list) or len(native) != 8 or not isinstance(metadata, list) or len(metadata) != 3:
                raise ValueError(f"{database_id} native inGitDB provider must pin eight store files and three metadata receipts")
            seen_paths: set[str] = set()
            for descriptor in native + metadata:
                if not isinstance(descriptor, dict) or set(descriptor) != {"path", "sha256", "bytes"}:
                    raise ValueError(f"{database_id} native pins must contain only path/sha256/bytes")
                path = safe_relative_path(descriptor.get("path"), f"{database_id} native path")
                if path in seen_paths or not isinstance(descriptor.get("sha256"), str) or not SHA256_PATTERN.fullmatch(descriptor["sha256"]) or type(descriptor.get("bytes")) is not int or not 0 < descriptor["bytes"] <= MAX_ARTIFACT_BYTES:
                    raise ValueError(f"{database_id} has an invalid or duplicate native pin")
                seen_paths.add(path)
            if any(not item["path"].startswith("ingitdb/") for item in native):
                raise ValueError(f"{database_id} native store pins must use the ingitdb/ source prefix")
            continue
        artifact = files["artifact"]
        if artifact.get("compression") is None:
            if type(artifact.get("bytes")) is not int or artifact["bytes"] <= 0 or artifact["bytes"] > MAX_ARTIFACT_BYTES:
                raise ValueError(f"{database_id} artifact must be between 1 byte and {MAX_ARTIFACT_BYTES} bytes")
        else:
            if artifact.get("compression") != "gzip":
                raise ValueError(f"{database_id} artifact compression must be gzip when specified")
            encoded_path = safe_relative_path(artifact.get("encodedPath"), f"{database_id} artifact encodedPath")
            if encoded_path != artifact["path"] + ".gz":
                raise ValueError(f"{database_id} gzip artifact encodedPath must be logical path plus .gz")
            if type(artifact.get("bytes")) is not int or artifact["bytes"] <= 0:
                raise ValueError(f"{database_id} gzip artifact bytes must describe the encoded stream")
            if artifact["bytes"] > MAX_ENCODED_ARTIFACT_BYTES:
                raise ValueError(f"{database_id} gzip artifact exceeds the {MAX_ENCODED_ARTIFACT_BYTES} byte encoded-stream limit")
            if type(artifact.get("decodedBytes")) is not int or not 0 < artifact["decodedBytes"] <= MAX_DECODED_ARTIFACT_BYTES:
                raise ValueError(f"{database_id} gzip artifact decodedBytes must be between 1 byte and {MAX_DECODED_ARTIFACT_BYTES} bytes")
            if not isinstance(artifact.get("decodedSha256"), str) or not SHA256_PATTERN.fullmatch(artifact["decodedSha256"]):
                raise ValueError(f"{database_id} gzip artifact must pin the decoded SHA-256")
            chunks = artifact.get("chunks")
            if chunks is not None:
                if not isinstance(chunks, list) or not chunks:
                    raise ValueError(f"{database_id} gzip artifact chunks must be a non-empty list")
                seen_chunks: set[str] = set()
                encoded_bytes = 0
                for chunk in chunks:
                    if not isinstance(chunk, dict) or set(chunk) != {"path", "sha256", "bytes"}:
                        raise ValueError(f"{database_id} gzip artifact chunks must be objects")
                    chunk_path = safe_relative_path(chunk.get("path"), f"{database_id} artifact chunk path")
                    if chunk_path in seen_chunks:
                        raise ValueError(f"{database_id} gzip artifact has a duplicate chunk path: {chunk_path}")
                    seen_chunks.add(chunk_path)
                    size = chunk.get("bytes")
                    if type(size) is not int or not 0 < size <= MAX_ARTIFACT_BYTES:
                        raise ValueError(f"{database_id} artifact chunks must be between 1 byte and {MAX_ARTIFACT_BYTES} bytes")
                    if not isinstance(chunk.get("sha256"), str) or not SHA256_PATTERN.fullmatch(chunk["sha256"]):
                        raise ValueError(f"{database_id} artifact chunks must pin a SHA-256")
                    encoded_bytes += size
                if encoded_bytes != artifact["bytes"]:
                    raise ValueError(f"{database_id} artifact chunk byte totals do not match the encoded stream")
            elif artifact["bytes"] > MAX_ARTIFACT_BYTES:
                raise ValueError(f"{database_id} unsplit gzip artifact exceeds the {MAX_ARTIFACT_BYTES} byte file limit")
        origins = provider.get("corsOrigins")
        if not isinstance(origins, list) or not origins:
            raise ValueError(f"{database_id} must declare CORS origins")
        provider_origins: set[str] = set()
        for origin in origins:
            if not isinstance(origin, str):
                raise ValueError(f"{database_id} CORS origins must be HTTPS origins")
            parsed_origin = urlsplit(origin)
            try:
                parsed_origin.port
            except ValueError as error:
                raise ValueError(f"{database_id} has invalid CORS origin {origin!r}") from error
            if parsed_origin.scheme != "https" or not parsed_origin.hostname or parsed_origin.username or parsed_origin.password or parsed_origin.path or parsed_origin.query or parsed_origin.fragment:
                raise ValueError(f"{database_id} CORS origins must be HTTPS origins")
            if origin in provider_origins:
                raise ValueError(f"{database_id} has duplicate CORS origin: {origin}")
            provider_origins.add(origin)
    if not providers:
        raise ValueError("provider inventory cannot be empty")
    return providers


class RefuseRedirects(urllib.request.HTTPRedirectHandler):
    """Never let Location expand the immutable inventory fetch allowlist."""

    def redirect_request(self, request, response, code, message, headers, new_url):
        response.close()
        raise ValueError(f"immutable provider redirect refused: HTTP {code}")


def immutable_url(repository: str, revision: str, relative_path: str) -> str:
    if not isinstance(repository, str) or not REPOSITORY_PATTERN.fullmatch(repository):
        raise ValueError("invalid immutable GitHub repository")
    if not isinstance(revision, str) or not COMMIT_PATTERN.fullmatch(revision):
        raise ValueError("revision must be an immutable full Git commit SHA")
    path = safe_relative_path(relative_path, "provider file path")
    url = f"{RAW_GITHUB_ORIGIN}/{repository}/{revision}/" + "/".join(quote(segment, safe="") for segment in path.split("/"))
    parsed = urlsplit(url)
    if parsed.scheme != "https" or parsed.netloc != "raw.githubusercontent.com" or parsed.query or parsed.fragment:
        raise ValueError("immutable provider URL must use the exact raw GitHub HTTPS origin")
    return url


def default_fetch(repository: str, revision: str, relative_path: str) -> bytes:
    request = urllib.request.Request(immutable_url(repository, revision, relative_path), headers={"User-Agent": "DemoDB-Cloud-fixture-verifier/1"})
    # A dedicated opener prevents application/global opener configuration from
    # introducing redirects, cookies, or a looser provider origin policy.
    opener = urllib.request.build_opener(RefuseRedirects())
    try:
        with opener.open(request, timeout=30) as response:
            data = response.read(MAX_ARTIFACT_BYTES + 1)
    except urllib.error.HTTPError as error:
        # Do not copy a remote error body/Location into logs; close without reading.
        status = error.code
        error.close()
        raise ValueError(f"immutable provider request failed: HTTP {status}") from None
    if len(data) > MAX_ARTIFACT_BYTES:
        raise ValueError(f"provider file exceeds the {MAX_ARTIFACT_BYTES} byte build limit: {relative_path}")
    return data


def _fetch_verified_artifact(
    provider: dict[str, Any],
    fetch: Callable[[str, str, str], bytes],
    checksums: dict[str, Any],
    artifact_output: Path | None,
) -> bytes | Path:
    descriptor = provider["files"]["artifact"]
    database_id = provider["id"]
    repository = provider["repository"]
    revision = provider["revision"]
    advertised_files = checksums.get("files", {})
    if descriptor.get("compression") is None:
        data = fetch(repository, revision, descriptor["path"])
        verify_blob(data, descriptor["sha256"], descriptor["bytes"], f"{database_id} artifact")
        advertised = advertised_files.get(descriptor["path"])
        if not isinstance(advertised, dict) or advertised.get("sha256") != descriptor["sha256"] or advertised.get("bytes") != descriptor["bytes"]:
            raise ValueError(f"{database_id} artifact pin disagrees with provider checksums")
        return data

    if artifact_output is None:
        raise ValueError(f"{database_id} compressed artifact validation needs a temporary output path")
    chunks = descriptor.get("chunks")
    physical = chunks or [{"path": descriptor["encodedPath"], "bytes": descriptor["bytes"], "sha256": descriptor["sha256"]}]
    artifact_output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(prefix=f"{database_id}-", suffix=".sqlite.gz", dir=artifact_output.parent, delete=False) as stream_file:
        encoded_path = Path(stream_file.name)
    encoded_digest = hashlib.sha256()
    encoded_bytes = 0
    try:
        with encoded_path.open("wb") as stream:
            for chunk in physical:
                path = safe_relative_path(chunk["path"], f"{database_id} artifact chunk path")
                data = fetch(repository, revision, path)
                verify_blob(data, chunk["sha256"], chunk["bytes"], f"{database_id} artifact chunk {path}")
                advertised = advertised_files.get(path)
                if not isinstance(advertised, dict) or advertised.get("sha256") != chunk["sha256"] or advertised.get("bytes") != chunk["bytes"]:
                    raise ValueError(f"{database_id} artifact chunk {path} disagrees with provider checksums")
                if len(data) > MAX_ARTIFACT_BYTES or encoded_bytes + len(data) > MAX_ENCODED_ARTIFACT_BYTES or encoded_bytes + len(data) > descriptor["bytes"]:
                    raise ValueError(f"{database_id} encoded artifact exceeds its pinned size")
                stream.write(data)
                encoded_digest.update(data)
                encoded_bytes += len(data)
        verify_blob_digest(encoded_digest.hexdigest(), encoded_bytes, descriptor["sha256"], descriptor["bytes"], f"{database_id} encoded artifact")

        decoded_digest = hashlib.sha256()
        decoded_bytes = 0
        with gzip.open(encoded_path, "rb") as compressed, artifact_output.open("wb") as decoded:
            while chunk := compressed.read(min(1024 * 1024, descriptor["decodedBytes"] - decoded_bytes + 1, MAX_DECODED_ARTIFACT_BYTES - decoded_bytes + 1)):
                decoded_bytes += len(chunk)
                if decoded_bytes > descriptor["decodedBytes"] or decoded_bytes > MAX_DECODED_ARTIFACT_BYTES:
                    raise ValueError(f"{database_id} decoded artifact exceeds its pinned size")
                decoded_digest.update(chunk)
                decoded.write(chunk)
        verify_blob_digest(decoded_digest.hexdigest(), decoded_bytes, descriptor["decodedSha256"], descriptor["decodedBytes"], f"{database_id} decoded artifact")
        return artifact_output
    except Exception:
        artifact_output.unlink(missing_ok=True)
        raise
    finally:
        encoded_path.unlink(missing_ok=True)


def verify_blob_digest(actual_hash: str, actual_bytes: int, expected_hash: str, expected_bytes: int, context: str) -> None:
    if actual_hash != expected_hash:
        raise ValueError(f"{context} SHA-256 does not match its immutable inventory pin")
    if actual_bytes != expected_bytes:
        raise ValueError(f"{context} is {actual_bytes} bytes, expected {expected_bytes}")


def validate_native_provider(provider: dict[str, Any], fetch: Callable[[str, str, str], bytes], fetched: dict[str, Any]) -> dict[str, Any]:
    """Validate the closed native inGitDB store without transforming its bytes."""
    database_id, repository, revision = provider["id"], provider["repository"], provider["revision"]
    native: dict[str, bytes] = {}
    metadata: dict[str, bytes] = {}
    for target, pins in ((native, provider["nativeFiles"]), (metadata, provider["metadataFiles"])):
        for pin in pins:
            data = fetch(repository, revision, pin["path"])
            verify_blob(data, pin["sha256"], pin["bytes"], f"{database_id} {pin['path']}")
            target[pin["path"]] = data
    manifest = read_json(fetched["manifest"], f"{database_id} manifest")
    contract = read_json(fetched["contract"], f"{database_id} contract")
    descriptor = read_json(fetched["databaseManifest"], f"{database_id} descriptor")
    checksums = read_json(fetched["checksums"], f"{database_id} checksums")
    if manifest.get("id") != database_id or manifest.get("storage") != {"engine": "ingitdb", "path": "ingitdb"} or contract.get("schema", {}).get("database", {}).get("id") != database_id:
        raise ValueError(f"{database_id} native provider identity/storage mismatch")
    if descriptor.get("localId") != database_id or descriptor.get("capabilities") != {"read": True, "query": True, "write": False}:
        raise ValueError(f"{database_id} native public descriptor is not read-only/queryable")
    schema = read_json(metadata["metadata/schema.json"], f"{database_id} pinned schema")
    if schema != contract.get("schema"):
        raise ValueError(f"{database_id} pinned schema disagrees with contract")
    source_pin = next(pin for pin in provider["metadataFiles"] if pin["path"] == "source-manifest.json")
    if manifest.get("source", {}).get("sha256") != source_pin["sha256"]:
        raise ValueError(f"{database_id} provider source digest disagrees with pinned source manifest")
    advertised = checksums.get("files", {})
    for pin in provider["nativeFiles"]:
        if advertised.get(pin["path"]) != {"sha256": pin["sha256"], "bytes": pin["bytes"]}:
            raise ValueError(f"{database_id} checksums disagree with {pin['path']}")
    expected_root = {
        "baseline": "collections/baseline",
        "recovery11045example": "collections/recovery11045example",
        "recovery11062example": "collections/recovery11062example",
    }
    expected_paths = {"ingitdb/.ingitdb/settings.yaml", "ingitdb/.ingitdb/root-collections.yaml"}
    for path in expected_root.values():
        expected_paths.update({f"ingitdb/{path}/.collection/definition.yaml", f"ingitdb/{path}/records.jsonl"})
    if set(native) != expected_paths or len(provider["nativeFiles"]) != len(expected_paths):
        raise ValueError(f"{database_id} native tree must contain exactly the eight declared members")
    root = read_json(native["ingitdb/.ingitdb/root-collections.yaml"], f"{database_id} root collections")
    if root != expected_root:
        raise ValueError(f"{database_id} root collections must be the exact three public IDs")
    schema_tables = {table["name"]: table for table in contract["schema"]["tables"]}
    if set(schema_tables) != set(expected_root):
        raise ValueError(f"{database_id} contract must contain exactly the three public collections")
    for collection_id, collection_path in expected_root.items():
        definition_path = f"ingitdb/{collection_path}/.collection/definition.yaml"
        records_path = f"ingitdb/{collection_path}/records.jsonl"
        definition = read_json(native[definition_path], f"{database_id} {collection_id} definition")
        if definition.get("record_file") != {"name": "records.jsonl", "type": "[]map[string]any", "format": "jsonl"} or definition.get("primary_key") != ["event_id"]:
            raise ValueError(f"{database_id} {collection_id} has an incompatible record file or primary key")
        columns = definition.get("columns")
        if not isinstance(columns, dict) or columns.get("event_id", {}).get("required") is not True or columns["event_id"].get("min_length") != 1:
            raise ValueError(f"{database_id} {collection_id} does not require a nonblank event_id")
        table = schema_tables.get(collection_id)
        if table is None or set(columns) != {column["name"] for column in table["columns"]}:
            raise ValueError(f"{database_id} {collection_id} definition disagrees with contract schema")
        contract_columns = {column["name"]: column for column in table["columns"]}
        for name, spec in columns.items():
            column = contract_columns[name]
            if (set(spec) - {"type", "required", "min_length"} or
                    spec.get("type") not in {"string", "int", "bool"} or
                    spec.get("type") != column.get("type") or
                    spec.get("required") is not (not column.get("nullable", True)) or
                    ("min_length" in spec and (spec["type"] != "string" or type(spec["min_length"]) is not int or spec["min_length"] < 0))):
                raise ValueError(f"{database_id} {collection_id}.{name} definition disagrees with contract constraints")
        if definition.get("columns_order") != list(columns):
            raise ValueError(f"{database_id} {collection_id} column order differs from its definition")
        seen: set[str] = set()
        rows = native[records_path].splitlines()
        for line in rows:
            record = read_json(line, f"{database_id} {collection_id} record")
            if set(record) != set(columns):
                raise ValueError(f"{database_id} {collection_id} record differs from its definition")
            key = record.get("event_id")
            if not isinstance(key, str) or not key.strip() or key in seen:
                raise ValueError(f"{database_id} {collection_id} has blank or duplicate event_id")
            seen.add(key)
            for name, spec in columns.items():
                value = record[name]
                expected = spec.get("type")
                if (expected == "string" and (not isinstance(value, str) or len(value) < spec.get("min_length", 0))) or (expected == "int" and (type(value) is not int)) or (expected == "bool" and type(value) is not bool):
                    raise ValueError(f"{database_id} {collection_id}.{name} violates its definition")
        if len(rows) != table.get("rowCount"):
            raise ValueError(f"{database_id} {collection_id} row count disagrees with contract")
    source = read_json(metadata["source-manifest.json"], f"{database_id} source manifest")
    source_files = manifest["source"]["files"]
    if set(source_files) != set(expected_root) or len(source.get("logs", [])) != len(expected_root):
        raise ValueError(f"{database_id} source manifest must describe exactly three distinct original logs")
    source_prefix = str(Path(manifest["source"]["path"]).parent) + "/"
    matched = set()
    for name, details in source_files.items():
        source_path = details["sourcePath"]
        if not source_path.startswith(source_prefix):
            raise ValueError(f"{database_id} source path escapes the original fixture")
        logs = [item for item in source["logs"] if source_prefix + item.get("path", "") == source_path]
        pin = next((pin for pin in provider["nativeFiles"] if pin["path"] == details["path"]), None)
        expected_class = "baseline" if name == "baseline" else "recovery-example"
        if (len(logs) != 1 or source_path in matched or pin is None or
                details["path"] != f"ingitdb/{expected_root[name]}/records.jsonl" or
                details.get("classification") != expected_class or
                details.get("rowCount") != schema_tables[name]["rowCount"] or
                any(details.get(field) != pin[field] or logs[0].get(field) != pin[field] for field in ("sha256", "bytes"))):
            raise ValueError(f"{database_id} source manifest disagrees with original native file {name}")
        matched.add(source_path)
    fetched["nativeFiles"], fetched["metadataFiles"] = native, metadata
    return fetched


def validate_provider(
    provider: dict[str, Any],
    fetch: Callable[[str, str, str], bytes],
    artifact_output: Path | None = None,
) -> dict[str, Any]:
    database_id = provider["id"]
    repository = provider["repository"]
    revision = provider["revision"]
    fetched: dict[str, Any] = {}
    for key, descriptor in provider["files"].items():
        if key == "artifact":
            continue
        data = fetch(repository, revision, descriptor["path"])
        if len(data) > MAX_ARTIFACT_BYTES:
            raise ValueError(f"{database_id} {key} exceeds the physical file limit")
        expected_bytes = descriptor.get("bytes")
        verify_blob(data, descriptor["sha256"], expected_bytes, f"{database_id} {key}")
        fetched[key] = data

    if provider.get("storageEngine", "sqlite") == "ingitdb":
        return validate_native_provider(provider, fetch, fetched)

    manifest = read_json(fetched["manifest"], f"{database_id} provider manifest")
    contract = read_json(fetched["contract"], f"{database_id} provider contract")
    checksums = read_json(fetched["checksums"], f"{database_id} provider checksums")

    database_manifest = read_json(fetched["databaseManifest"], f"{database_id} OVDB database manifest")
    if provider.get("readProfile") == "bounded-immutable/1":
        validate_descriptor_members(database_manifest)
    if manifest.get("id") != database_id or contract.get("manifest", {}).get("id") != database_id:
        raise ValueError(f"{database_id} provider ID differs between manifest and contract")
    if contract.get("contractVersion") != 1 or checksums.get("contractVersion") != 1:
        raise ValueError(f"{database_id} provider contract version is unsupported")
    ovdb_capabilities = manifest.get("capabilities", {}).get("ovdb", {})
    if ovdb_capabilities.get("readOnly") is not True:
        raise ValueError(f"{database_id} provider does not declare read-only OVDB access")
    if provider.get("requirePublishedQuery", True) and (ovdb_capabilities.get("available") is not True or ovdb_capabilities.get("query") is not True):
        raise ValueError(f"{database_id} provider does not declare OVDB query availability")
    if database_manifest.get("format") != "ovdb-database/draft-1" or database_manifest.get("localId") != database_id:
        raise ValueError(f"{database_id} OVDB database manifest has incompatible identity")
    public_capabilities = database_manifest.get("capabilities", {})
    deployment_engine = database_manifest.get("deployment", {}).get("engine")
    expected_engine = "openvaultdb-go" if database_id in ("shipping", "shipping-logs") else "sqlite"
    if deployment_engine != expected_engine or public_capabilities.get("read") is not True or public_capabilities.get("write") is not False:
        raise ValueError(f"{database_id} OVDB database manifest is not a compatible read-only provider")
    if provider.get("requirePublishedQuery", True) and public_capabilities.get("query") is not True:
        raise ValueError(f"{database_id} OVDB database manifest does not advertise queries")
    bounded = provider.get("readProfile") == "bounded-immutable/1"
    publisher = None
    if bounded:
        pin = provider["publisherManifest"]
        data = fetch(repository, revision, pin["path"])
        verify_blob(data, pin["sha256"], pin["bytes"], f"{database_id} publisherManifest")
        fetched["publisherManifest"] = data
        publisher = parse_publisher(data)
        fetched["_publisher"] = publisher
        if publisher.get("id") != database_id:
            raise ValueError("publisher manifest database identity mismatch")
    server_id = database_manifest.get("serverId", "")
    expected_api_url = f"https://cloud.openvaultdb.com/v1/databases/{database_id}" if bounded else f"{server_id.rstrip('/')}/v1/databases/{database_id}"
    expected_db_base_url = f"https://cloud.openvaultdb.com/ovdb/dbs/{database_id}" if bounded else f"{server_id.rstrip('/')}/db/{database_id}/"
    if not server_id.startswith("https://") or database_manifest.get("apiUrl") != expected_api_url or database_manifest.get("serverDbBaseUrl") != expected_db_base_url:
        raise ValueError(f"{database_id} OVDB database manifest routes do not match its provider ID")
    expected_homepage = manifest.get("homepage") or f"https://{manifest.get('siteHost')}/"
    if bounded and not manifest.get("siteHost"):
        # No-website publishers use their pinned repository, never a fabricated host.
        expected_homepage = f"https://github.com/{repository}"
        if publisher.get("homepage") != expected_homepage or ("homepage" in manifest and manifest["homepage"] != expected_homepage):
            raise ValueError(f"{database_id} no-website homepage must be its pinned HTTPS repository")
    if database_manifest.get("id") != ovdb_capabilities.get("canonicalUrl") or database_manifest.get("homepage") != expected_homepage:
        raise ValueError(f"{database_id} public identity or homepage differs from its provider manifest")
    if not isinstance(contract.get("schema", {}).get("tables"), list):
        raise ValueError(f"{database_id} provider contract has no native table schema")
    if contract["schema"].get("database", {}).get("id") != database_id:
        raise ValueError(f"{database_id} provider native schema declares a different database ID")
    tables = contract["schema"]["tables"]
    physical_tables = [table for table in tables if table.get("kind", "table") == "table" and isinstance(table.get("name"), str)]
    table_names = [table["name"] for table in physical_tables]
    if not table_names:
        raise ValueError(f"{database_id} provider contract has no physical recordset to query")
    if bounded:
        selected = publisher["recordsets"]
        public_recordsets = database_manifest.get("recordsets", [])
        public_names = [item.get("name") for item in public_recordsets if isinstance(item, dict) and item.get("kind", "table") == "table"]
        if len(public_names) != len(public_recordsets):
            raise ValueError("public descriptor must contain only selected physical tables")
        if set(selected) - set(table_names) or len(public_names) != len(selected) or set(public_names) != set(selected):
            raise ValueError("publisher recordsets disagree with native/public descriptor selection")
        for item in public_recordsets:
            native = next(table for table in physical_tables if table["name"] == item["name"])
            if item.get("columns") != native.get("columns") or item.get("primaryKey", []) != native.get("primaryKey", []) or item.get("foreignKeys", []) != native.get("foreignKeys", []) or item.get("rowCount") != native.get("rowCount"):
                raise ValueError("public descriptor disagrees with selected native schema")
        public_pin = provider["files"]["databaseManifest"]
        if set(public_pin) != {"path", "sha256", "bytes"} or type(public_pin["bytes"]) is not int or not 0 < public_pin["bytes"] <= 2 * 1024 * 1024:
            raise ValueError("bounded provider requires a closed public descriptor pin")
        if publisher.get("homepage") != database_manifest.get("homepage") or publisher.get("url") != expected_db_base_url or database_manifest.get("id") != expected_db_base_url or server_id != "https://cloud.openvaultdb.com/ovdb/":
            raise ValueError("publisher public route identity mismatch")
        expected_deployment = {"engine": "sqlite", "url": expected_db_base_url, "discovery": "https://cloud.openvaultdb.com/.well-known/openvaultdb"}
        if database_manifest.get("deployment") != expected_deployment or publisher.get("deployment") != {**expected_deployment, "recordset_page": expected_db_base_url + "/collections/{name}"}:
            raise ValueError("publisher/descriptor deployment routes mismatch")
        available, query = ovdb_capabilities.get("available"), ovdb_capabilities.get("query")
        if type(available) is not bool or type(query) is not bool or available != query or public_capabilities.get("query") is not query or provider.get("requirePublishedQuery", True) != query:
            raise ValueError("publisher/descriptor query flags disagree")
        physical_tables = [table for table in physical_tables if table["name"] in selected]
        table_names = selected
    smoke_recordset = next((table["name"] for table in physical_tables if table.get("rowCount", 0) > 0), table_names[0])
    smoke_recordsets = provider.get("smokeRecordsets", [smoke_recordset])
    unknown_smoke_recordsets = set(smoke_recordsets) - set(table_names)
    if unknown_smoke_recordsets:
        raise ValueError(f"{database_id} smoke recordsets are absent from its native table schema: {sorted(unknown_smoke_recordsets)}")
    empty_smoke_recordsets = [
        table["name"] for table in physical_tables
        if table["name"] in smoke_recordsets and table.get("rowCount", 0) == 0
    ]
    if empty_smoke_recordsets:
        raise ValueError(f"{database_id} smoke recordsets have no rows in the pinned schema: {sorted(empty_smoke_recordsets)}")
    empty_recordsets = provider.get("emptyRecordsets", [])
    unknown_empty_recordsets = set(empty_recordsets) - set(table_names)
    if unknown_empty_recordsets:
        raise ValueError(f"{database_id} expected-empty recordsets are absent from its native table schema: {sorted(unknown_empty_recordsets)}")
    if set(smoke_recordsets) & set(empty_recordsets):
        raise ValueError(f"{database_id} recordsets cannot be both queried and expected empty")
    nonempty_expected_empty = [
        table["name"] for table in physical_tables
        if table["name"] in empty_recordsets and table.get("rowCount") != 0
    ]
    if nonempty_expected_empty:
        raise ValueError(f"{database_id} expected-empty recordsets contain rows in the pinned schema: {sorted(nonempty_expected_empty)}")
    blob_smoke_fields = provider.get("blobSmokeFields", {})
    for recordset, columns in blob_smoke_fields.items():
        table = next((item for item in physical_tables if item["name"] == recordset), None)
        native_columns = {column["name"]: column.get("type", "").upper() for column in table.get("columns", [])} if table else {}
        for column in columns:
            if native_columns.get(column) not in {"BLOB", "BINARY", "VARBINARY"}:
                raise ValueError(f"{database_id} BLOB smoke field {recordset}.{column} is not declared as binary in its native schema")

    native_foreign_keys = {
        (table.get("name"), foreign_key.get("column"), foreign_key.get("table"), foreign_key.get("referencedColumn"))
        for table in physical_tables
        for foreign_key in table.get("foreignKeys", [])
    }
    for relationship in provider.get("foreignKeySmoke", []):
        source_recordset = relationship["sourceRecordset"]
        source_field = relationship["sourceField"]
        target_recordset = relationship["targetRecordset"]
        target_field = relationship["targetField"]
        if (source_recordset, source_field, target_recordset, target_field) not in native_foreign_keys:
            raise ValueError(
                f"{database_id} foreignKeySmoke {source_recordset}.{source_field} -> {target_recordset}.{target_field} "
                "is not a declared native foreign key"
            )

    source = manifest.get("source", {})
    provenance = database_manifest.get("provenance", {})
    if provenance.get("repository") != source.get("repository") or provenance.get("revision") != source.get("revision") or provenance.get("path") != source.get("path"):
        raise ValueError(f"{database_id} public provenance differs from its provider source manifest")
    license_descriptor = provider["files"]["license"]
    if source.get("licenseFile") != license_descriptor["path"]:
        raise ValueError(f"{database_id} pinned license path differs from its provider manifest")
    if not isinstance(source.get("license"), str) or not source["license"].strip():
        raise ValueError(f"{database_id} provider manifest has no data license")
    if database_manifest.get("licences", {}).get("data") != source["license"]:
        raise ValueError(f"{database_id} public database manifest license differs from provider metadata")
    if database_manifest.get("provenance", {}).get("license") != source["license"]:
        raise ValueError(f"{database_id} public provenance license differs from provider metadata")

    artifact_descriptor = provider["files"]["artifact"]
    artifact_hash = artifact_descriptor.get("decodedSha256", artifact_descriptor["sha256"])
    validate_sqlite_export(database_id, contract.get("exports", []), artifact_descriptor)
    source_hash = source.get("sha256") or source.get("databaseSha256")
    if source_hash and source_hash != artifact_hash:
        raise ValueError(f"{database_id} source fixture hash differs from the published SQLite artifact")
    if provenance.get("sha256") != artifact_hash:
        raise ValueError(f"{database_id} public provenance digest differs from the published SQLite artifact")

    hosts = {host for host in (manifest.get("siteHost"), *manifest.get("aliases", [])) if host}
    declared_origins = {origin.removeprefix("https://") for origin in provider["corsOrigins"]}
    if not hosts.issubset(declared_origins):
        raise ValueError(f"{database_id} CORS origins omit a provider host or alias: {sorted(hosts - declared_origins)}")
    fetched["artifact"] = _fetch_verified_artifact(provider, fetch, checksums, artifact_output)
    return fetched


# Narrow operator declarations derived from verified publisher data metadata.
# They remain server-declared evidence, not publisher attestation or legal advice.
SHOWCASE_RECORDSETS = {"northwind": ("Customers",), "chinook": ("Customer", "Invoice")}


def scoped_recordset_licenses(provider: dict[str, Any], fetched: dict[str, Any]) -> dict[str, dict[str, str]] | None:
    names = SHOWCASE_RECORDSETS.get(provider["id"])
    if names is None:
        return None
    database_id = provider["id"]
    if provider["repository"] != "demo-db/" + database_id:
        raise ValueError("scoped rights require the pinned publisher repository")
    source = read_json(fetched["manifest"], "provider manifest")["source"]
    public = read_json(fetched["databaseManifest"], "public database manifest")
    contract = read_json(fetched["contract"], "provider contract")
    descriptor = provider["files"]["license"]
    if descriptor["path"] != "data-source/LICENSE.md" or source.get("licenseFile") != descriptor["path"] or source.get("license") != "MIT" or public.get("licences", {}).get("data") != "MIT" or public.get("provenance", {}).get("license") != "MIT":
        raise ValueError("scoped rights require matching publisher data MIT declarations")
    for key in ("manifest", "databaseManifest", "contract", "license"):
        if hashlib.sha256(fetched[key]).hexdigest() != provider["files"][key]["sha256"]:
            raise ValueError("scoped rights input differs from its verified pin")
    native = {table["name"] for table in contract["schema"]["tables"] if table.get("kind", "table") == "table"}
    published = {table["name"] for table in public["recordsets"] if table.get("kind", "table") == "table"}
    if not set(names).issubset(native & published):
        raise ValueError("scoped rights recordset absent from native/public publisher scope")
    text = fetched["license"].decode("utf-8")
    if not text.strip() or len(fetched["license"]) > 65536:
        raise ValueError("scoped rights license text is empty or exceeds evidence bound")
    # Validate repository/revision/path with the existing immutable fetch contract.
    immutable_url(provider["repository"], provider["revision"], descriptor["path"])
    url = f"https://github.com/{provider['repository']}/blob/{provider['revision']}/{descriptor['path']}"
    return {name: {"spdx": "MIT", "url": url, "text": text} for name in names}


def validate_image_layout(output: Path) -> None:
    """The image contains flat provider files plus one closed native store."""
    for entry in output.iterdir():
        if entry.is_symlink() or (not entry.is_file() and entry.name != "shipping-logs") or entry.name.startswith("."):
            raise ValueError(f"image fixture output has an unapproved entry: {entry.name}")
        if entry.is_file() and entry.stat().st_nlink != 1:
            raise ValueError(f"image fixture output has a linked file: {entry.name}")
    native = output / "shipping-logs"
    if native.exists():
        expected = {
            ".ingitdb/settings.yaml", ".ingitdb/root-collections.yaml",
            "collections/baseline/.collection/definition.yaml", "collections/baseline/records.jsonl",
            "collections/recovery11045example/.collection/definition.yaml", "collections/recovery11045example/records.jsonl",
            "collections/recovery11062example/.collection/definition.yaml", "collections/recovery11062example/records.jsonl",
        }
        actual = set()
        for path in native.rglob("*"):
            if path.is_symlink() or (path.is_file() and path.stat().st_nlink != 1):
                raise ValueError("native inGitDB image tree contains a link")
            if path.is_file():
                actual.add(path.relative_to(native).as_posix())
        if actual != expected:
            raise ValueError("native inGitDB image tree differs from its exact eight-file inventory")
    if not (output / "inventory.json").is_file():
        raise ValueError("image fixture output has no inventory.json")


def prepare_inventory(inventory: Path, output: Path, local_root: Path | None = None) -> Path:
    document = read_json(inventory.read_bytes(), str(inventory))
    providers = validate_inventory(document)
    inventory_version = document["version"]
    output.mkdir(parents=True, exist_ok=True)
    runtime_databases: list[dict[str, Any]] = []
    for provider in providers:
        def fetch(repository: str, revision: str, relative_path: str) -> bytes:
            if local_root is None:
                return default_fetch(repository, revision, relative_path)
            immutable_url(repository, revision, relative_path)
            provider_root = (local_root / repository).resolve()
            local_path = (provider_root / relative_path).resolve()
            if not local_path.is_relative_to(provider_root):
                raise ValueError("local provider file escapes its repository")
            with local_path.open("rb") as local_file:
                data = local_file.read(MAX_ARTIFACT_BYTES + 1)
            if len(data) > MAX_ARTIFACT_BYTES:
                raise ValueError("local provider file exceeds the physical file limit")
            return data

        database_id = provider["id"]
        if provider.get("storageEngine", "sqlite") == "ingitdb":
            fetched = validate_provider(provider, fetch)
            native_root = output / database_id
            for pin in provider["nativeFiles"]:
                relative = pin["path"].removeprefix("ingitdb/")
                target = native_root / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(fetched["nativeFiles"][pin["path"]])
                target.chmod(0o444)
            for directory in sorted((path for path in native_root.rglob("*") if path.is_dir()), reverse=True):
                directory.chmod(0o555)
            native_root.chmod(0o555)
            for pin in provider["metadataFiles"]:
                target = output / f"{database_id}-{Path(pin['path']).name}"
                target.write_bytes(fetched["metadataFiles"][pin["path"]])
            manifest_path = output / f"{database_id}.yaml"
            manifest_path.write_text(
                "database:\n  id: shipping-logs\n  schema_mode: schemaless\nstorage:\n  engine: ingitdb\n  path: ./shipping-logs\n",
                encoding="utf-8",
            )
            license_name = f"{database_id}-UPSTREAM-LICENSE.md"
            (output / license_name).write_bytes(fetched["license"])
            source_sha = next(pin["sha256"] for pin in provider["metadataFiles"] if pin["path"] == "source-manifest.json")
            (output / f"{database_id}.source-sha256").write_text(source_sha + "\n", encoding="utf-8")
            runtime_databases.append({
                "storageEngine": "ingitdb", "nativeRoot": database_id, "nativeFiles": [
                    {**pin, "path": pin["path"].removeprefix("ingitdb/")} for pin in provider["nativeFiles"]
                ],
                "id": database_id, "manifest": manifest_path.name, "manifestSha256": prepare_fixture.sha256_file(manifest_path),
                "corsOrigins": provider["corsOrigins"], "providerRepository": provider["repository"], "providerRevision": provider["revision"],
                "sourceSha256": source_sha, "servingSha256": provider["files"]["contract"]["sha256"],
                "license": license_name, "licenseSha256": provider["files"]["license"]["sha256"],
                "smokeRecordset": provider["smokeRecordsets"][0], "smokeRecordsets": provider["smokeRecordsets"],
                "emptyRecordsets": [], "blobSmokeFields": {}, "foreignKeySmoke": [],
            })
            continue
        artifact = provider["files"]["artifact"]
        source_fd, source_name = tempfile.mkstemp(prefix=f"{database_id}-source-", suffix=".sqlite", dir=output)
        os.close(source_fd)
        source_path = Path(source_name)
        source_path.unlink(missing_ok=True)
        try:
            fetched = validate_provider(provider, fetch, source_path if artifact.get("compression") else None)
            tables = read_json(fetched["contract"], f"{database_id} provider contract")["schema"]["tables"]
            physical_tables = [table for table in tables if table.get("kind", "table") == "table" and isinstance(table.get("name"), str)]
            selected = fetched["_publisher"]["recordsets"] if provider.get("readProfile") else None
            if selected is not None:
                physical_tables = [table for table in physical_tables if table["name"] in selected]
            smoke_recordset = next((table["name"] for table in physical_tables if table.get("rowCount", 0) > 0), physical_tables[0]["name"])
            smoke_recordsets = provider.get("smokeRecordsets", [smoke_recordset])
            empty_recordsets = provider.get("emptyRecordsets", [])
            if artifact.get("compression") is None:
                with source_path.open("wb") as source_file:
                    source_file.write(fetched["artifact"])
            manifest_path = prepare_fixture.prepare(
                source_path,
                output,
                database_id,
                artifact.get("decodedSha256", artifact["sha256"]),
                provider["recordKeyFormat"],
                consume_source=True,
                serving_adapter=provider.get("servingAdapter"),
                selected_tables=selected,
                recordset_licenses=scoped_recordset_licenses(provider, fetched),
            )
        finally:
            source_path.unlink(missing_ok=True)
        license_name = f"{database_id}-UPSTREAM-LICENSE.md"
        (output / license_name).write_bytes(fetched["license"])
        runtime_databases.append(
            {
                "id": database_id,
                "manifest": manifest_path.name,
                "corsOrigins": provider["corsOrigins"],
                "providerRepository": provider["repository"],
                "providerRevision": provider["revision"],
                "sourceSha256": artifact.get("decodedSha256", artifact["sha256"]),
                "servingSha256": prepare_fixture.sha256_file(output / f"{database_id}.sqlite"),
                "license": license_name,
                "licenseSha256": provider["files"]["license"]["sha256"],
                "smokeRecordset": smoke_recordset,
                "smokeRecordsets": smoke_recordsets,
                "emptyRecordsets": empty_recordsets,
                "blobSmokeFields": provider.get("blobSmokeFields", {}),
                "foreignKeySmoke": provider.get("foreignKeySmoke", []),
            }
        )
        if inventory_version == 2:
            runtime_databases[-1]["manifestSha256"] = prepare_fixture.sha256_file(manifest_path)
            if provider.get("readProfile"):
                (output / f"{database_id}.publisher.yaml").write_bytes(fetched["publisherManifest"])
                (output / f"{database_id}.descriptor.json").write_bytes(fetched["databaseManifest"])
                runtime_databases[-1]["publicDescriptor"] = provider["files"]["databaseManifest"]
                runtime_databases[-1]["requirePublishedQuery"] = provider.get("requirePublishedQuery", True)
            for field in sorted(PREPARATION_FIELDS):
                if field in provider:
                    runtime_databases[-1][field] = provider[field]
    runtime_path = output / "inventory.json"
    runtime_path.write_text(json.dumps({"version": inventory_version, "databases": runtime_databases}, indent=2) + "\n", encoding="utf-8")
    for path in output.rglob("*"):
        if path.is_file():
            path.chmod(0o444)
        elif path.is_dir():
            path.chmod(0o555)
    validate_image_layout(output)
    return runtime_path


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, default=Path("providers.json"), help="immutable provider inventory JSON")
    parser.add_argument("--output-dir", type=Path, default=Path("fixture/output"), help="prepared serving fixture directory")
    parser.add_argument("--local-root", type=Path, help="development-only local provider repositories root")
    args = parser.parse_args()
    generated = prepare_inventory(args.inventory, args.output_dir, args.local_root)
    print(generated)


if __name__ == "__main__":
    main()
