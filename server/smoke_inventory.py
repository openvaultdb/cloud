#!/usr/bin/env python3
"""Smoke every pinned database after Cloud Run deployment."""

import argparse
import base64
import json
import hashlib
import time
from pathlib import Path
import sqlite3
from urllib.error import HTTPError, URLError
from urllib.parse import quote, unquote, urlencode, parse_qs, urlsplit
from urllib.request import Request, urlopen


OBSERVATIONS = []

def observe(origin, path, method, status, headers, body):
    OBSERVATIONS.append({"origin": origin, "path": path, "method": method, "status": status, "bodyBytes": len(body), "bodySha256": hashlib.sha256(body).hexdigest(), "bodyBase64": base64.b64encode(body).decode(), "headers": {name: headers.get(name) for name in ("Access-Control-Allow-Origin", "Cache-Control", "OVDB-Provider-Revision", "OVDB-Source-SHA256", "OVDB-Serving-SHA256", "OVDB-Manifest-SHA256")}})
    return status, headers, body

def request(origin: str, path: str, method: str = "GET", body: bytes | None = None, headers: dict[str, str] | None = None):
    request_headers = {"User-Agent": "DemoDB-cloud-smoke/1"}
    request_headers.update(headers or {})
    req = Request(origin.rstrip("/") + path, data=body, headers=request_headers, method=method)
    try:
        with urlopen(req, timeout=20) as response:
            return observe(origin, path, method, response.status, response.headers, response.read())
    except HTTPError as error:
        return observe(origin, path, method, error.code, error.headers, error.read())
    except URLError as error:
        raise RuntimeError(f"request {method} {path} failed: {error}") from error


def require_status(origin: str, path: str, expected: int = 200, **kwargs):
    status, headers, body = request(origin, path, **kwargs)
    if status != expected:
        raise RuntimeError(f"{kwargs.get('method', 'GET')} {path} returned {status}, expected {expected}: {body[:400]!r}")
    return headers, body


def require_query_cache_control(database, cache_control):
    if database.get("storageEngine") == "ingitdb":
        if cache_control.strip().lower() != "no-store":
            raise RuntimeError(f"{database['id']} native GET query is not no-store")
    elif database.get("readProfile") == "bounded-immutable/1":
        if "no-store" not in cache_control:
            raise RuntimeError(f"{database['id']} bounded GET query is not no-store")
    elif "max-age=86400" not in cache_control:
        raise RuntimeError(f"{database['id']} GET query is not cacheable")


def require_cors(origin: str, database_id: str, cors_origin: str, query_path: str) -> None:
    for path in (f"/ovdb/dbs/{database_id}", query_path):
        headers, _ = require_status(origin, path, headers={"Origin": cors_origin})
        if headers.get("Access-Control-Allow-Origin") != cors_origin:
            raise RuntimeError(f"{database_id} {path} omitted allowed CORS origin {cors_origin}")
        if "origin" not in {value.strip().lower() for value in headers.get("Vary", "").split(",")}:
            raise RuntimeError(f"{database_id} {path} omitted Vary: Origin")
    query = parse_qs(urlsplit(query_path).query)["q"][0]
    headers, _ = require_status(origin, f"/v1/databases/{database_id}/dtql", method="POST", body=json.dumps({"query": query}).encode(), headers={"Origin": cors_origin, "Content-Type": "application/json"})
    if headers.get("Access-Control-Allow-Origin") != cors_origin:
        raise RuntimeError(f"{database_id} POST query omitted allowed CORS origin {cors_origin}")
    headers, _ = require_status(
        origin, f"/v1/databases/{database_id}/dtql", expected=204, method="OPTIONS",
        headers={"Origin": cors_origin, "Access-Control-Request-Method": "POST",
                 "Access-Control-Request-Headers": "Content-Type,OVDB-Page-Size,OVDB-Page-Token,OVDB-Page-Close"},
    )
    if headers.get("Access-Control-Allow-Origin") != cors_origin:
        raise RuntimeError(f"{database_id} preflight omitted allowed CORS origin {cors_origin}")
    methods = {value.strip().upper() for value in headers.get("Access-Control-Allow-Methods", "").split(",")}
    allowed_headers = {value.strip().lower() for value in headers.get("Access-Control-Allow-Headers", "").split(",")}
    if "POST" not in methods or not {"content-type", "ovdb-page-size", "ovdb-page-token", "ovdb-page-close"} <= allowed_headers:
        raise RuntimeError(f"{database_id} preflight omitted POST or paging request headers")
    if "origin" not in {value.strip().lower() for value in headers.get("Vary", "").split(",")}:
        raise RuntimeError(f"{database_id} preflight omitted Vary: Origin")


def quote_identifier(identifier: str) -> str:
    return '"' + identifier.replace('"', '""') + '"'


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", required=True, help="deployed Cloud Run HTTPS origin")
    parser.add_argument("--inventory", type=Path, required=True, help="verified generated runtime inventory")
    parser.add_argument("--receipt", type=Path, help="write actual public HTTP status/body/hash evidence")
    parser.add_argument("--publication-receipt", type=Path, help="require full Shipping publication checks bound to serving identity")
    args = parser.parse_args()
    OBSERVATIONS.clear()
    inventory = json.loads(args.inventory.read_text(encoding="utf-8"))
    databases = inventory.get("databases")
    if inventory.get("version") not in (1, 2) or not isinstance(databases, list) or not databases:
        raise RuntimeError("runtime inventory must contain at least one database")

    for database in databases:
        database_id = quote(database["id"], safe="")
        cors_origin = database["corsOrigins"][0]
        _, profile = require_status(args.origin, f"/ovdb/dbs/{database_id}")
        if f"https://cloud.openvaultdb.com/ovdb/dbs/{database_id}".encode() not in profile:
            raise RuntimeError(f"{database['id']} human profile omitted its public cloud URL")
        profile_headers, api_profile = require_status(args.origin, f"/v1/databases/{database_id}")
        if args.publication_receipt or all(field in database for field in ("providerRevision", "sourceSha256", "servingSha256", "manifestSha256")):
            for header, field in (("OVDB-Provider-Revision", "providerRevision"), ("OVDB-Source-SHA256", "sourceSha256"), ("OVDB-Serving-SHA256", "servingSha256"), ("OVDB-Manifest-SHA256", "manifestSha256")):
                if profile_headers.get(header) != database[field]:
                    raise RuntimeError(f"{database['id']} public proxy pin mismatch: {header}")
        # Every provider must refuse undeclared CORS origins on reads and preflight.
        for method, denied_path in (("GET", f"/v1/databases/{database_id}"), ("OPTIONS", f"/v1/databases/{database_id}/dtql")):
            status, denied_headers, _ = request(args.origin, denied_path, method=method, headers={"Origin": "https://undeclared.invalid", "Access-Control-Request-Method": "POST"})
            if status not in (200, 204, 403) or denied_headers.get("Access-Control-Allow-Origin"):
                raise RuntimeError(f"{database['id']} granted undeclared origin")
        recordsets = database.get("smokeRecordsets") or [database["smokeRecordset"]]
        first_name = recordsets[0].replace("'", "''")
        cors_query = f"from: {{name: '{first_name}'}}\nlimit: 1\n"
        cors_query_path = f"/v1/databases/{database_id}/dtql?{urlencode({'q': cors_query})}"
        for declared_origin in database["corsOrigins"]:
            require_cors(args.origin, database_id, declared_origin, cors_query_path)
        queried_rows = {}
        for collection in recordsets:
            collection_path = quote(collection, safe="")
            if database.get("storageEngine", "sqlite") == "sqlite":
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
            require_query_cache_control(database, headers.get("Cache-Control", ""))
            result = json.loads(result_bytes)
            records = result.get("records")
            if not isinstance(records, list) or not records:
                raise RuntimeError(f"{database['id']} query returned no records from {collection!r}")
            first_record = records[0]
            if not isinstance(first_record, dict) or not isinstance(first_record.get("data"), dict):
                raise RuntimeError(f"{database['id']} query returned malformed record data for {collection!r}")
            record_key = first_record.get("key") if isinstance(first_record, dict) else None
            if not isinstance(record_key, str) or not record_key.strip() or "<nil>" in record_key:
                raise RuntimeError(f"{database['id']} query returned an invalid record key for {collection!r}: {record_key!r}")
            identity_field = "event_id" if database.get("storageEngine") == "ingitdb" else "id"
            record_id = first_record["data"].get(identity_field)
            if database.get("servingAdapter") == "separate-id/1":
                # Native data can retain its own integer `id`; the envelope
                # carries the separately declared serving record key.
                prefix = collection + "/"
                if not record_key.startswith(prefix) or "/" in record_key[len(prefix):]:
                    raise RuntimeError(f"{database['id']} malformed serving key for {collection!r}")
                record_id = unquote(record_key[len(prefix):])
            if not isinstance(record_id, str) or not record_id:
                raise RuntimeError(f"{database['id']} query returned no serving identity for {collection!r}")
            queried_rows[collection] = first_record["data"]
            record_status, _, record_bytes = request(
                args.origin,
                f"/v1/databases/{database_id}/records/{collection_path}/{quote(record_id, safe='')}",
            )
            if record_status != 200:
                raise RuntimeError(f"{database['id']} record lookup for {collection!r}/{record_id!r} returned {record_status}: {record_bytes[:400]!r}")
            fetched_record = json.loads(record_bytes)
            if fetched_record.get("key") != record_key or fetched_record.get("data") != first_record["data"]:
                raise RuntimeError(f"{database['id']} record lookup differs from DTQL result for {collection!r}/{record_id!r}")
            for field in database.get("blobSmokeFields", {}).get(collection, []):
                blob_value = first_record["data"].get(field)
                if not isinstance(blob_value, str) or not blob_value:
                    raise RuntimeError(f"{database['id']} query returned no encoded binary value for {collection}.{field}")
                try:
                    decoded_blob = base64.b64decode(blob_value, validate=True)
                except (ValueError, base64.binascii.Error) as error:
                    raise RuntimeError(f"{database['id']} query returned invalid base64 for {collection}.{field}") from error
                fixture_path = args.inventory.parent / f"{database['id']}.sqlite"
                with sqlite3.connect(f"file:{fixture_path}?mode=ro", uri=True) as fixture:
                    expected_blob = fixture.execute(
                        f"SELECT {quote_identifier(field)} FROM {quote_identifier(collection)} WHERE {quote_identifier('id')} = ?",
                        (record_id,),
                    ).fetchone()
                if expected_blob is None or expected_blob[0] is None or decoded_blob != expected_blob[0]:
                    raise RuntimeError(f"{database['id']} BLOB payload differs from the pinned serving fixture for {collection}.{field}")

        if args.publication_receipt and database["id"] == "shipping":
            schema = json.loads(api_profile).get("schemas", {}).get("collections", {})
            if "requests" not in schema:
                raise RuntimeError("Shipping public schema omits requests")
            query = "from: {name: requests}\nwhere: {op: ==, left: {field: order_id}, right: {value: 11062}}\nlimit: 1"
            result = json.loads(require_status(args.origin, f"/v1/databases/{database_id}/dtql?" + urlencode({"q": query}))[1])
            rows = result.get("records", [])
            if len(rows) != 1 or rows[0].get("data", {}).get("order_id") != 11062:
                raise RuntimeError("Shipping order 11062 query failed")
            row = rows[0]
            keyed = json.loads(require_status(args.origin, f"/v1/databases/{database_id}/records/requests/" + quote(row["data"]["id"], safe=""))[1])
            if keyed.get("data") != row["data"]:
                raise RuntimeError("Shipping order 11062 keyed read differs")
        if args.publication_receipt and database["id"] == "shipping-logs":
            expected = {"baseline": "attempt-11062-1-e1", "recovery11045example": "attempt-11045-2-e1", "recovery11062example": "attempt-11062-2-e1"}
            if set(recordsets) != set(expected) or set(json.loads(api_profile).get("collections", [])) != set(expected):
                raise RuntimeError("Shipping Logs collection IDs differ")
            for collection, key in expected.items():
                query = f"from: {{name: {collection}}}\nlimit: 100"
                rows = json.loads(require_status(args.origin, f"/v1/databases/{database_id}/dtql?" + urlencode({"q": query}))[1]).get("records", [])
                if not rows or not all(row["key"].startswith(collection + "/") for row in rows) or not any(row["data"].get("event_id") == key for row in rows):
                    raise RuntimeError(f"Shipping Logs separate collection query failed: {collection}")
                keyed = json.loads(require_status(args.origin, f"/v1/databases/{database_id}/records/{collection}/{key}")[1])
                if keyed.get("data", {}).get("event_id") != key:
                    raise RuntimeError(f"Shipping Logs representative keyed read failed: {collection}")

        for relationship in database.get("foreignKeySmoke", []):
            source = queried_rows.get(relationship["sourceRecordset"])
            if not isinstance(source, dict) or relationship["sourceField"] not in source or source[relationship["sourceField"]] is None:
                raise RuntimeError(f"{database['id']} foreign-key source row omits {relationship['sourceRecordset']}.{relationship['sourceField']}")
            target = relationship["targetRecordset"]
            target_field = relationship["targetField"]
            query = (
                f"from: {{name: '{target.replace(chr(39), chr(39) * 2)}'}}\n"
                f"where: {{op: '==', left: {{field: '{target_field.replace(chr(39), chr(39) * 2)}'}}, right: {{param: 'foreignKey'}}}}\n"
                "limit: 1\n"
            )
            query_path = f"/v1/databases/{database_id}/dtql?{urlencode({'q': query, 'parameters': json.dumps({'foreignKey': source[relationship['sourceField']]}, separators=(',', ':'))})}"
            _, target_bytes = require_status(args.origin, query_path)
            target_result = json.loads(target_bytes)
            target_records = target_result.get("records")
            if not isinstance(target_records, list) or not target_records:
                raise RuntimeError(f"{database['id']} foreign-key target query returned no {target!r} row")
            target_data = target_records[0].get("data")
            if not isinstance(target_data, dict) or target_data.get(target_field) != source[relationship["sourceField"]]:
                raise RuntimeError(f"{database['id']} foreign-key target {target}.{target_field} does not match {relationship['sourceRecordset']}.{relationship['sourceField']}")

        empty_recordsets = database.get("emptyRecordsets", [])
        for collection in empty_recordsets:
            collection_path = quote(collection, safe="")
            if database.get("storageEngine", "sqlite") == "sqlite":
                require_status(args.origin, f"/ovdb/dbs/{database_id}/collections/{collection_path}")
            query_name = collection.replace("'", "''")
            query = f"from: {{name: '{query_name}'}}\nlimit: 1\n"
            result = json.loads(require_status(
                args.origin,
                f"/v1/databases/{database_id}/dtql?{urlencode({'q': query})}",
                headers={"Origin": cors_origin},
            )[1])
            if result.get("records") != []:
                raise RuntimeError(f"{database['id']} expected empty recordset {collection!r} returned rows")

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
        print(f"verified {database['id']}: {len(recordsets)} populated queries, {len(empty_recordsets)} empty tables, record lookup, CORS, and read-only")

    identity = None
    if args.publication_receipt:
        identity = json.loads(args.publication_receipt.read_text())
        if identity.get("mergeSha") != identity.get("deployedSourceSha") or identity.get("candidateImage") != identity.get("deployedImage") or not identity.get("deployedRevision"):
            raise RuntimeError("publication receipt lacks verified serving identity")
        if {database["id"] for database in databases} != {"adventureworks", "employees", "chinook", "northwind", "pubs", "sakila", "shipping", "shipping-logs"}:
            raise RuntimeError("publication acceptance requires all eight providers")
        require_status(identity["serviceOrigin"], "/ovdb/dbs/chinook", expected=403)
        _, discovery = require_status("https://demodb.dev", "/.well-known/openvaultdb")
        if "cloud.openvaultdb.com" not in json.dumps(json.loads(discovery)):
            raise RuntimeError("central discovery omits the Cloud provider")
    if args.receipt:
        args.receipt.write_text(json.dumps({"verifiedAt": int(time.time()), "servingIdentity": identity, "providers": [database["id"] for database in databases], "requests": OBSERVATIONS}, sort_keys=True, indent=2) + "\n")


if __name__ == "__main__":
    main()
