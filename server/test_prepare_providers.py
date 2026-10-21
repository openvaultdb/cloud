import json
import copy
import sys
from unittest.mock import patch

import smoke_inventory
import gzip
import hashlib
import sqlite3
import tempfile
import unittest
from pathlib import Path

import prepare_providers


PROJECTS_ROOT = Path(__file__).resolve().parents[6]
INVENTORY = Path(__file__).with_name("providers.json")


class PrepareProvidersTest(unittest.TestCase):
    def scoped_rights_fixture(self, database_id="chinook"):
        names = prepare_providers.SHOWCASE_RECORDSETS[database_id]
        blobs = {
            "manifest": json.dumps({"source": {"license":"MIT", "licenseFile":"data-source/LICENSE.md"}}).encode(),
            "databaseManifest": json.dumps({"licences":{"data":"MIT"},"provenance":{"license":"MIT"},"recordsets":[{"name":name,"kind":"table"} for name in names]}).encode(),
            "contract": json.dumps({"schema":{"tables":[{"name":name,"kind":"table"} for name in names]}}).encode(),
            "license": b"MIT fixture-only license text\n",
        }
        provider = {"id":database_id,"repository":"demo-db/"+database_id,"revision":"a"*40,"files":{key:{"path":"data-source/LICENSE.md" if key=="license" else key+".json","sha256":hashlib.sha256(value).hexdigest()} for key,value in blobs.items()}}
        return provider, blobs

    def test_scoped_rights_derive_only_three_verified_publisher_recordsets(self):
        for database_id in ("northwind", "chinook"):
            provider, blobs = self.scoped_rights_fixture(database_id)
            rights = prepare_providers.scoped_recordset_licenses(provider, blobs)
            self.assertEqual(set(rights), set(prepare_providers.SHOWCASE_RECORDSETS[database_id]))
            for declaration in rights.values():
                self.assertEqual(declaration, {"spdx":"MIT","url":f"https://github.com/demo-db/{database_id}/blob/{'a'*40}/data-source/LICENSE.md","text":blobs["license"].decode()})
        self.assertIsNone(prepare_providers.scoped_recordset_licenses({"id":"shipping"}, {}))

    def test_scoped_rights_refuse_missing_membership_license_and_pin_changes(self):
        for corruption in ("native", "public", "data-license", "provenance-license", "license-path", "license-bytes", "repository", "revision"):
            with self.subTest(corruption=corruption):
                provider, blobs = self.scoped_rights_fixture()
                if corruption in ("native", "public", "data-license", "provenance-license"):
                    key = "contract" if corruption=="native" else "databaseManifest"
                    document=json.loads(blobs[key])
                    if corruption=="native": document["schema"]["tables"][0]["name"]="customer"
                    elif corruption=="public": document["recordsets"][0]["name"]="customer"
                    elif corruption=="data-license": document["licences"]["data"]="CC0-1.0"
                    else: document["provenance"]["license"]="CC0-1.0"
                    blobs[key]=json.dumps(document).encode()
                    provider["files"][key]["sha256"]=hashlib.sha256(blobs[key]).hexdigest()
                elif corruption=="license-path": provider["files"]["license"]["path"]="README.md"
                elif corruption=="license-bytes": blobs["license"]+=b"changed"
                elif corruption=="repository": provider["repository"]="unrelated/chinook"
                else: provider["revision"]="main"
                with self.assertRaises(ValueError): prepare_providers.scoped_recordset_licenses(provider, blobs)

    def test_pinned_provider_repositories_prepare_a_runtime_inventory(self) -> None:
        providers_root = PROJECTS_ROOT
        providers = prepare_providers.load_inventory(INVENTORY)
        self.assertEqual(["adventureworks", "employees", "chinook", "northwind", "pubs", "sakila", "shipping", "shipping-logs"], [provider["id"] for provider in providers])
        by_id = {provider["id"]: provider for provider in providers}
        self.assertTrue(by_id["adventureworks"]["requirePublishedQuery"])
        self.assertTrue(by_id["employees"]["requirePublishedQuery"])
        self.assertTrue(by_id["pubs"].get("requirePublishedQuery", True))
        self.assertTrue(by_id["sakila"]["requirePublishedQuery"])
        self.assertIn("film_actor", by_id["sakila"]["smokeRecordsets"])
        self.assertEqual(["dbo.DatabaseLog", "dbo.ErrorLog"], by_id["adventureworks"]["emptyRecordsets"])
        self.assertEqual({"Production.ProductPhoto": ["ThumbNailPhoto", "LargePhoto"]}, by_id["adventureworks"]["blobSmokeFields"])
        if not all((providers_root / provider["repository"]).is_dir() for provider in providers):
            missing = [str(providers_root / provider["repository"]) for provider in providers if not (providers_root / provider["repository"]).is_dir()]
            present = len(providers) - len(missing)
            self.skipTest(f"{present}/{len(providers)} configured provider clone directories exist; missing: {', '.join(missing)}; CI exercises immutable remote fetches during fixture preparation")
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            runtime_path = prepare_providers.prepare_inventory(INVENTORY, output, providers_root)
            runtime = json.loads(runtime_path.read_text())
            self.assertEqual(2, runtime["version"])
            self.assertEqual(["adventureworks", "employees", "chinook", "northwind", "pubs", "sakila", "shipping", "shipping-logs"], [database["id"] for database in runtime["databases"]])
            for database in runtime["databases"]:
                self.assertTrue((output / database["manifest"]).is_file())
                self.assertTrue((output / database["license"]).is_file())
                self.assertEqual(40, len(database["providerRevision"]))
                self.assertIn("/", database["providerRepository"])
                self.assertTrue((output / f"{database['id']}.source-sha256").is_file())

    def test_smoke_actual_helper_supports_both_inventory_engines(self) -> None:
        for engine, identity in (("sqlite", "id"), ("ingitdb", "event_id")):
            with self.subTest(engine=engine), tempfile.TemporaryDirectory() as temporary:
                inventory = Path(temporary) / "inventory.json"
                inventory.write_text(json.dumps({"version": 2, "databases": [{
                    "id": "sample", "storageEngine": engine, "corsOrigins": ["https://sample.test"],
                    "smokeRecordsets": ["baseline"],
                }]}))
                paths = []
                data = {identity: "key1", "value": "original"}
                def request(origin, path, method="GET", body=None, headers=None):
                    paths.append(path)
                    response_headers = {"Access-Control-Allow-Origin": "https://sample.test", "Vary": "Origin",
                                        "Access-Control-Allow-Methods": "POST", "Cache-Control": "no-store" if engine == "ingitdb" else "max-age=86400",
                                        "Access-Control-Allow-Headers": "Content-Type,OVDB-Page-Size,OVDB-Page-Token,OVDB-Page-Close"}
                    if headers and headers.get("Origin") == "https://undeclared.invalid":
                        response_headers.pop("Access-Control-Allow-Origin")
                    if method == "OPTIONS":
                        return 204, response_headers, b""
                    if method == "PUT":
                        return 403, response_headers, b""
                    if path == "/ovdb/dbs/sample":
                        return 200, response_headers, b"https://cloud.openvaultdb.com/ovdb/dbs/sample"
                    if "/collections/" in path and engine == "ingitdb":
                        self.fail("native engine has no SQLite collection HTML profile")
                    result = {"key": "key1", "data": data}
                    if "/dtql?" in path:
                        result = {"records": [result]}
                    return 200, response_headers, json.dumps(result).encode()
                with patch.object(sys, "argv", ["smoke_inventory.py", "--origin", "https://cloud.test", "--inventory", str(inventory)]), patch.object(smoke_inventory, "request", request):
                    smoke_inventory.main()
                self.assertIn("/v1/databases/sample/records/baseline/key1", paths)

    def test_smoke_cache_contracts_remain_engine_specific(self):
        for provider, accepted, rejected in [
            ({"id": "native", "storageEngine": "ingitdb"}, "no-store", ["", "max-age=86400", "no-store, max-age=86400"]),
            ({"id": "bounded", "readProfile": "bounded-immutable/1"}, "no-store", ["", "max-age=86400"]),
            ({"id": "legacy", "storageEngine": "sqlite"}, "max-age=86400", ["", "no-store", "max-age=300"]),
        ]:
            with self.subTest(provider=provider["id"]):
                smoke_inventory.require_query_cache_control(provider, accepted)
                for value in rejected:
                    with self.assertRaises(RuntimeError):
                        smoke_inventory.require_query_cache_control(provider, value)

    def native_fixture(self):
        ids = ("baseline", "recovery11045example", "recovery11062example")
        root = {name: f"collections/{name}" for name in ids}
        columns = {"event_id": {"type": "string", "required": True, "min_length": 1},
                   "kind": {"type": "string", "required": True, "min_length": 2},
                   "sequence": {"type": "int", "required": True}, "synthetic": {"type": "bool", "required": True}}
        definition = {"record_file": {"name": "records.jsonl", "type": "[]map[string]any", "format": "jsonl"},
                      "primary_key": ["event_id"], "columns": columns, "columns_order": list(columns)}
        blobs = {"ingitdb/.ingitdb/settings.yaml": b"{}", "ingitdb/.ingitdb/root-collections.yaml": json.dumps(root).encode()}
        tables, files, logs = [], {}, []
        for name in ids:
            blobs[f"ingitdb/collections/{name}/.collection/definition.yaml"] = json.dumps(definition).encode()
            path = f"ingitdb/collections/{name}/records.jsonl"
            rows = [{"event_id": f"{name}-{n}", "kind": "observed", "sequence": n, "synthetic": True} for n in range(2)]
            blobs[path] = ("\n".join(json.dumps(row) for row in rows) + "\n").encode()
            pin = {"sha256": hashlib.sha256(blobs[path]).hexdigest(), "bytes": len(blobs[path])}
            files[name] = {"path": path, "sourcePath": f"fixture/logs/{name}.jsonl", "classification": "baseline" if name == "baseline" else "recovery-example", "rowCount": 2, **pin}
            logs.append({"path": f"logs/{name}.jsonl", **pin})
            tables.append({"name": name, "rowCount": 2, "columns": [{"name": key, "type": value["type"], "nullable": False} for key, value in columns.items()]})
        fetched = {"manifest": json.dumps({"id": "shipping-logs", "storage": {"engine": "ingitdb", "path": "ingitdb"}, "source": {"path": "fixture/manifest.json", "files": files}}).encode(),
                   "contract": json.dumps({"schema": {"database": {"id": "shipping-logs"}, "tables": tables}}).encode(),
                   "databaseManifest": json.dumps({"localId": "shipping-logs", "capabilities": {"read": True, "query": True, "write": False}}).encode()}
        blobs["metadata/schema.json"] = json.dumps({"database": {"id": "shipping-logs"}, "tables": tables}).encode()
        blobs["source-manifest.json"] = json.dumps({"logs": logs}).encode()
        return blobs, fetched

    def test_native_semantic_corruption_with_consistent_updated_pins(self) -> None:
        records_path = "ingitdb/collections/baseline/records.jsonl"
        definition_path = "ingitdb/collections/baseline/.collection/definition.yaml"
        def change_json(blobs, path, change):
            value = json.loads(blobs[path]); change(value); blobs[path] = json.dumps(value).encode()
        def change_record(blobs, change):
            rows = [json.loads(line) for line in blobs[records_path].splitlines()]
            change(rows)
            blobs[records_path] = ("\n".join(json.dumps(row) for row in rows) + "\n").encode()
        mutations = {
            "valid": lambda b: None,
            "root alias": lambda b: change_json(b, "ingitdb/.ingitdb/root-collections.yaml", lambda v: v.update(alias="collections/baseline")),
            "missing member": lambda b: b.pop(records_path),
            "extra definition": lambda b: b.update({"ingitdb/collections/extra/.collection/definition.yaml": b"{}"}),
            "record filename": lambda b: change_json(b, definition_path, lambda v: v["record_file"].update(name="other.jsonl")),
            "primary key": lambda b: change_json(b, definition_path, lambda v: v.update(primary_key=["kind"])),
            "schema type": lambda b: change_json(b, definition_path, lambda v: v["columns"]["kind"].update(type="int")),
            "required constraint": lambda b: change_json(b, definition_path, lambda v: v["columns"]["kind"].update(required=False)),
            "unknown constraint": lambda b: change_json(b, definition_path, lambda v: v["columns"]["kind"].update(unsupported=True)),
            "short string": lambda b: change_record(b, lambda rows: rows[0].update(kind="x")),
            "blank identity": lambda b: change_record(b, lambda rows: rows[0].update(event_id=" ")),
            "duplicate identity": lambda b: change_record(b, lambda rows: rows[1].update(event_id=rows[0]["event_id"])),
            "wrong primitive": lambda b: change_record(b, lambda rows: rows[0].update(sequence=True)),
            "extra field": lambda b: change_record(b, lambda rows: rows[0].update(extra="x")),
            "missing field": lambda b: change_record(b, lambda rows: rows[0].pop("kind")),
            "pinned schema": lambda b: change_json(b, "metadata/schema.json", lambda v: v["tables"][0]["columns"][0].update(type="bool")),
            "source hash": lambda b: change_json(b, "source-manifest.json", lambda v: v["logs"][0].update(sha256="0" * 64)),
            "source length": lambda b: change_json(b, "source-manifest.json", lambda v: v["logs"][0].update(bytes=0)),
            "source alias": lambda b: change_json(b, "source-manifest.json", lambda v: v["logs"][0].update(path=v["logs"][1]["path"])),
        }
        for name, mutate in mutations.items():
            with self.subTest(mutation=name):
                blobs, fetched = self.native_fixture()
                mutate(blobs)
                native_pins = [{"path": path, "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data)} for path, data in blobs.items() if path.startswith("ingitdb/")]
                metadata_pins = [{"path": path, "sha256": hashlib.sha256(blobs[path]).hexdigest(), "bytes": len(blobs[path])} for path in ("source-manifest.json", "metadata/schema.json")]
                manifest = json.loads(fetched["manifest"])
                manifest["source"]["sha256"] = metadata_pins[0]["sha256"]
                fetched["manifest"] = json.dumps(manifest).encode()
                # Keep all transport pins/checksums consistent, reaching semantic rejection.
                fetched["checksums"] = json.dumps({"files": {pin["path"]: {key: pin[key] for key in ("sha256", "bytes")} for pin in native_pins}}).encode()
                provider = {"id": "shipping-logs", "repository": "demo-db/shipping-logs", "revision": "a" * 40, "nativeFiles": native_pins, "metadataFiles": metadata_pins}
                validate = lambda: prepare_providers.validate_native_provider(provider, lambda repo, revision, path: blobs[path], copy.deepcopy(fetched))
                if name == "valid":
                    self.assertEqual(8, len(validate()["nativeFiles"]))
                else:
                    with self.assertRaises(ValueError):
                        validate()

    def test_inventory_rejects_mutable_revisions_and_unsafe_artifact_paths(self) -> None:
        document = json.loads(INVENTORY.read_text())
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "providers.json"
            document["databases"][0]["revision"] = "main"
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "immutable full Git commit SHA"):
                prepare_providers.load_inventory(path)

            document = json.loads(INVENTORY.read_text())
            document["databases"][0]["files"]["artifact"]["path"] = "../../secret.sqlite"
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "must not escape"):
                prepare_providers.load_inventory(path)

            document = json.loads(INVENTORY.read_text())
            document["databases"][0]["corsOrigins"][0] = "https://user@example.test"
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "HTTPS origins"):
                prepare_providers.load_inventory(path)

            document = json.loads(INVENTORY.read_text())
            document["databases"][0]["emptyRecordsets"] = [document["databases"][0]["smokeRecordsets"][0]]
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "both queried and expected empty"):
                prepare_providers.load_inventory(path)

    def test_inventory_accepts_verified_chunked_sqlite_descriptors_and_bounds_them(self) -> None:
        document = json.loads(INVENTORY.read_text())
        artifact = document["databases"][0]["files"]["artifact"]
        artifact.update({
            "compression": "gzip",
            "encodedPath": artifact["path"] + ".gz",
            "decodedBytes": artifact["bytes"],
            "decodedSha256": artifact["sha256"],
            "chunks": [{"path": artifact["path"] + ".gz.part-0001", "bytes": 2, "sha256": "a" * 64}],
        })
        artifact["bytes"] = 2
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "providers.json"
            path.write_text(json.dumps(document))
            self.assertEqual("adventureworks", prepare_providers.load_inventory(path)[0]["id"])

            artifact["decodedBytes"] = prepare_providers.MAX_DECODED_ARTIFACT_BYTES + 1
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "decodedBytes"):
                prepare_providers.load_inventory(path)
            artifact["decodedBytes"] = 1
            artifact["bytes"] = prepare_providers.MAX_ENCODED_ARTIFACT_BYTES + 1
            path.write_text(json.dumps(document))
            with self.assertRaisesRegex(ValueError, "encoded-stream limit"):
                prepare_providers.load_inventory(path)

    def test_adventureworks_contract_export_matches_encoded_and_decoded_inventory_pins(self) -> None:
        # These are the real sqlite export fields from demo-db/adventureworks
        # at cd8dcdf2079fe31480ad6d6c024b8c17cb91beea, contract
        # 462d32735cb3fb85868af977741a83cdb3add508809503934ef7a08daa36d682.
        chunks = [
            {"path": "artifacts/adventureworks.sqlite.gz.part-0001", "bytes": 26214400, "sha256": "3c59d3812fbc29d8a3070da9bef2306b25cff219fa08bb3fd82e5838f92d080a"},
            {"path": "artifacts/adventureworks.sqlite.gz.part-0002", "bytes": 9075051, "sha256": "80b1a2c58648469aebe5b47b9dfaef3b3c84d14e141b5297e6f5f94953ef28a7"},
        ]
        artifact = {
            "path": "artifacts/adventureworks.sqlite",
            "compression": "gzip",
            "encodedPath": "artifacts/adventureworks.sqlite.gz",
            "bytes": 35289451,
            "sha256": "533165298eb696d330be6a2fc648ba56d985be491d511ca581d7ea64736e1a28",
            "decodedBytes": 125276160,
            "decodedSha256": "e0f352f0e3a28ff15158130c237a91d58f1b60fffa7f30d2c412b92df4065dfd",
            "chunks": chunks,
        }
        contract_export = {"path": artifact["path"], "format": "sqlite", **artifact}

        prepare_providers.validate_sqlite_export("adventureworks", [contract_export], artifact)

        wrong_encoded_hash = {**contract_export, "sha256": artifact["decodedSha256"]}
        with self.assertRaisesRegex(ValueError, "encoded hash or size"):
            prepare_providers.validate_sqlite_export("adventureworks", [wrong_encoded_hash], artifact)

        wrong_decoded_size = {**contract_export, "decodedBytes": artifact["bytes"]}
        with self.assertRaisesRegex(ValueError, "decoded hash or size"):
            prepare_providers.validate_sqlite_export("adventureworks", [wrong_decoded_size], artifact)

        wrong_chunks = {**contract_export, "chunks": [*chunks[:-1], {**chunks[-1], "path": "artifacts/wrong.part"}]}
        with self.assertRaisesRegex(ValueError, "chunks disagree"):
            prepare_providers.validate_sqlite_export("adventureworks", [wrong_chunks], artifact)

    def test_rejects_provider_bytes_that_disagree_with_pins(self) -> None:
        with self.assertRaisesRegex(ValueError, "SHA-256"):
            prepare_providers.verify_blob(b"changed", "0" * 64, 7, "fixture")

    def test_chunked_gzip_fixture_is_verified_and_decoded_without_changing_bytes(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            database_path = Path(temporary) / "native.sqlite"
            connection = sqlite3.connect(database_path)
            connection.execute("CREATE TABLE native_table (id INTEGER PRIMARY KEY, value TEXT)")
            connection.execute("INSERT INTO native_table VALUES (1, 'preserved')")
            connection.commit()
            connection.close()
            decoded = database_path.read_bytes()
            encoded = gzip.compress(decoded, mtime=0)
            split = max(1, len(encoded) // 2)
            chunks = [encoded[:split], encoded[split:]]
            names = ["artifacts/native.sqlite.gz.part-0001", "artifacts/native.sqlite.gz.part-0002"]
            chunk_descriptors = [
                {"path": name, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}
                for name, data in zip(names, chunks)
            ]
            artifact = {
                "path": "artifacts/native.sqlite",
                "compression": "gzip",
                "encodedPath": "artifacts/native.sqlite.gz",
                "bytes": len(encoded),
                "sha256": hashlib.sha256(encoded).hexdigest(),
                "decodedBytes": len(decoded),
                "decodedSha256": hashlib.sha256(decoded).hexdigest(),
                "chunks": chunk_descriptors,
            }
            provider = {"id": "fixture", "repository": "demo-db/fixture", "revision": "a" * 40, "files": {"artifact": artifact}}
            files = {name: content for name, content in zip(names, chunks)}
            checksums = {"files": {entry["path"]: {"bytes": entry["bytes"], "sha256": entry["sha256"]} for entry in chunk_descriptors}}
            fetch = lambda _repository, _revision, path: files[path]
            output = Path(temporary) / "decoded.sqlite"
            result = prepare_providers._fetch_verified_artifact(provider, fetch, checksums, output)
            self.assertEqual(output, result)
            self.assertEqual(decoded, output.read_bytes())

    def test_chunked_gzip_fixture_rejects_bad_chunks_and_decoded_hashes(self) -> None:
        encoded = gzip.compress(b"native sqlite bytes", mtime=0)
        digest = hashlib.sha256(encoded).hexdigest()
        path = "artifacts/native.sqlite.gz.part-0001"
        chunk = {"path": path, "bytes": len(encoded), "sha256": digest}
        provider = {
            "id": "fixture", "repository": "demo-db/fixture", "revision": "a" * 40,
            "files": {"artifact": {
                "path": "artifacts/native.sqlite", "compression": "gzip", "encodedPath": "artifacts/native.sqlite.gz",
                "bytes": len(encoded), "sha256": digest, "decodedBytes": len(b"native sqlite bytes"),
                "decodedSha256": "0" * 64, "chunks": [chunk],
            }},
        }
        checksums = {"files": {path: {"bytes": len(encoded), "sha256": digest}}}
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "decoded.sqlite"
            fetch = lambda _repository, _revision, _path: encoded
            with self.assertRaisesRegex(ValueError, "decoded artifact SHA-256"):
                prepare_providers._fetch_verified_artifact(provider, fetch, checksums, output)
            self.assertFalse(output.exists(), "failed verification removes partial decoded output")
            with self.assertRaisesRegex(ValueError, "chunk .* SHA-256"):
                prepare_providers._fetch_verified_artifact(provider, lambda *_: b"tampered", checksums, output)


if __name__ == "__main__":
    unittest.main()
