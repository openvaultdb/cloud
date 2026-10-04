#!/usr/bin/env python3
"""Smoke every pinned database after Cloud Run deployment."""

import argparse
import json
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode
from urllib.request import Request, urlopen


def request(origin: str, path: str, method: str = "GET", body: bytes | None = None, headers: dict[str, str] | None = None):
    request_headers = {"User-Agent": "DemoDB-cloud-smoke/1"}
    request_headers.update(headers or {})
    req = Request(origin.rstrip("/") + path, data=body, headers=request_headers, method=method)
    try:
        with urlopen(req, timeout=20) as response:
            return response.status, response.headers, response.read()
    except HTTPError as error:
        return error.code, error.headers, error.read()
    except URLError as error:
        raise RuntimeError(f"request {method} {path} failed: {error}") from error


def require_status(origin: str, path: str, expected: int = 200, **kwargs):
    status, headers, body = request(origin, path, **kwargs)
    if status != expected:
        raise RuntimeError(f"{kwargs.get('method', 'GET')} {path} returned {status}, expected {expected}: {body[:400]!r}")
    return headers, body


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", required=True, help="deployed Cloud Run HTTPS origin")
    parser.add_argument("--inventory", type=Path, required=True, help="verified generated runtime inventory")
    args = parser.parse_args()
    inventory = json.loads(args.inventory.read_text(encoding="utf-8"))
    databases = inventory.get("databases")
    if inventory.get("version") != 1 or not isinstance(databases, list) or not databases:
        raise RuntimeError("runtime inventory must contain at least one database")

    for database in databases:
        database_id = quote(database["id"], safe="")
        cors_origin = database["corsOrigins"][0]
        _, profile = require_status(args.origin, f"/ovdb/dbs/{database_id}")
        if f"https://cloud.openvaultdb.com/ovdb/dbs/{database_id}".encode() not in profile:
            raise RuntimeError(f"{database['id']} human profile omitted its public cloud URL")
        require_status(args.origin, f"/v1/databases/{database_id}")
        recordsets = database.get("smokeRecordsets") or [database["smokeRecordset"]]
        for collection in recordsets:
            collection_path = quote(collection, safe="")
            require_status(args.origin, f"/ovdb/dbs/{database_id}/collections/{collection_path}")
            query_name = collection.replace("'", "''")
            query = f"from: {{name: '{query_name}'}}\nlimit: 1\n"
            query_path = f"/v1/databases/{database_id}/dtql?{urlencode({'q': query})}"
            headers, result_bytes = require_status(
                args.origin,
                query_path,
                headers={"Origin": cors_origin},
            )
            if headers.get("Access-Control-Allow-Origin") != cors_origin:
                raise RuntimeError(f"{database['id']} response omitted allowed CORS origin {cors_origin}")
            if "max-age=86400" not in headers.get("Cache-Control", ""):
                raise RuntimeError(f"{database['id']} GET query is not cacheable")
            result = json.loads(result_bytes)
            records = result.get("records")
            if not isinstance(records, list) or not records:
                raise RuntimeError(f"{database['id']} query returned no records from {collection!r}")
            first_record = records[0]
            record_key = first_record.get("key") if isinstance(first_record, dict) else None
            if not isinstance(record_key, str) or not record_key.strip() or "<nil>" in record_key:
                raise RuntimeError(f"{database['id']} query returned an invalid record key for {collection!r}: {record_key!r}")

        write_collection_path = quote(recordsets[0], safe="")
        status, _, body = request(
            args.origin,
            f"/v1/databases/{database_id}/records/{write_collection_path}/no-such-record",
            method="PUT",
            body=b'{"data":{}}',
            headers={"Content-Type": "application/json"},
        )
        if status != 403:
            raise RuntimeError(f"{database['id']} write was not rejected as read-only: {status} {body[:400]!r}")
        print(f"verified {database['id']}: profile, {len(recordsets)} collections and queries, CORS, and read-only")


if __name__ == "__main__":
    main()
