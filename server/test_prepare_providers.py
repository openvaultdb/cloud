import json
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
    def test_pinned_provider_repositories_prepare_a_runtime_inventory(self) -> None:
        providers_root = PROJECTS_ROOT
        providers = prepare_providers.load_inventory(INVENTORY)
        self.assertEqual(["adventureworks", "employees", "chinook", "northwind", "pubs", "sakila"], [provider["id"] for provider in providers])
        by_id = {provider["id"]: provider for provider in providers}
        self.assertTrue(by_id["adventureworks"]["requirePublishedQuery"])
        self.assertTrue(by_id["employees"]["requirePublishedQuery"])
        self.assertTrue(by_id["pubs"].get("requirePublishedQuery", True))
        self.assertTrue(by_id["sakila"]["requirePublishedQuery"])
        self.assertIn("film_actor", by_id["sakila"]["smokeRecordsets"])
        self.assertEqual(["dbo.DatabaseLog", "dbo.ErrorLog"], by_id["adventureworks"]["emptyRecordsets"])
        self.assertEqual({"Production.ProductPhoto": ["ThumbNailPhoto", "LargePhoto"]}, by_id["adventureworks"]["blobSmokeFields"])
        if not all((providers_root / provider["repository"]).is_dir() for provider in providers):
            self.skipTest("local provider clones are absent; CI exercises immutable remote fetches during fixture preparation")
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary)
            runtime_path = prepare_providers.prepare_inventory(INVENTORY, output, providers_root)
            runtime = json.loads(runtime_path.read_text())
            self.assertEqual(1, runtime["version"])
            self.assertEqual(["adventureworks", "employees", "chinook", "northwind", "pubs", "sakila"], [database["id"] for database in runtime["databases"]])
            for database in runtime["databases"]:
                self.assertTrue((output / database["manifest"]).is_file())
                self.assertTrue((output / database["license"]).is_file())
                self.assertEqual(40, len(database["providerRevision"]))
                self.assertIn("/", database["providerRepository"])
                self.assertTrue((output / f"{database['id']}.source-sha256").is_file())

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
