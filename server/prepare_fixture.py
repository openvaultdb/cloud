#!/usr/bin/env python3
"""Build an OVDB-readable SQLite copy from the pinned upstream Chinook file."""

import hashlib
import shutil
import sqlite3
import sys
from pathlib import Path

EXPECTED_SHA256 = "7651ba378ac2fcd0dfc3c66fb101f7a7eed3ba39a612ec642b96e20702061f15"


def field_type(sql_type: str) -> str:
    kind = sql_type.upper()
    if "INT" in kind:
        return "integer"
    if any(token in kind for token in ("REAL", "FLOA", "DOUB", "DECIMAL", "NUMERIC")):
        return "number"
    return "string"


def main(source: Path, output: Path) -> None:
    if hashlib.sha256(source.read_bytes()).hexdigest() != EXPECTED_SHA256:
        raise ValueError("Chinook source SHA-256 differs from the pinned fixture")
    output.mkdir(parents=True, exist_ok=True)
    database_path = output / "chinook.sqlite"
    shutil.copyfile(source, database_path)
    connection = sqlite3.connect(database_path)
    manifest = [
        "database: {id: chinook, schema_mode: strict}",
        "storage: {engine: sqlite, path: ./chinook.sqlite}",
        "schemas:",
        "  collections:",
    ]
    try:
        tables = [row[0] for row in connection.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"
        )]
        if len(tables) != 11:
            raise ValueError(f"expected 11 Chinook tables, got {len(tables)}")
        for table in tables:
            if not table.isidentifier():
                raise ValueError(f"unsafe table name: {table}")
            columns = list(connection.execute(f'PRAGMA table_info("{table}")'))
            primary_keys = [name for _, name, _, _, _, pk in sorted(columns, key=lambda item: item[5]) if pk]
            if not primary_keys:
                raise ValueError(f"table {table} has no primary key")
            row_count = connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0]
            # DALgo's SQLite record adapter uses an `id` column. Keep every
            # original Chinook column and derive its stable record ID from the PK.
            connection.execute(f'ALTER TABLE "{table}" ADD COLUMN id TEXT')
            key_expression = " || ',' || ".join(f'CAST("{key}" AS TEXT)' for key in primary_keys)
            connection.execute(f'UPDATE "{table}" SET id = {key_expression}')
            connection.execute(f'CREATE UNIQUE INDEX "ovdb_{table}_id" ON "{table}" (id)')
            if connection.execute(f'SELECT COUNT(*) FROM "{table}"').fetchone()[0] != row_count:
                raise ValueError(f"row count changed for {table}")
            manifest.extend((f"    {table}:", "      fields:"))
            for _, name, sql_type, _, _, _ in columns:
                if not name.isidentifier():
                    raise ValueError(f"unsafe field name: {name}")
                manifest.append(f"        {name}: {{type: {field_type(sql_type)}}}")
        connection.commit()
        if connection.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
            raise ValueError("derived SQLite file failed integrity check")
    finally:
        connection.close()
    (output / "chinook.yaml").write_text("\n".join(manifest) + "\n")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        raise SystemExit("usage: prepare_fixture.py SOURCE OUTPUT_DIR")
    main(Path(sys.argv[1]), Path(sys.argv[2]))
