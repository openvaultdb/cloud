#!/usr/bin/env python3
"""Smoke every pinned database after Cloud Run deployment."""

import argparse
import base64
import json
import hashlib
import math
import re
import time
from pathlib import Path
import sqlite3
from urllib.error import HTTPError, URLError
from urllib.parse import quote, unquote, urlencode, parse_qs, urlsplit
from urllib.request import Request, urlopen


OBSERVATIONS = []

# The published DemoDB catalogue: central discovery and publication acceptance require exactly these providers.
PUBLISHED_PROVIDERS = frozenset({"adventureworks", "employees", "chinook", "northwind", "pubs", "sakila", "shipping", "shipping-logs", "countries"})
# Providers the central demodb.dev catalogue (demo-db/websites) must list. It lists only datasets that
# have a demodb.dev host; Countries is served here but has no demodb.dev host, so the catalogue may omit it.
CATALOGUE_PROVIDERS = PUBLISHED_PROVIDERS - {"countries"}
PIN_FIELDS = (("OVDB-Provider-Revision", "providerRevision"), ("OVDB-Source-SHA256", "sourceSha256"), ("OVDB-Serving-SHA256", "servingSha256"), ("OVDB-Manifest-SHA256", "manifestSha256"))

def observe(origin, path, method, status, headers, body):
    OBSERVATIONS.append({"origin": origin, "path": path, "method": method, "status": status, "bodyBytes": len(body), "bodySha256": hashlib.sha256(body).hexdigest(), "bodyBase64": base64.b64encode(body).decode(), "headers": {name: headers.get(name) for name in ("Access-Control-Allow-Origin", "Cache-Control", "OVDB-Provider-Revision", "OVDB-Source-SHA256", "OVDB-Serving-SHA256", "OVDB-Manifest-SHA256", "Retry-After")}})
    return status, headers, body

def query_request(path, method):
    """Match the gateway query class after canonical path decoding; OPTIONS is separate."""
    canonical = unquote(urlsplit(path).path)
    return method != "OPTIONS" and (canonical in ("/v1/tugql", "/v1/tugql/")
        or re.fullmatch(r"/v1/databases/[^/]+/(tugql|query|batch|read)/?", canonical) is not None)


class SmokeRequestPolicy:
    def __init__(self, query_interval=1.1, retries=2, max_retry_after=120,
                 clock=None, sleep=None, transport=None):
        if not math.isfinite(query_interval) or not 0 <= query_interval <= 60:
            raise ValueError("query interval must be finite and between 0 and 60 seconds")
        if type(retries) is not int or not 0 <= retries <= 5:
            raise ValueError("rate limit retries must be an integer between 0 and 5")
        if not math.isfinite(max_retry_after) or not 0 <= max_retry_after <= 300:
            raise ValueError("maximum Retry-After must be finite and between 0 and 300 seconds")
        self.query_interval, self.retries, self.max_retry_after = query_interval, retries, max_retry_after
        self.clock, self.sleep, self.transport = clock or time.monotonic, sleep or time.sleep, transport or urlopen
        self.last_query = None

    def settings(self):
        return {"queryIntervalSeconds": self.query_interval, "rateLimitRetries": self.retries,
                "maxRetryAfterSeconds": self.max_retry_after}

    def request(self, origin, path, method="GET", body=None, headers=None):
        request_headers = {"User-Agent": "DemoDB-cloud-smoke/1"}
        request_headers.update(headers or {})
        for attempt in range(self.retries + 1):
            if query_request(path, method):
                if self.last_query is not None:
                    delay = self.query_interval - (self.clock() - self.last_query)
                    if delay > 0:
                        self.sleep(delay)
                self.last_query = self.clock()
            req = Request(origin.rstrip("/") + path, data=body, headers=request_headers, method=method)
            try:
                with self.transport(req, timeout=20) as response:
                    result = observe(origin, path, method, response.status, response.headers, response.read())
            except HTTPError as error:
                with error:
                    result = observe(origin, path, method, error.code, error.headers, error.read())
            except URLError as error:
                raise RuntimeError(f"request {method} {path} failed: {error}") from error
            if result[0] != 429 or attempt == self.retries:
                return result
            retry_after = result[1].get("Retry-After", "")
            # The owning gateway emits delta-seconds. Reject other syntax and excessive waits.
            if not re.fullmatch(r"[0-9]+", retry_after) or len(retry_after) > 6:
                raise RuntimeError("HTTP 429 has no valid bounded delta-seconds Retry-After")
            delay = int(retry_after)
            if delay > self.max_retry_after:
                raise RuntimeError("HTTP 429 Retry-After exceeds the configured bound")
            self.sleep(delay)


REQUEST_POLICY = SmokeRequestPolicy()


def request(origin: str, path: str, method: str = "GET", body: bytes | None = None, headers: dict[str, str] | None = None):
    return REQUEST_POLICY.request(origin, path, method, body, headers)


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
    headers, _ = require_status(origin, f"/v1/databases/{database_id}/tugql", method="POST", body=json.dumps({"query": query}).encode(), headers={"Origin": cors_origin, "Content-Type": "application/json"})
    if headers.get("Access-Control-Allow-Origin") != cors_origin:
        raise RuntimeError(f"{database_id} POST query omitted allowed CORS origin {cors_origin}")
    headers, _ = require_status(
        origin, f"/v1/databases/{database_id}/tugql", expected=204, method="OPTIONS",
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


# One original event per public Shipping Logs collection; baseline streams and
# their future recovery examples are separate collections.
SHIPPING_LOGS_REPRESENTATIVE_EVENTS = {
    "baseline": "attempt-11062-1-e1", "recovery11045example": "attempt-11045-2-e1", "recovery11062example": "attempt-11062-2-e1",
    "carrierGateway": "attempt-11038-1-c1", "carrierGatewayRecovery11045example": "attempt-11045-2-c1", "carrierGatewayRecovery11062example": "attempt-11062-2-c1",
}
SHIPPING_LOGS_BASELINES = ("baseline", "carrierGateway")


def require_recovery_separation(event_ids):
    """No recovery-example event may appear in a baseline view."""
    recovery = set().union(*(ids for name, ids in event_ids.items() if name not in SHIPPING_LOGS_BASELINES))
    for name in SHIPPING_LOGS_BASELINES:
        if event_ids.get(name, set()) & recovery:
            raise RuntimeError(f"Shipping Logs baseline {name} contains recovery-example events")

# The public-demo rows the Countries reference provider must resolve, by ISO alpha-2 key:
# alpha-3 code and currency, and a World Bank population with its year and source.
COUNTRIES_EXPECTED = {"CA": ("CAN", "CAD"), "IT": ("ITA", "EUR"), "BE": ("BEL", "EUR"), "US": ("USA", "USD"), "VE": ("VEN", "VES")}
COUNTRIES_ALIASES = {"USA": "US", "UK": "GB"}


def require_countries_reference(origin, database_id, recordsets, api_profile):
    """Countries resolves the demo's ship countries by code and alias, with attributed populations."""
    if set(recordsets) != {"countries", "country_aliases", "sources"}:
        raise RuntimeError("Countries smoke recordsets differ")
    if not {"countries", "country_aliases", "sources"} <= set(json.loads(api_profile).get("collections", [])):
        raise RuntimeError("Countries public profile omits a recordset")
    for iso2, (iso3, currency) in COUNTRIES_EXPECTED.items():
        row = json.loads(require_status(origin, f"/v1/databases/{database_id}/records/countries/{iso2}")[1]).get("data", {})
        population, year = row.get("population"), row.get("population_year")
        if (row.get("iso2"), row.get("iso3"), row.get("currency_code")) != (iso2, iso3, currency) or type(population) is not int or population <= 0 or type(year) is not int or year < 1960 or row.get("population_source_id") != "worldbank-sp-pop-totl":
            raise RuntimeError(f"Countries {iso2} lacks its codes, currency or attributed population")
    for alias, iso2 in COUNTRIES_ALIASES.items():
        query = f"from: {{name: country_aliases}}\nwhere: {{op: ==, left: {{field: alias}}, right: {{value: {alias}}}}}\nlimit: 1"
        rows = json.loads(require_status(origin, f"/v1/databases/{database_id}/tugql?" + urlencode({"q": query}))[1]).get("records", [])
        if len(rows) != 1 or rows[0].get("data", {}).get("country_iso2") != iso2:
            raise RuntimeError(f"Countries alias {alias} does not resolve to {iso2}")
    source = json.loads(require_status(origin, f"/v1/databases/{database_id}/records/sources/worldbank-sp-pop-totl")[1]).get("data", {})
    if source.get("publisher") != "The World Bank" or source.get("licence") != "CC-BY-4.0" or "SP.POP.TOTL" not in source.get("attribution", ""):
        raise RuntimeError("Countries World Bank attribution differs")


def require_publication_discovery(raw, expected_ids):
    """Validate the central catalogue's published DemoDB alias contract, not its host implementation."""
    discovery = json.loads(raw)
    if not isinstance(discovery, dict) or discovery.get("protocol") != "openvaultdb/0.1" or discovery.get("authEnabled") is not False:
        raise RuntimeError("central discovery protocol/auth contract mismatch")
    rows = discovery.get("databases")
    expected = set(expected_ids)
    if len(expected_ids) != len(PUBLISHED_PROVIDERS) or expected != PUBLISHED_PROVIDERS:
        raise RuntimeError("central discovery requires the exact nine-provider inventory")
    if not isinstance(rows, list) or not len(CATALOGUE_PROVIDERS) <= len(rows) <= len(PUBLISHED_PROVIDERS):
        raise RuntimeError("central discovery must list the catalogue providers")
    found = set()
    for row in rows:
        if not isinstance(row, dict) or set(row) != {"id", "url", "apiUrl", "manifestUrl", "capabilities"}:
            raise RuntimeError("central discovery database shape mismatch")
        identifier = row["id"]
        if not isinstance(identifier, str) or identifier not in expected or identifier in found:
            raise RuntimeError("central discovery has missing, duplicate or unexpected provider IDs")
        found.add(identifier)
        if row["url"] != f"https://demodb.dev/{identifier}/" or row["apiUrl"] != f"https://demodb.dev/ovdb/v1/databases/{identifier}" or row["manifestUrl"] != f"https://demodb.dev/ovdb/db/{identifier}/ovdb-database.json":
            raise RuntimeError("central discovery canonical branded URL mismatch")
        capabilities = row["capabilities"]
        if not isinstance(capabilities, dict) or set(capabilities) != {"read", "query", "write"} or capabilities["read"] is not True or capabilities["query"] is not True or capabilities["write"] is not False:
            raise RuntimeError("central discovery must declare actual read-only query capabilities")
    if not CATALOGUE_PROVIDERS <= found:
        raise RuntimeError("central discovery provider inventory mismatch")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", required=True, help="branded public HTTPS origin")
    parser.add_argument("--inventory", type=Path, required=True, help="verified generated runtime inventory")
    parser.add_argument("--receipt", type=Path, help="write actual public HTTP status/body/hash evidence")
    parser.add_argument("--publication-receipt", type=Path, help="require full Shipping publication checks bound to serving identity")
    parser.add_argument("--rollback-acceptance", action="store_true", help="accept an older immutable rollback revision: fully smoke providers whose four pins match, record absent or differently pinned ones")
    parser.add_argument("--query-interval-seconds", type=float, default=1.1, help="minimum query-class spacing (0..60 seconds; default 1.1)")
    parser.add_argument("--rate-limit-retries", type=int, default=2, help="additional attempts on HTTP429 only (0..5)")
    parser.add_argument("--max-retry-after-seconds", type=float, default=120, help="maximum accepted gateway delta-seconds Retry-After (0..300)")
    args = parser.parse_args()
    if args.rollback_acceptance and args.publication_receipt:
        parser.error("--rollback-acceptance cannot accept a publication")
    global REQUEST_POLICY
    try:
        REQUEST_POLICY = SmokeRequestPolicy(args.query_interval_seconds, args.rate_limit_retries, args.max_retry_after_seconds)
    except ValueError as error:
        parser.error(str(error))
    OBSERVATIONS.clear()
    inventory = json.loads(args.inventory.read_text(encoding="utf-8"))
    databases = inventory.get("databases")
    if inventory.get("version") not in (1, 2) or not isinstance(databases, list) or not databases:
        raise RuntimeError("runtime inventory must contain at least one database")

    unverified = []
    for database in databases:
        database_id = quote(database["id"], safe="")
        cors_origin = database["corsOrigins"][0]
        if args.rollback_acceptance:
            # The rollback revision is bound by image digest and predates this inventory; this
            # inventory's content checks apply only to providers it serves at identical pins.
            status, headers, _ = request(args.origin, f"/v1/databases/{database_id}")
            if status == 404:
                unverified.append({"id": database["id"], "reason": "absent"})
                continue
            if status != 200:
                raise RuntimeError(f"{database['id']} rollback profile returned {status}")
            if any(headers.get(header) != database.get(field) for header, field in PIN_FIELDS):
                unverified.append({"id": database["id"], "reason": "different-pins", "providerRevision": headers.get("OVDB-Provider-Revision")})
                continue
        _, profile = require_status(args.origin, f"/ovdb/dbs/{database_id}")
        if f"https://cloud.openvaultdb.com/ovdb/dbs/{database_id}".encode() not in profile:
            raise RuntimeError(f"{database['id']} human profile omitted its public cloud URL")
        profile_headers, api_profile = require_status(args.origin, f"/v1/databases/{database_id}")
        if args.publication_receipt or all(field in database for field in ("providerRevision", "sourceSha256", "servingSha256", "manifestSha256")):
            for header, field in PIN_FIELDS:
                if profile_headers.get(header) != database[field]:
                    raise RuntimeError(f"{database['id']} public proxy pin mismatch: {header}")
        # Every provider must refuse undeclared CORS origins on reads and preflight.
        for method, denied_path in (("GET", f"/v1/databases/{database_id}"), ("OPTIONS", f"/v1/databases/{database_id}/tugql")):
            status, denied_headers, _ = request(args.origin, denied_path, method=method, headers={"Origin": "https://undeclared.invalid", "Access-Control-Request-Method": "POST"})
            if status not in (200, 204, 403) or denied_headers.get("Access-Control-Allow-Origin"):
                raise RuntimeError(f"{database['id']} granted undeclared origin")
        recordsets = database.get("smokeRecordsets") or [database["smokeRecordset"]]
        first_name = recordsets[0].replace("'", "''")
        cors_query = f"from: {{name: '{first_name}'}}\nlimit: 1\n"
        cors_query_path = f"/v1/databases/{database_id}/tugql?{urlencode({'q': cors_query})}"
        for declared_origin in database["corsOrigins"]:
            require_cors(args.origin, database_id, declared_origin, cors_query_path)
        queried_rows = {}
        for collection in recordsets:
            collection_path = quote(collection, safe="")
            if database.get("storageEngine", "sqlite") == "sqlite":
                require_status(args.origin, f"/ovdb/dbs/{database_id}/collections/{collection_path}")
            query_name = collection.replace("'", "''")
            query = f"from: {{name: '{query_name}'}}\nlimit: 1\n"
            query_path = f"/v1/databases/{database_id}/tugql?{urlencode({'q': query})}"
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
                raise RuntimeError(f"{database['id']} record lookup differs from TugQL result for {collection!r}/{record_id!r}")
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
            result = json.loads(require_status(args.origin, f"/v1/databases/{database_id}/tugql?" + urlencode({"q": query}))[1])
            rows = result.get("records", [])
            if len(rows) != 1 or rows[0].get("data", {}).get("order_id") != 11062:
                raise RuntimeError("Shipping order 11062 query failed")
            row = rows[0]
            keyed = json.loads(require_status(args.origin, f"/v1/databases/{database_id}/records/requests/" + quote(row["data"]["id"], safe=""))[1])
            if keyed.get("data") != row["data"]:
                raise RuntimeError("Shipping order 11062 keyed read differs")
        if args.publication_receipt and database["id"] == "shipping-logs":
            expected = SHIPPING_LOGS_REPRESENTATIVE_EVENTS
            if set(recordsets) != set(expected) or set(json.loads(api_profile).get("collections", [])) != set(expected):
                raise RuntimeError("Shipping Logs collection IDs differ")
            event_ids = {}
            for collection, key in expected.items():
                query = f"from: {{name: {collection}}}\nlimit: 100"
                rows = json.loads(require_status(args.origin, f"/v1/databases/{database_id}/tugql?" + urlencode({"q": query}))[1]).get("records", [])
                if not rows or not all(row["key"].startswith(collection + "/") for row in rows) or not any(row["data"].get("event_id") == key for row in rows):
                    raise RuntimeError(f"Shipping Logs separate collection query failed: {collection}")
                event_ids[collection] = {row["data"].get("event_id") for row in rows}
                keyed = json.loads(require_status(args.origin, f"/v1/databases/{database_id}/records/{collection}/{key}")[1])
                if keyed.get("data", {}).get("event_id") != key:
                    raise RuntimeError(f"Shipping Logs representative keyed read failed: {collection}")
            require_recovery_separation(event_ids)

        if args.publication_receipt and database["id"] == "countries":
            require_countries_reference(args.origin, database_id, recordsets, api_profile)

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
            query_path = f"/v1/databases/{database_id}/tugql?{urlencode({'q': query, 'parameters': json.dumps({'foreignKey': source[relationship['sourceField']]}, separators=(',', ':'))})}"
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
                f"/v1/databases/{database_id}/tugql?{urlencode({'q': query})}",
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

    if args.rollback_acceptance and len(unverified) == len(databases):
        raise RuntimeError("rollback revision serves no provider at this inventory's pins")
    identity = None
    if args.publication_receipt:
        identity = json.loads(args.publication_receipt.read_text())
        if identity.get("mergeSha") != identity.get("deployedSourceSha") or identity.get("candidateImage") != identity.get("deployedImage") or not identity.get("deployedRevision"):
            raise RuntimeError("publication receipt lacks verified serving identity")
        if {database["id"] for database in databases} != PUBLISHED_PROVIDERS:
            raise RuntimeError("publication acceptance requires all nine providers")
        boundary = json.loads(Path(identity["originBoundaryReceipt"]).read_text())
        observed = boundary.get("originObservations", [])
        if not observed or any(item.get("status") not in (403, 404) for item in observed):
            raise RuntimeError("missing direct origin rejection proof")
        for host in ("cloud.openvaultdb.com", "ovdb.demodb.dev"):
            if not any(item.get("host") == host and item.get("status") == 403 for item in observed):
                raise RuntimeError("missing mapped host origin rejection proof")
        _, discovery = require_status("https://demodb.dev", "/.well-known/openvaultdb")
        require_publication_discovery(discovery, [database["id"] for database in databases])
    if args.receipt:
        args.receipt.write_text(json.dumps({"verifiedAt": int(time.time()), "servingIdentity": identity, "providers": [database["id"] for database in databases if database["id"] not in {item["id"] for item in unverified}], "unverifiedProviders": unverified, "requests": OBSERVATIONS, "requestPolicy": REQUEST_POLICY.settings()}, sort_keys=True, indent=2) + "\n")


if __name__ == "__main__":
    main()
