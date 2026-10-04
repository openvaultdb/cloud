#!/usr/bin/env python3
"""Fetch and verify the immutable provider inventory for Cloud Run fixtures."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import tempfile
import urllib.request
from pathlib import Path, PurePosixPath
from typing import Any, Callable
from urllib.parse import urlsplit

import prepare_fixture


SHA256_PATTERN = re.compile(r"^[0-9a-f]{64}$")
COMMIT_PATTERN = re.compile(r"^[0-9a-f]{40}$")
DATABASE_ID_PATTERN = re.compile(r"^[a-z][a-z0-9-]{0,62}$")
MAX_ARTIFACT_BYTES = 25 * 1024 * 1024
RAW_GITHUB_ORIGIN = "https://raw.githubusercontent.com"


def safe_relative_path(value: Any, context: str) -> str:
    if not isinstance(value, str) or not value or "\\" in value:
        raise ValueError(f"{context} must be a non-empty POSIX relative path")
    path = PurePosixPath(value)
    if path.is_absolute() or any(part in ("", ".", "..") for part in path.parts):
        raise ValueError(f"{context} must not escape the provider root")
    return path.as_posix()


def read_json(data: bytes, context: str) -> dict[str, Any]:
    try:
        value = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ValueError(f"{context} is not valid UTF-8 JSON: {error}") from error
    if not isinstance(value, dict):
        raise ValueError(f"{context} must contain a JSON object")
    return value


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def verify_blob(data: bytes, expected_hash: str, expected_bytes: int | None, context: str) -> None:
    if sha256(data) != expected_hash:
        raise ValueError(f"{context} SHA-256 does not match its immutable inventory pin")
    if expected_bytes is not None and len(data) != expected_bytes:
        raise ValueError(f"{context} is {len(data)} bytes, expected {expected_bytes}")


def load_inventory(path: Path) -> list[dict[str, Any]]:
    document = read_json(path.read_bytes(), str(path))
    if document.get("version") != 1 or not isinstance(document.get("databases"), list):
        raise ValueError("provider inventory must use version 1 with a databases array")
    providers = document["databases"]
    seen_ids: set[str] = set()
    for provider in providers:
        if not isinstance(provider, dict):
            raise ValueError("each provider inventory entry must be a JSON object")
        database_id = provider.get("id")
        if not isinstance(database_id, str) or not DATABASE_ID_PATTERN.fullmatch(database_id):
            raise ValueError(f"invalid provider database id: {database_id!r}")
        if database_id in seen_ids:
            raise ValueError(f"duplicate provider database id: {database_id}")
        seen_ids.add(database_id)
        repository = provider.get("repository")
        if not isinstance(repository, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
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
        files = provider.get("files")
        if not isinstance(files, dict) or set(files) != {"manifest", "contract", "checksums", "databaseManifest", "artifact", "license"}:
            raise ValueError(f"{database_id} must pin manifest, contract, checksums, databaseManifest, artifact, and license files")
        for file_key, descriptor in files.items():
            if not isinstance(descriptor, dict):
                raise ValueError(f"{database_id} {file_key} pin must be an object")
            safe_relative_path(descriptor.get("path"), f"{database_id} {file_key} path")
            if not isinstance(descriptor.get("sha256"), str) or not SHA256_PATTERN.fullmatch(descriptor["sha256"]):
                raise ValueError(f"{database_id} {file_key} must pin a SHA-256")
        artifact = files["artifact"]
        if not isinstance(artifact.get("bytes"), int) or artifact["bytes"] <= 0 or artifact["bytes"] > MAX_ARTIFACT_BYTES:
            raise ValueError(f"{database_id} artifact must be between 1 byte and {MAX_ARTIFACT_BYTES} bytes")
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


def default_fetch(repository: str, revision: str, relative_path: str) -> bytes:
    url = f"{RAW_GITHUB_ORIGIN}/{repository}/{revision}/{relative_path}"
    request = urllib.request.Request(url, headers={"User-Agent": "DemoDB-Cloud-fixture-verifier/1"})
    with urllib.request.urlopen(request, timeout=30) as response:
        data = response.read(MAX_ARTIFACT_BYTES + 1)
    if len(data) > MAX_ARTIFACT_BYTES:
        raise ValueError(f"provider file exceeds the {MAX_ARTIFACT_BYTES} byte build limit: {relative_path}")
    return data


def validate_provider(provider: dict[str, Any], fetch: Callable[[str, str, str], bytes]) -> dict[str, bytes]:
    database_id = provider["id"]
    repository = provider["repository"]
    revision = provider["revision"]
    fetched: dict[str, bytes] = {}
    for key, descriptor in provider["files"].items():
        data = fetch(repository, revision, descriptor["path"])
        expected_bytes = descriptor.get("bytes")
        verify_blob(data, descriptor["sha256"], expected_bytes, f"{database_id} {key}")
        fetched[key] = data

    manifest = read_json(fetched["manifest"], f"{database_id} provider manifest")
    contract = read_json(fetched["contract"], f"{database_id} provider contract")
    checksums = read_json(fetched["checksums"], f"{database_id} provider checksums")
    database_manifest = read_json(fetched["databaseManifest"], f"{database_id} OVDB database manifest")
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
    if database_manifest.get("deployment", {}).get("engine") != "sqlite" or public_capabilities.get("read") is not True or public_capabilities.get("write") is not False:
        raise ValueError(f"{database_id} OVDB database manifest is not a read-only SQLite provider")
    if provider.get("requirePublishedQuery", True) and public_capabilities.get("query") is not True:
        raise ValueError(f"{database_id} OVDB database manifest does not advertise queries")
    server_id = database_manifest.get("serverId", "")
    expected_api_url = f"{server_id.rstrip('/')}/v1/databases/{database_id}"
    expected_db_base_url = f"{server_id.rstrip('/')}/db/{database_id}/"
    if not server_id.startswith("https://") or database_manifest.get("apiUrl") != expected_api_url or database_manifest.get("serverDbBaseUrl") != expected_db_base_url:
        raise ValueError(f"{database_id} OVDB database manifest routes do not match its provider ID")
    if database_manifest.get("id") != ovdb_capabilities.get("canonicalUrl") or database_manifest.get("homepage") != f"https://{manifest.get('siteHost')}/":
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
    smoke_recordset = next((table["name"] for table in physical_tables if table.get("rowCount", 0) > 0), table_names[0])
    smoke_recordsets = provider.get("smokeRecordsets", [smoke_recordset])
    unknown_smoke_recordsets = set(smoke_recordsets) - set(table_names)
    if unknown_smoke_recordsets:
        raise ValueError(f"{database_id} smoke recordsets are absent from its native table schema: {sorted(unknown_smoke_recordsets)}")

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

    artifact_path = provider["files"]["artifact"]["path"]
    file_checksums = checksums.get("files", {})
    advertised = file_checksums.get(artifact_path)
    if not isinstance(advertised, dict):
        raise ValueError(f"{database_id} checksums omit its declared SQLite artifact")
    if advertised.get("sha256") != provider["files"]["artifact"]["sha256"] or advertised.get("bytes") != provider["files"]["artifact"]["bytes"]:
        raise ValueError(f"{database_id} artifact pin disagrees with provider checksums")
    sqlite_exports = [
        entry
        for entry in contract.get("exports", [])
        if entry.get("path") == artifact_path and entry.get("format", "sqlite" if artifact_path.endswith(".sqlite") else None) == "sqlite"
    ]
    if len(sqlite_exports) != 1:
        raise ValueError(f"{database_id} contract must declare its SQLite artifact exactly once")
    if sqlite_exports[0].get("sha256") not in (None, provider["files"]["artifact"]["sha256"]):
        raise ValueError(f"{database_id} contract SQLite hash disagrees with its immutable provider pin")
    if sqlite_exports[0].get("bytes") != provider["files"]["artifact"]["bytes"]:
        raise ValueError(f"{database_id} contract SQLite size disagrees with its immutable provider pin")
    source_hash = source.get("sha256") or source.get("databaseSha256")
    if source_hash and source_hash != provider["files"]["artifact"]["sha256"]:
        raise ValueError(f"{database_id} source fixture hash differs from the published SQLite artifact")
    if provenance.get("sha256") != provider["files"]["artifact"]["sha256"]:
        raise ValueError(f"{database_id} public provenance digest differs from the published SQLite artifact")

    hosts = {manifest.get("siteHost"), *manifest.get("aliases", [])}
    declared_origins = {origin.removeprefix("https://") for origin in provider["corsOrigins"]}
    if not hosts.issubset(declared_origins):
        raise ValueError(f"{database_id} CORS origins omit a provider host or alias: {sorted(hosts - declared_origins)}")
    return fetched


def prepare_inventory(inventory: Path, output: Path, local_root: Path | None = None) -> Path:
    providers = load_inventory(inventory)
    output.mkdir(parents=True, exist_ok=True)
    runtime_databases: list[dict[str, Any]] = []
    for provider in providers:
        def fetch(repository: str, revision: str, relative_path: str) -> bytes:
            if local_root is None:
                return default_fetch(repository, revision, relative_path)
            return (local_root / repository / relative_path).read_bytes()

        fetched = validate_provider(provider, fetch)
        database_id = provider["id"]
        artifact = provider["files"]["artifact"]
        tables = read_json(fetched["contract"], f"{database_id} provider contract")["schema"]["tables"]
        physical_tables = [table for table in tables if table.get("kind", "table") == "table" and isinstance(table.get("name"), str)]
        smoke_recordset = next((table["name"] for table in physical_tables if table.get("rowCount", 0) > 0), physical_tables[0]["name"])
        smoke_recordsets = provider.get("smokeRecordsets", [smoke_recordset])
        with tempfile.NamedTemporaryFile(prefix=f"{database_id}-", suffix=".sqlite", dir=output, delete=False) as temporary:
            source_path = Path(temporary.name)
            temporary.write(fetched["artifact"])
        try:
            manifest_path = prepare_fixture.prepare(
                source_path,
                output,
                database_id,
                artifact["sha256"],
                provider["recordKeyFormat"],
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
                "sourceSha256": artifact["sha256"],
                "servingSha256": sha256((output / f"{database_id}.sqlite").read_bytes()),
                "license": license_name,
                "licenseSha256": provider["files"]["license"]["sha256"],
                "smokeRecordset": smoke_recordset,
                "smokeRecordsets": smoke_recordsets,
            }
        )
    runtime_path = output / "inventory.json"
    runtime_path.write_text(json.dumps({"version": 1, "databases": runtime_databases}, indent=2) + "\n", encoding="utf-8")
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
