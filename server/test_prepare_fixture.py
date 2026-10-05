import hashlib
import importlib.util
import sqlite3
import tempfile
import unittest
from unittest.mock import patch
from pathlib import Path


SCRIPT = Path(__file__).with_name("prepare_fixture.py")
SPEC = importlib.util.spec_from_file_location("prepare_fixture", SCRIPT)
prepare_fixture = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(prepare_fixture)


def make_source(path: Path) -> None:
    connection = sqlite3.connect(path)
    connection.executescript(
        '''
        PRAGMA foreign_keys = ON;
        CREATE TABLE "Order Details" (
            "Order ID" INTEGER NOT NULL,
            "Line Number" INTEGER NOT NULL,
            "Unit Price" MONEY NOT NULL,
            "Exact Price" DECIMAL_TEXT(30,2) NOT NULL,
            "Payload" BLOB,
            PRIMARY KEY ("Order ID", "Line Number"),
            FOREIGN KEY ("Order ID") REFERENCES Orders("Order ID")
        ) WITHOUT ROWID;
        CREATE TABLE Orders (
            "Order ID" INTEGER PRIMARY KEY,
            ParentID INTEGER REFERENCES Orders("Order ID")
        );
        CREATE TABLE discounts (discounttype TEXT, stor_id TEXT);
        CREATE TABLE empty_demo (code TEXT PRIMARY KEY);
        CREATE TABLE shadowed_rowid (rowid TEXT, value TEXT);
        INSERT INTO Orders VALUES (1, NULL), (2, 1);
        INSERT INTO "Order Details" VALUES (1, 1, 3.25, '9007199254740993.12', X'00FF10'), (2, 1, 7.5, '0.50', NULL);
        INSERT INTO discounts VALUES ('customer', '6380');
        INSERT INTO shadowed_rowid VALUES ('duplicate', 'first'), ('duplicate', 'second');
        CREATE VIEW "Order Detail View" AS SELECT "Order ID", "Unit Price" FROM "Order Details";
        '''
    )
    connection.commit()
    connection.close()


class PrepareFixtureTest(unittest.TestCase):
    def test_consumes_verified_staging_source_without_a_second_full_sqlite_copy(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "staging.sqlite"
            make_source(source)
            source_digest = hashlib.sha256(source.read_bytes()).hexdigest()

            manifest = prepare_fixture.prepare(
                source,
                root / "prepared",
                "demo-sales",
                source_digest,
                "natural",
                consume_source=True,
            )

            self.assertFalse(source.exists(), "staging copy should be moved into the serving path")
            self.assertEqual(source_digest, (manifest.parent / "demo-sales.source-sha256").read_text().strip())
            serving = sqlite3.connect(manifest.parent / "demo-sales.sqlite")
            try:
                self.assertEqual(2, serving.execute('SELECT COUNT(*) FROM "Order Details"').fetchone()[0])
                self.assertEqual(2, serving.execute('SELECT COUNT(id) FROM "Order Details"').fetchone()[0])
                self.assertEqual("ok", serving.execute("PRAGMA integrity_check").fetchone()[0])
            finally:
                serving.close()

    def test_preserves_native_metadata_and_adds_ids_for_keyed_and_keyless_tables(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.sqlite"
            make_source(source)
            original_digest = hashlib.sha256(source.read_bytes()).hexdigest()
            prepared = root / "prepared"
            manifest = prepare_fixture.prepare(source, prepared, "demo-sales", original_digest, "natural")
            derived = prepared / "demo-sales.sqlite"

            self.assertEqual(original_digest, hashlib.sha256(source.read_bytes()).hexdigest())
            self.assertIn("database: {id: demo-sales,", manifest.read_text())
            text = manifest.read_text()
            self.assertIn("Order Details", text)
            self.assertIn("Order ID", text)
            self.assertIn("Unit Price", text)
            self.assertNotIn('\\"Order Details\\"', text)
            self.assertNotIn('\\"Order ID\\"', text)
            self.assertIn('"Unit Price": {type: number}', text)
            self.assertIn('"Exact Price": {"type":"decimal","decimal":{"precision":30,"scale":2,"storage":"text"}}', text)
            self.assertIn('"Payload": {type: any}', text)
            self.assertIn('"discounts":', text)
            self.assertIn('"empty_demo":', text)

            original = sqlite3.connect(source)
            serving = sqlite3.connect(derived)
            try:
                self.assertEqual(
                    original.execute('SELECT "Order ID", "Line Number", "Unit Price", "Payload" FROM "Order Details" ORDER BY 1, 2').fetchall(),
                    serving.execute('SELECT "Order ID", "Line Number", "Unit Price", "Payload" FROM "Order Details" ORDER BY 1, 2').fetchall(),
                )
                self.assertEqual(
                    original.execute('SELECT * FROM "Order Detail View" ORDER BY 1').fetchall(),
                    serving.execute('SELECT * FROM "Order Detail View" ORDER BY 1').fetchall(),
                )
                self.assertEqual(
                    original.execute("SELECT sql FROM sqlite_master WHERE type='view' AND name='Order Detail View'").fetchone(),
                    serving.execute("SELECT sql FROM sqlite_master WHERE type='view' AND name='Order Detail View'").fetchone(),
                )
                self.assertEqual(original.execute('SELECT COUNT(*) FROM discounts').fetchone(), serving.execute('SELECT COUNT(*) FROM discounts').fetchone())
                self.assertEqual(original.execute('SELECT COUNT(*) FROM empty_demo').fetchone(), serving.execute('SELECT COUNT(*) FROM empty_demo').fetchone())
                self.assertEqual(0, serving.execute('PRAGMA foreign_key_check').fetchall().__len__())
                for table, expected_count in (("Order Details", 2), ("discounts", 1), ("empty_demo", 0), ("shadowed_rowid", 2)):
                    self.assertEqual(expected_count, serving.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0])
                    self.assertEqual(expected_count, serving.execute(f'SELECT COUNT(id) FROM "{table}"').fetchone()[0])
                self.assertEqual(2, serving.execute('SELECT COUNT(DISTINCT id) FROM shadowed_rowid').fetchone()[0])
                self.assertEqual("ok", serving.execute("PRAGMA integrity_check").fetchone()[0])
            finally:
                original.close()
                serving.close()

            first_derived_hash = hashlib.sha256(derived.read_bytes()).hexdigest()
            second_manifest = prepare_fixture.prepare(source, root / "prepared-again", "demo-sales", original_digest, "natural")
            self.assertEqual(manifest.read_bytes(), second_manifest.read_bytes())
            self.assertEqual(first_derived_hash, hashlib.sha256((root / "prepared-again/demo-sales.sqlite").read_bytes()).hexdigest())

    def test_updates_keyless_tables_in_bounded_batches_and_rejects_casefolded_id(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.sqlite"
            connection = sqlite3.connect(source)
            connection.execute("CREATE TABLE many_rows (value INTEGER)")
            connection.executemany("INSERT INTO many_rows VALUES (?)", ((index,) for index in range(2_105)))
            connection.execute('CREATE TABLE reserved_name ("ID" TEXT)')
            connection.commit()
            connection.close()

            digest = hashlib.sha256(source.read_bytes()).hexdigest()
            with self.assertRaisesRegex(ValueError, "already has an id column"):
                prepare_fixture.prepare(source, root / "out", "sample", digest, "natural")

            connection = sqlite3.connect(source)
            connection.execute("DROP TABLE reserved_name")
            connection.commit()
            connection.close()
            digest = hashlib.sha256(source.read_bytes()).hexdigest()
            prepare_fixture.prepare(source, root / "out", "sample", digest, "natural")
            serving = sqlite3.connect(root / "out/sample.sqlite")
            try:
                count, distinct_ids, missing_ids = serving.execute(
                    "SELECT COUNT(*), COUNT(DISTINCT id), SUM(id IS NULL) FROM many_rows"
                ).fetchone()
                self.assertEqual((2_105, 2_105, 0), (count, distinct_ids, missing_ids))
            finally:
                serving.close()

    def test_decimal_text_requires_a_valid_declared_precision_and_scale(self) -> None:
        for declared in ("DECIMAL_TEXT", "DECIMAL_TEXT(0,0)", "DECIMAL_TEXT(8,9)"):
            with self.subTest(declared=declared), self.assertRaisesRegex(ValueError, "DECIMAL_TEXT"):
                prepare_fixture.field_schema(declared)
    def test_separate_identity_preserves_native_names_values_and_schema_objects(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "native.sqlite"
            native = sqlite3.connect(source)
            native.executescript("""
                CREATE TABLE organizations (id TEXT PRIMARY KEY, __OVDB_RECORD_ID TEXT, __ovdb_record_id_1 TEXT, payload BLOB, nullable TEXT, number REAL);
                CREATE INDEX ovdb_organizations_id ON organizations(nullable);
                CREATE TABLE ovdb_organizations_id_1 (code INTEGER PRIMARY KEY);
                CREATE TABLE capitals (ID INTEGER PRIMARY KEY, parent TEXT REFERENCES organizations(id));
                CREATE INDEX ovdb_native_index ON capitals(parent);
                CREATE TABLE composite (country TEXT, place INTEGER, PRIMARY KEY(country, place)) WITHOUT ROWID;
                CREATE TABLE generated_native (ID INTEGER PRIMARY KEY, "__OVDB_RECORD_ID" TEXT GENERATED ALWAYS AS ('source-' || ID) VIRTUAL);
                INSERT INTO generated_native(ID) VALUES (4);
                CREATE TABLE empty_native (ID TEXT PRIMARY KEY);
                CREATE TABLE keyless (ID TEXT, payload BLOB);
                CREATE VIEW native_view AS SELECT id, nullable, payload FROM organizations;
                INSERT INTO organizations VALUES ('https://ror.org/a% b', 'native', 'native-1', X'00FF10', NULL, 1.25), ('second', '', '', X'', '', 2);
                INSERT INTO capitals VALUES (1, 'second');
                INSERT INTO composite VALUES ('GB', 2), ('IE', 3);
                INSERT INTO keyless VALUES ('duplicate', NULL), ('duplicate', X'00');
            """)
            native.commit()
            tables = [row[0] for row in native.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")]
            columns = {name: list(native.execute(f"PRAGMA table_info({prepare_fixture.quote_identifier(name)})")) for name in tables}
            metadata = {name: prepare_fixture.table_metadata(native, name) for name in tables}
            digests = {name: prepare_fixture.native_value_digest(native, name, columns[name]) for name in tables}
            native.close()
            source_hash = prepare_fixture.sha256_file(source)
            manifest = prepare_fixture.prepare(source, root / "out", "fixture", source_hash, "natural", serving_adapter="separate-id/1")
            self.assertEqual(source_hash, prepare_fixture.sha256_file(source))
            text = manifest.read_text()
            self.assertIn('"organizations": "__ovdb_record_id_2"', text)
            self.assertIn('busy_timeout: 0s', text)
            self.assertIn('"id": {type: string}', text)
            self.assertIn('"ID": {type: integer}', text)
            self.assertIn('"generated_native": "__ovdb_record_id_1"', text)
            self.assertIn('"__OVDB_RECORD_ID": {type: string}', text)
            serving = sqlite3.connect(root / "out/fixture.sqlite")
            try:
                for name in tables:
                    helper = "__ovdb_record_id_2" if name == "organizations" else "__ovdb_record_id_1" if name == "generated_native" else "__ovdb_record_id"
                    index = "ovdb_organizations_id_2" if name == "organizations" else "ovdb_" + name + "_id"
                    self.assertEqual(metadata[name], prepare_fixture.table_metadata(serving, name, (helper, index)))
                    self.assertEqual(digests[name], prepare_fixture.native_value_digest(serving, name, columns[name]))
                self.assertEqual(('https://ror.org/a% b', 'native', 'native-1', b'\x00\xff\x10', None), serving.execute("SELECT id, __OVDB_RECORD_ID, __ovdb_record_id_1, payload, nullable FROM organizations WHERE id LIKE 'https:%'").fetchone())
                self.assertEqual(2, serving.execute("SELECT COUNT(DISTINCT __ovdb_record_id) FROM keyless").fetchone()[0])
                self.assertEqual([], serving.execute("PRAGMA foreign_key_check").fetchall())
                self.assertEqual(2, len(serving.execute("SELECT * FROM native_view").fetchall()))
            finally:
                serving.close()
            again = prepare_fixture.prepare(source, root / "again", "fixture", source_hash, "natural", serving_adapter="separate-id/1")
            self.assertEqual(manifest.read_bytes(), again.read_bytes())
            self.assertEqual(prepare_fixture.sha256_file(root / "out/fixture.sqlite"), prepare_fixture.sha256_file(root / "again/fixture.sqlite"))

    def test_native_digest_detects_typed_changes_and_adapter_corruption(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.sqlite"
            native = sqlite3.connect(source)
            native.executescript("CREATE TABLE sample (code TEXT PRIMARY KEY, value); INSERT INTO sample VALUES ('a', NULL), ('b', X'00');")
            native.commit()
            columns = list(native.execute("PRAGMA table_info(sample)"))
            before = prepare_fixture.native_value_digest(native, "sample", columns)
            native.execute("UPDATE sample SET value='' WHERE code='a'")
            self.assertNotEqual(before, prepare_fixture.native_value_digest(native, "sample", columns))
            native.rollback()
            native.close()
            add_ids = prepare_fixture.add_record_ids
            def corrupt(connection, table, columns, key_format, serving_adapter):
                generated = add_ids(connection, table, columns, key_format, serving_adapter)
                connection.execute("UPDATE sample SET value='' WHERE code='a'")
                return generated
            with patch.object(prepare_fixture, "add_record_ids", corrupt), self.assertRaisesRegex(ValueError, "native row values"):
                prepare_fixture.prepare(source, root / "out", "fixture", prepare_fixture.sha256_file(source), "natural", serving_adapter="separate-id/1")

    def test_legacy_manifest_bytes_and_composite_formats_remain_unchanged(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.sqlite"
            native = sqlite3.connect(source)
            native.executescript("CREATE TABLE sample (code TEXT, part INTEGER, PRIMARY KEY(code, part)); INSERT INTO sample VALUES ('a,b', 2);")
            native.close()
            manifest = prepare_fixture.prepare(source, root / "out", "fixture", prepare_fixture.sha256_file(source), "legacy")
            self.assertEqual(b'database: {id: fixture, schema_mode: strict, cache_ttl: 24h}\nstorage: {engine: sqlite, path: ./fixture.sqlite}\nschemas:\n  collections:\n    "sample":\n      fields:\n        "code": {type: string}\n        "part": {type: integer}\n        "id": {type: string}\n', manifest.read_bytes())
            serving = sqlite3.connect(root / "out/fixture.sqlite")
            self.assertEqual('a,b,2', serving.execute("SELECT id FROM sample").fetchone()[0])
            serving.close()
            self.assertEqual('ovdb:W3sidHlwZSI6InN0ciIsInZhbHVlIjoiYSxiIn0seyJ0eXBlIjoiaW50IiwidmFsdWUiOjJ9XQ', prepare_fixture.record_id(('a,b', 2), "natural"))

    def test_separate_identity_rejects_empty_keys_and_unknown_adapter(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.sqlite"
            native = sqlite3.connect(source)
            native.executescript("CREATE TABLE sample (id TEXT PRIMARY KEY); INSERT INTO sample VALUES ('');")
            native.close()
            with self.assertRaisesRegex(ValueError, "empty serving ID"):
                prepare_fixture.prepare(source, root / "out", "fixture", prepare_fixture.sha256_file(source), "natural", serving_adapter="separate-id/1")
            with self.assertRaisesRegex(ValueError, "unknown serving adapter"):
                prepare_fixture.prepare(source, root / "out", "fixture", prepare_fixture.sha256_file(source), "natural", serving_adapter="next")

    def test_separate_identity_refuses_colliding_legacy_composite_keys(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "source.sqlite"
            native = sqlite3.connect(source)
            native.executescript("CREATE TABLE sample (a TEXT, b TEXT, PRIMARY KEY(a, b)); INSERT INTO sample VALUES ('a,b', 'c'), ('a', 'b,c');")
            native.close()
            digest = prepare_fixture.sha256_file(source)
            with self.assertRaises(sqlite3.IntegrityError):
                prepare_fixture.prepare(source, root / "out", "fixture", digest, "legacy", serving_adapter="separate-id/1")
            self.assertFalse((root / "out/fixture.sqlite").exists())
            self.assertEqual(digest, prepare_fixture.sha256_file(source))
            prepare_fixture.prepare(source, root / "natural", "fixture", digest, "natural", serving_adapter="separate-id/1")

    def test_rejects_foreign_key_violations_and_invalid_ids(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "invalid.sqlite"
            connection = sqlite3.connect(source)
            connection.executescript(
                "PRAGMA foreign_keys=OFF; CREATE TABLE parent(id INTEGER PRIMARY KEY); "
                "CREATE TABLE child(parent_id INTEGER REFERENCES parent(id)); INSERT INTO child VALUES(5);"
            )
            connection.close()
            digest = hashlib.sha256(source.read_bytes()).hexdigest()
            with self.assertRaisesRegex(ValueError, "foreign-key violations"):
                prepare_fixture.prepare(source, root / "out", "valid-id", digest, "natural")
            connection = sqlite3.connect(source)
            connection.execute("DELETE FROM child")
            connection.commit()
            connection.close()
            clean_digest = hashlib.sha256(source.read_bytes()).hexdigest()
            for invalid in ("Bad-ID", "_bad", "1bad", "has.dot"):
                with self.subTest(database_id=invalid), self.assertRaisesRegex(ValueError, "invalid database id"):
                    prepare_fixture.prepare(source, root / "out", invalid, clean_digest, "natural")


if __name__ == "__main__":
    unittest.main()
