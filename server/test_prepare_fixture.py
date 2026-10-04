import hashlib
import importlib.util
import sqlite3
import tempfile
import unittest
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
        INSERT INTO "Order Details" VALUES (1, 1, 3.25, X'00FF10'), (2, 1, 7.5, NULL);
        INSERT INTO discounts VALUES ('customer', '6380');
        INSERT INTO shadowed_rowid VALUES ('duplicate', 'first'), ('duplicate', 'second');
        CREATE VIEW "Order Detail View" AS SELECT "Order ID", "Unit Price" FROM "Order Details";
        '''
    )
    connection.commit()
    connection.close()


class PrepareFixtureTest(unittest.TestCase):
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
            self.assertIn("{type: number}", text)
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
