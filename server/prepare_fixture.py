#!/usr/bin/env python3
"""Prepare a derived SQLite copy for the read-only OVDB record adapter.

The provider file is checked against its pinned SHA-256 and copied before the
adapter key column is added. Native columns, primary keys, indexes, views, and
foreign-key declarations remain in the derived copy.
"""

import base64
import hashlib
import json
import re
import shutil
import sqlite3
import sys
from pathlib import Path
from typing import Any


DEFAULT_CHINOOK_SHA256 = "7651ba378ac2fcd0dfc3c66fb101f7a7eed3ba39a612ec642b96e20702061f15"
DATABASE_ID_PATTERN = re.compile(r"^[a-z][a-z0-9-]{0,62}$")
ID_COLUMN_NAME = "id"
UPDATE_BATCH_SIZE = 1000


def field_type(sql_type: str) -> str:
    kind = sql_type.upper()
    if "INT" in kind:
        return "integer"
    if any(token in kind for token in ("REAL", "FLOA", "DOUB", "DECIMAL", "NUMERIC", "MONEY")):
        return "number"
    if "BLOB" in kind:
        return "any"
    return "string"


def quote_identifier(name: str) -> str:
    return '"' + name.replace('"', '""') + '"'


def manifest_identifier(name: str) -> str:
    # The driver emits collection/field identifiers as SQL fragments during
    # strict-schema setup. Quote only names that SQLite
    # cannot parse as bare identifiers; main.go restores the logical names
    # after mounting, while the SQLite driver still sees the native table.
    return name if name.isidentifier() else quote_identifier(name)


def record_id(values: tuple[Any, ...], key_format: str) -> str:
    if len(values) == 1:
        value = values[0]
        return base64.b64encode(value).decode("ascii") if isinstance(value, bytes) else str(value)
    if key_format == "legacy":
        return ",".join(base64.b64encode(value).decode("ascii") if isinstance(value, bytes) else str(value) for value in values)

    def encode(value: Any) -> Any:
        if isinstance(value, bytes):
            return {"blob": base64.b64encode(value).decode("ascii")}
        return {"type": type(value).__name__, "value": value}

    raw = json.dumps([encode(value) for value in values], sort_keys=True, separators=(",", ":")).encode()
    return "ovdb:" + base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def hidden_rowid_name(column_names: set[str]) -> str | None:
    lowered = {name.casefold() for name in column_names}
    return next((candidate for candidate in ("rowid", "_rowid_", "oid") if candidate.casefold() not in lowered), None)


def is_without_rowid(connection: sqlite3.Connection, table: str) -> bool:
    try:
        for row in connection.execute("PRAGMA table_list"):
            if row[0] == "main" and row[1] == table and row[2] == "table":
                return bool(row[4])
    except sqlite3.DatabaseError:
        pass
    table_statement = connection.execute(
        "SELECT sql FROM sqlite_master WHERE type='table' AND name=?", (table,)
    ).fetchone()[0]
    return bool(re.search(r"\bWITHOUT\s+ROWID\b", table_statement or "", re.IGNORECASE))


def add_record_ids(connection: sqlite3.Connection, table: str, columns: list[tuple[Any, ...]], key_format: str) -> None:
    names = {str(column[1]) for column in columns}
    if any(name.casefold() == ID_COLUMN_NAME for name in names):
        raise ValueError(f"table {table!r} already has an id column; the OVDB adapter reserves this name")
    primary_keys = [str(name) for _, name, _, _, _, order in sorted(columns, key=lambda item: item[5]) if order]
    table_sql = quote_identifier(table)
    without_rowid = is_without_rowid(connection, table)
    rowid_column = hidden_rowid_name(names)
    connection.execute(f"ALTER TABLE {table_sql} ADD COLUMN \"id\" TEXT")

    if primary_keys and without_rowid:
        projection = ", ".join(quote_identifier(name) for name in primary_keys)
        cursor = connection.execute(f"SELECT {projection} FROM {table_sql}")
        predicate = " AND ".join(f"{quote_identifier(name)} IS ?" for name in primary_keys)
    elif primary_keys and rowid_column:
        projection = ", ".join(quote_identifier(name) for name in primary_keys)
        cursor = connection.execute(f"SELECT {quote_identifier(rowid_column)}, {projection} FROM {table_sql}")
        predicate = f"{quote_identifier(rowid_column)}=?"
    else:
        if primary_keys:
            raise ValueError(f"table {table!r} shadows SQLite's hidden row identity; cannot safely prepare serving IDs")
        if not rowid_column:
            raise ValueError(f"keyless table {table!r} shadows all SQLite row identity aliases")
        cursor = connection.execute(f"SELECT {quote_identifier(rowid_column)} FROM {table_sql}")
        predicate = f"{quote_identifier(rowid_column)}=?"
    update_sql = f"UPDATE {table_sql} SET \"id\"=? WHERE {predicate}"
    while rows := cursor.fetchmany(UPDATE_BATCH_SIZE):
        updates = []
        for row in rows:
            if primary_keys and without_rowid:
                values = tuple(row)
                updates.append((record_id(values, key_format), *values))
            elif primary_keys:
                values = tuple(row[1:])
                updates.append((record_id(values, key_format), row[0]))
            else:
                values = (row[0],)
                updates.append((record_id(values, key_format), values[0]))
        connection.executemany(update_sql, updates)
    missing_ids = connection.execute(f"SELECT COUNT(*) FROM {table_sql} WHERE \"id\" IS NULL").fetchone()[0]
    if missing_ids:
        raise ValueError(f"failed to assign adapter IDs for {table!r}: {missing_ids} missing")
    connection.execute(f"CREATE UNIQUE INDEX {quote_identifier('ovdb_' + table + '_id')} ON {table_sql} (\"id\")")


def table_metadata(connection: sqlite3.Connection, table: str) -> tuple[Any, ...]:
    quoted = quote_identifier(table)
    columns = tuple(
        (name, sql_type, not_null, default_value, primary_key_order)
        for _, name, sql_type, not_null, default_value, primary_key_order
        in connection.execute(f"PRAGMA table_info({quoted})")
        if name.casefold() != ID_COLUMN_NAME
    )
    foreign_keys = tuple(connection.execute(f"PRAGMA foreign_key_list({quoted})"))
    indexes = []
    for _, name, unique, origin, partial in connection.execute(f"PRAGMA index_list({quoted})"):
        if str(name).startswith("ovdb_"):
            continue
        fields = tuple(row[2] for row in connection.execute(f"PRAGMA index_info({quote_identifier(name)})"))
        indexes.append((name, unique, origin, partial, fields))
    return columns, foreign_keys, tuple(sorted(indexes))


def prepare(source: Path, output: Path, database_id: str, expected_sha256: str, key_format: str) -> Path:
    actual_sha256 = hashlib.sha256(source.read_bytes()).hexdigest()
    if expected_sha256 and actual_sha256 != expected_sha256:
        raise ValueError(f"{database_id} source SHA-256 differs from its pin: got {actual_sha256}")
    if not DATABASE_ID_PATTERN.fullmatch(database_id):
        raise ValueError(f"invalid database id: {database_id!r}")
    if key_format not in ("natural", "legacy"):
        raise ValueError(f"unknown OVDB record key format: {key_format!r}")

    output.mkdir(parents=True, exist_ok=True)
    database_path = output / f"{database_id}.sqlite"
    shutil.copyfile(source, database_path)
    connection = sqlite3.connect(database_path)
    manifest = [
        f"database: {{id: {database_id}, schema_mode: strict, cache_ttl: 24h}}",
        f"storage: {{engine: sqlite, path: ./{database_id}.sqlite}}",
        "schemas:",
        "  collections:",
    ]
    try:
        if connection.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
            raise ValueError(f"{database_id} source SQLite file failed integrity check")
        violations = connection.execute("PRAGMA foreign_key_check").fetchall()
        if violations:
            raise ValueError(f"{database_id} source SQLite file has {len(violations)} foreign-key violations")
        tables = [row[0] for row in connection.execute(
            "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"
        )]
        if not tables:
            raise ValueError(f"{database_id} source contains no tables")
        native_metadata = {table: table_metadata(connection, table) for table in tables}
        native_views = tuple(connection.execute(
            "SELECT name, sql FROM sqlite_master WHERE type='view' ORDER BY name"
        ))
        for table in tables:
            columns = list(connection.execute(f"PRAGMA table_info({quote_identifier(table)})"))
            if not columns:
                raise ValueError(f"table {table!r} has no readable columns")
            original_count = connection.execute(f"SELECT COUNT(*) FROM {quote_identifier(table)}").fetchone()[0]
            add_record_ids(connection, table, columns, key_format)
            if connection.execute(f"SELECT COUNT(*) FROM {quote_identifier(table)}").fetchone()[0] != original_count:
                raise ValueError(f"row count changed for {table!r}")

            manifest.extend((f"    {json.dumps(manifest_identifier(table))}:", "      fields:"))
            for _, name, sql_type, _, _, _ in columns:
                manifest.append(f"        {json.dumps(manifest_identifier(name))}: {{type: {field_type(sql_type)}}}")
            manifest.append('        "id": {type: string}')

        for table in tables:
            if table_metadata(connection, table) != native_metadata[table]:
                raise ValueError(f"adapter preparation changed native schema metadata for {table!r}")
        if tuple(connection.execute(
            "SELECT name, sql FROM sqlite_master WHERE type='view' ORDER BY name"
        )) != native_views:
            raise ValueError("adapter preparation changed native view definitions")
        connection.commit()
        if connection.execute("PRAGMA integrity_check").fetchone()[0] != "ok":
            raise ValueError(f"{database_id} derived SQLite file failed integrity check")
        violations = connection.execute("PRAGMA foreign_key_check").fetchall()
        if violations:
            raise ValueError(f"{database_id} derived SQLite file has {len(violations)} foreign-key violations")
        if actual_sha256 != hashlib.sha256(source.read_bytes()).hexdigest():
            raise ValueError("provider source changed during preparation")
    finally:
        connection.close()
    manifest_path = output / f"{database_id}.yaml"
    manifest_path.write_text("\n".join(manifest) + "\n", encoding="utf-8")
    (output / f"{database_id}.source-sha256").write_text(actual_sha256 + "\n", encoding="ascii")
    return manifest_path


def main(argv: list[str]) -> None:
    if len(argv) not in (3, 4, 5, 6):
        raise SystemExit("usage: prepare_fixture.py SOURCE OUTPUT_DIR [DATABASE_ID [EXPECTED_SHA256 [KEY_FORMAT]]]")
    source = Path(argv[1])
    output = Path(argv[2])
    database_id = argv[3] if len(argv) >= 4 else "chinook"
    expected = argv[4] if len(argv) == 5 else (DEFAULT_CHINOOK_SHA256 if database_id == "chinook" else "")
    if len(argv) == 6:
        expected = argv[4]
    key_format = argv[5] if len(argv) == 6 else "natural"
    manifest = prepare(source, output, database_id, expected, key_format)
    print(manifest)


if __name__ == "__main__":
    main(sys.argv)
