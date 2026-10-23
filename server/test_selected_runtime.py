"""Tiny native/public graph fixtures; no provider download or public admission."""
import copy
import json
import sqlite3
import tempfile
import unittest
from pathlib import Path

import prepare_fixture
import prepare_providers

KEYS = ["simple", "a/b", "with space", 'quote\".$#[]', "Éire_日本"]


def build_provider(root, database_id, native_count, selected_count, *, bounded=True, published=False):
    repository = "synthetic/" + database_id
    folder = root / repository
    folder.mkdir(parents=True)
    native = folder / "native.sqlite"
    connection = sqlite3.connect(native)
    names = [f"table_{i:02}" for i in range(native_count)]
    tables = []
    for name in names:
        connection.execute(f'CREATE TABLE "{name}" (native_key TEXT PRIMARY KEY, id INTEGER, __ovdb_record_id TEXT, nullable TEXT, number REAL, payload BLOB)')
        for index, key in enumerate(KEYS):
            connection.execute(f'INSERT INTO "{name}" VALUES (?,?,?,?,?,?)', (key, index, "native-helper-like", None, 1.25, b"\x00\xff"))
        columns = [{"name": c[1], "type": c[2]} for c in connection.execute(f'PRAGMA table_info("{name}")')]
        tables.append({"name": name, "kind": "table", "rowCount": len(KEYS), "columns": columns, "primaryKey": [], "foreignKeys": []})
    connection.execute(f'CREATE VIEW native_view AS SELECT native_key FROM "{names[-1]}"')
    connection.commit()
    connection.close()
    if not bounded:
        # Legacy adapter reserves id, so use the historical native column shape.
        connection = sqlite3.connect(native)
        for name in names:
            connection.execute(f'ALTER TABLE "{name}" DROP COLUMN id')
            tables[0]["columns"] = [c for c in tables[0]["columns"] if c["name"] != "id"]
        connection.commit(); connection.close()
    artifact_data = native.read_bytes()
    artifact_pin = {"path": "native.sqlite", "bytes": len(artifact_data), "sha256": prepare_providers.sha256(artifact_data)}
    cloud = "https://cloud.openvaultdb.com"
    canonical = cloud + "/ovdb/dbs/" + database_id
    homepage = "https://github.com/" + repository if bounded else "https://" + database_id + ".example.test/"
    source = {"repository": "https://github.com/" + repository, "revision": "b" * 40, "path": "native.sqlite", "sha256": artifact_pin["sha256"], "license": "CC0-1.0", "licenseFile": "LICENSE.txt"}
    manifest = {"id": database_id, "homepage": homepage, "source": source, "capabilities": {"ovdb": {"readOnly": True, "available": published if bounded else True, "query": published if bounded else True, "canonicalUrl": canonical}}}
    if not bounded:
        manifest["siteHost"] = database_id + ".example.test"
    deployment = {"engine": "sqlite", "url": canonical, "discovery": cloud + "/.well-known/openvaultdb"}
    server_id = cloud + "/ovdb/" if bounded else cloud
    descriptor = {"format": "ovdb-database/draft-1", "localId": database_id, "id": canonical, "homepage": homepage, "serverId": server_id,
                  "apiUrl": cloud + "/v1/databases/" + database_id, "serverDbBaseUrl": canonical if bounded else cloud + "/db/" + database_id + "/", "deployment": deployment,
                  "capabilities": {"read": True, "write": False, "query": published if bounded else True}, "recordsets": tables[:selected_count], "provenance": {**source}, "licences": {"data": "CC0-1.0"}}
    docs = {"manifest": ("provider.json", manifest), "contract": ("contract.json", {"contractVersion": 1, "manifest": {"id": database_id}, "schema": {"database": {"id": database_id}, "tables": tables}, "exports": [{**artifact_pin, "format": "sqlite"}]}), "checksums": ("checksums.json", {"contractVersion": 1, "files": {"native.sqlite": artifact_pin}}), "databaseManifest": ("descriptor.json", descriptor)}
    files = {"artifact": artifact_pin}
    for kind, (name, document) in docs.items():
        data = json.dumps(document).encode()
        (folder / name).write_bytes(data)
        files[kind] = {"path": name, "bytes": len(data), "sha256": prepare_providers.sha256(data)}
    license_data = b"CC0 synthetic fixture"
    (folder / "LICENSE.txt").write_bytes(license_data)
    files["license"] = {"path": "LICENSE.txt", "bytes": len(license_data), "sha256": prepare_providers.sha256(license_data)}
    provider = {"id": database_id, "repository": repository, "revision": "a" * 40, "recordKeyFormat": "natural", "files": files, "corsOrigins": ["https://" + database_id + ".example.test"], "smokeRecordsets": names[:selected_count]}
    if bounded:
        publisher = {"format": "ovdb-manifest/draft-1", "id": database_id, "homepage": homepage, "url": canonical, "deployment": {**deployment, "recordset_page": canonical + "/collections/{name}"}, "recordsets": names[:selected_count]}
        # JSON is a YAML subset, allowing standard-library synthetic generation.
        data = json.dumps(publisher).encode()
        (folder / "ovdb.yaml").write_bytes(data)
        provider.update(servingAdapter="separate-id/1", readProfile="bounded-immutable/1", requirePublishedQuery=published,
                        publisherManifest={"path": "ovdb.yaml", "sha256": prepare_providers.sha256(data), "bytes": len(data)})
    return provider


def build_inventory(directory):
    root = directory / "sources"
    providers = [build_provider(root, "candidate", 13, 8), build_provider(root, "final", 3, 3, published=True), build_provider(root, "legacy", 1, 1, bounded=False)]
    inventory = directory / "providers.json"
    inventory.write_text(json.dumps({"version": 2, "databases": providers}))
    return prepare_providers.prepare_inventory(inventory, directory / "output", root)


class SelectedPreparationTest(unittest.TestCase):
    def test_no_website_homepage_is_exact_pinned_repository(self):
        for variation in ("absent-source-homepage", "canonical", "unrelated", "wrong-repository", "http", "null-source-homepage", "missing-publisher-homepage", "missing-descriptor-homepage"):
            with self.subTest(variation=variation), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                provider = build_provider(root, "candidate", 1, 1)
                folder = root / provider["repository"]
                manifest = json.loads((folder / "provider.json").read_bytes())
                publisher = json.loads((folder / "ovdb.yaml").read_bytes())
                descriptor = json.loads((folder / "descriptor.json").read_bytes())
                if variation == "absent-source-homepage":
                    del manifest["homepage"]
                elif variation == "null-source-homepage":
                    manifest["homepage"] = None
                elif variation == "missing-publisher-homepage":
                    del publisher["homepage"]
                elif variation == "missing-descriptor-homepage":
                    del descriptor["homepage"]
                elif variation in ("unrelated", "wrong-repository", "http"):
                    homepage = {"unrelated": "https://unrelated.example.test/", "wrong-repository": "https://github.com/synthetic/other", "http": "http://github.com/synthetic/candidate"}[variation]
                    # Agreement alone must not authorize an unrelated no-website homepage.
                    for document in (manifest, publisher, descriptor):
                        document["homepage"] = homepage
                for kind, filename, document in (("manifest", "provider.json", manifest), ("databaseManifest", "descriptor.json", descriptor), ("publisherManifest", "ovdb.yaml", publisher)):
                    data = json.dumps(document).encode()
                    (folder / filename).write_bytes(data)
                    pin = provider["publisherManifest"] if kind == "publisherManifest" else provider["files"][kind]
                    pin.update(bytes=len(data), sha256=prepare_providers.sha256(data))
                fetch = lambda _repository, _revision, path: (folder / path).read_bytes()
                if variation in ("absent-source-homepage", "canonical"):
                    prepare_providers.validate_provider(provider, fetch)
                else:
                    with self.assertRaises(ValueError):
                        prepare_providers.validate_provider(provider, fetch)

    def test_legacy_website_homepage_fallback_is_preserved(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            provider = build_provider(root, "legacy", 1, 1, bounded=False)
            folder = root / provider["repository"]
            manifest = json.loads((folder / "provider.json").read_bytes())
            del manifest["homepage"]
            data = json.dumps(manifest).encode()
            (folder / "provider.json").write_bytes(data)
            provider["files"]["manifest"].update(bytes=len(data), sha256=prepare_providers.sha256(data))
            prepare_providers.validate_provider(provider, lambda _repository, _revision, path: (folder / path).read_bytes())

    def test_native16_selected11_and_legacy(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            runtime = json.loads(build_inventory(directory).read_text())
            self.assertEqual(2, runtime["version"])
            for provider in runtime["databases"]:
                output = directory / "output" / (provider["id"] + ".sqlite")
                native = directory / "sources" / provider["providerRepository"] / "native.sqlite"
                before, after = sqlite3.connect(native), sqlite3.connect(output)
                selected_count = len(provider["smokeRecordsets"])
                names = [row[0] for row in before.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
                for index, name in enumerate(names):
                    original_columns = [tuple(row[:6]) for row in before.execute(f'PRAGMA table_xinfo("{name}")')]
                    added = index < selected_count
                    generated = ("__ovdb_record_id_1", "ovdb_" + name + "_id") if provider["id"] != "legacy" and added else (("id", "ovdb_" + name + "_id") if added else None)
                    self.assertEqual(prepare_fixture.table_metadata(before, name), prepare_fixture.table_metadata(after, name, generated))
                    self.assertEqual(prepare_fixture.native_value_digest(before, name, original_columns), prepare_fixture.native_value_digest(after, name, original_columns))
                    if generated:
                        got = [row[0] for row in after.execute(f'SELECT "{generated[0]}" FROM "{name}" ORDER BY "{generated[0]}"')]
                        self.assertEqual(sorted(KEYS), got)
                self.assertEqual(list(before.execute("SELECT name,sql FROM sqlite_master WHERE type='view'")), list(after.execute("SELECT name,sql FROM sqlite_master WHERE type='view'")))
                before.close(); after.close()

    def test_inventory_requires_closed_publisher_pin_before_fetch(self):
        with tempfile.TemporaryDirectory() as temporary:
            provider = build_provider(Path(temporary), "fixture", 2, 1)
            for value in (None, {}, {**provider["publisherManifest"], "extra": 1}, {**provider["publisherManifest"], "bytes": True}, {**provider["publisherManifest"], "path": "../ovdb.yaml"}):
                with self.subTest(value=value):
                    item = copy.deepcopy(provider); item["publisherManifest"] = value
                    with self.assertRaises(ValueError):
                        prepare_providers.validate_inventory({"version": 2, "databases": [item]})

    def test_selection_descriptor_and_candidate_flags_fail_before_artifact_fetch(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            original = build_provider(root, "fixture", 2, 1)
            folder = root / original["repository"]
            for mutation in ("hidden", "duplicate", "query", "identity"):
                provider = copy.deepcopy(original)
                publisher = json.loads((folder / "ovdb.yaml").read_bytes())
                if mutation == "hidden": publisher["recordsets"] = ["table_01"]
                elif mutation == "duplicate": publisher["recordsets"] *= 2
                elif mutation == "identity": publisher["id"] = "other"
                else: provider["requirePublishedQuery"] = True
                data = json.dumps(publisher).encode()
                provider["publisherManifest"].update(bytes=len(data), sha256=prepare_providers.sha256(data))
                def fetch(repository, revision, path):
                    self.assertEqual(provider["repository"], repository); self.assertEqual("a" * 40, revision)
                    if path == "native.sqlite": self.fail("artifact fetched before selection admission")
                    return data if path == "ovdb.yaml" else (folder / path).read_bytes()
                with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                    prepare_providers.validate_provider(provider, fetch)

    def test_descriptor_canonical_member_parity_before_artifact_fetch(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            original = build_provider(root, "fixture", 2, 1)
            folder = root / original["repository"]
            baseline = json.loads((folder / "descriptor.json").read_bytes())
            for field in ("format", "localId", "id", "homepage", "serverId", "serverDbBaseUrl", "apiUrl", "deployment", "capabilities", "recordsets"):
                for mutation in ("alias-only", "alias-plus-canonical", "missing", "null", "wrong-type"):
                    provider = copy.deepcopy(original)
                    document = copy.deepcopy(baseline)
                    value = document[field]
                    if mutation == "alias-only":
                        document[field[0].upper() + field[1:]] = document.pop(field)
                    elif mutation == "alias-plus-canonical":
                        document[field[0].upper() + field[1:]] = value
                    elif mutation == "missing":
                        document.pop(field)
                    elif mutation == "null":
                        document[field] = None
                    else:
                        document[field] = False
                    data = json.dumps(document).encode()
                    provider["files"]["databaseManifest"].update(bytes=len(data), sha256=prepare_providers.sha256(data))
                    def fetch(repository, revision, path):
                        if path == "native.sqlite":
                            self.fail("artifact fetched before exact descriptor member admission")
                        return data if path == "descriptor.json" else (folder / path).read_bytes()
                    with self.subTest(field=field, mutation=mutation), self.assertRaises(ValueError):
                        prepare_providers.validate_provider(provider, fetch)
            document = copy.deepcopy(baseline)
            document.update(title="Legitimate title", model={"id": "modelspec://synthetic/fixture"})
            prepare_providers.validate_descriptor_members(document)

    def test_publisher_pin_and_envelope_member_spelling(self):
        with tempfile.TemporaryDirectory() as temporary:
            provider = build_provider(Path(temporary), "fixture", 2, 1)
            for pin_name in ("publisherManifest", "publicDescriptor"):
                for field in ("path", "sha256", "bytes"):
                    for duplicate in (False, True):
                        mutated = copy.deepcopy(provider)
                        # Public runtime pin originates from files.databaseManifest.
                        pin = mutated["publisherManifest"] if pin_name == "publisherManifest" else mutated["files"]["databaseManifest"]
                        pin[field[0].upper() + field[1:]] = pin[field]
                        if not duplicate:
                            pin.pop(field)
                        with self.subTest(pin=pin_name, field=field, duplicate=duplicate), self.assertRaises(ValueError):
                            prepare_providers.validate_inventory({"version": 2, "databases": [mutated]})
            for field in ("readProfile", "servingAdapter", "publisherManifest", "requirePublishedQuery"):
                for duplicate in (False, True):
                    mutated = copy.deepcopy(provider)
                    mutated[field[0].upper() + field[1:]] = mutated[field]
                    if not duplicate:
                        mutated.pop(field)
                    with self.subTest(binding=field, duplicate=duplicate), self.assertRaises(ValueError):
                        prepare_providers.validate_inventory({"version": 2, "databases": [mutated]})
