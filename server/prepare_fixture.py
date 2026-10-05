#!/usr/bin/env python3
"""Prepare a derived SQLite file for the read-only OVDB record adapter.

The provider file is checked against its pinned SHA-256 before the adapter key
column is added. Native columns, primary keys, indexes, views, and foreign-key
declarations remain in the derived file. Build staging files may be consumed to
avoid a second full-size copy; callers default to preserving their source.
"""

import base64
import hashlib
import json
import os
import re
import shutil
import sqlite3
import struct
import sys
from pathlib import Path
from typing import Any


DEFAULT_CHINOOK_SHA256 = "7651ba378ac2fcd0dfc3c66fb101f7a7eed3ba39a612ec642b96e20702061f15"
DATABASE_ID_PATTERN = re.compile(r"^[a-z][a-z0-9-]{0,62}$")
ID_COLUMN_NAME = "id"
UPDATE_BATCH_SIZE = 1000
HASH_CHUNK_SIZE = 1024 * 1024


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(HASH_CHUNK_SIZE):
            digest.update(chunk)
    return digest.hexdigest()


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
    # Manifests carry logical/native names. The SQLite adapter owns SQL
    # identifier quoting; storing quoted SQL fragments here makes those quote
    # characters part of the actual collection or field name.
    return name


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


def unused_name(base: str, occupied: set[str]) -> str:
    folded = {name.casefold() for name in occupied}
    candidate = base
    suffix = 0
    while candidate.casefold() in folded:
        suffix += 1
        candidate = f"{base}_{suffix}"
    return candidate


def native_value_digest(connection: sqlite3.Connection, table: str, columns: list[tuple[Any, ...]]) -> str:
    """Hash typed original rows in stable native identity order, with bounded memory."""
    names = [str(column[1]) for column in columns]
    primary_keys = [str(column[1]) for column in sorted(columns, key=lambda item: item[5]) if column[5]]
    rowid = hidden_rowid_name(set(names)) if not is_without_rowid(connection, table) else None
    ordering = [*primary_keys, *([rowid] if rowid else [])]
    if not ordering:
        raise ValueError(f"table {table!r} has no stable native row identity")
    projection = ", ".join(quote_identifier(name) for name in names)
    order = ", ".join(quote_identifier(name) for name in ordering)
    digest = hashlib.sha256()
    for row in connection.execute(f"SELECT {projection} FROM {quote_identifier(table)} ORDER BY {order}"):
        digest.update(b"R")
        for value in row:
            if value is None:
                tag, data = b"N", b""
            elif isinstance(value, bytes):
                tag, data = b"B", value
            elif isinstance(value, str):
                tag, data = b"S", value.encode("utf-8")
            elif isinstance(value, int):
                tag, data = b"I", str(value).encode("ascii")
            elif isinstance(value, float):
                tag, data = b"F", struct.pack(">d", value)
            else:
                raise ValueError(f"unsupported SQLite value type: {type(value).__name__}")
            digest.update(tag + struct.pack(">Q", len(data)) + data)
    return digest.hexdigest()


def add_record_ids(connection: sqlite3.Connection, table: str, columns: list[tuple[Any, ...]], key_format: str, serving_adapter: str | None = None) -> tuple[str, str]:
    names = {str(column[1]) for column in connection.execute(f"PRAGMA table_xinfo({quote_identifier(table)})")}
    if serving_adapter is None and any(name.casefold() == ID_COLUMN_NAME for name in names):
        raise ValueError(f"table {table!r} already has an id column; the OVDB adapter reserves this name")
    generated_column = unused_name("__ovdb_record_id", names) if serving_adapter else ID_COLUMN_NAME
    schema_names = {str(row[0]) for row in connection.execute("SELECT name FROM sqlite_master")}
    generated_index = unused_name("ovdb_" + table + "_id", schema_names) if serving_adapter else "ovdb_" + table + "_id"
    key_sql = quote_identifier(generated_column)
    primary_keys = [str(name) for _, name, _, _, _, order in sorted(columns, key=lambda item: item[5]) if order]
    table_sql = quote_identifier(table)
    without_rowid = is_without_rowid(connection, table)
    rowid_column = hidden_rowid_name(names)
    connection.execute(f"ALTER TABLE {table_sql} ADD COLUMN {key_sql} TEXT")

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
    update_sql = f"UPDATE {table_sql} SET {key_sql}=? WHERE {predicate}"
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
    missing_ids = connection.execute(f"SELECT COUNT(*) FROM {table_sql} WHERE {key_sql} IS NULL").fetchone()[0]
    if missing_ids:
        raise ValueError(f"failed to assign adapter IDs for {table!r}: {missing_ids} missing")
    if serving_adapter and connection.execute(f"SELECT COUNT(*) FROM {table_sql} WHERE {key_sql}=''").fetchone()[0]:
        raise ValueError(f"empty serving ID in {table!r}")
    connection.execute(f"CREATE UNIQUE INDEX {quote_identifier(generated_index)} ON {table_sql} ({key_sql})")
    return generated_column, generated_index


def table_metadata(connection: sqlite3.Connection, table: str, generated: tuple[str, str] | None = None) -> tuple[Any, ...]:
    quoted = quote_identifier(table)
    columns = tuple(
        (name, sql_type, not_null, default_value, primary_key_order)
        for _, name, sql_type, not_null, default_value, primary_key_order, _hidden
        in connection.execute(f"PRAGMA table_xinfo({quoted})")
        if generated is None or name != generated[0]
    )
    foreign_keys = tuple(connection.execute(f"PRAGMA foreign_key_list({quoted})"))
    indexes = []
    for _, name, unique, origin, partial in connection.execute(f"PRAGMA index_list({quoted})"):
        if generated is not None and name == generated[1]:
            continue
        fields = tuple(row for row in connection.execute(f"PRAGMA index_xinfo({quote_identifier(name)})") if generated is None or row[2] != generated[0])
        statement_row = connection.execute("SELECT sql FROM sqlite_master WHERE type='index' AND name=?", (name,)).fetchone()
        statement = statement_row[0] if statement_row else None
        indexes.append((name, unique, origin, partial, fields, statement))
    return columns, foreign_keys, tuple(sorted(indexes))


def prepare(
    source: Path,
    output: Path,
    database_id: str,
    expected_sha256: str,
    key_format: str,
    *,
    consume_source: bool = False,
    serving_adapter: str | None = None,
) -> Path:
    actual_sha256 = sha256_file(source)
    if expected_sha256 and actual_sha256 != expected_sha256:
        raise ValueError(f"{database_id} source SHA-256 differs from its pin: got {actual_sha256}")
    if not DATABASE_ID_PATTERN.fullmatch(database_id):
        raise ValueError(f"invalid database id: {database_id!r}")
    if key_format not in ("natural", "legacy"):
        raise ValueError(f"unknown OVDB record key format: {key_format!r}")

    if serving_adapter not in (None, "separate-id/1"):
        raise ValueError(f"unknown serving adapter: {serving_adapter!r}")

    output.mkdir(parents=True, exist_ok=True)
    database_path = output / f"{database_id}.sqlite"
    if consume_source:
        os.replace(source, database_path)
    else:
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
        column_pragma = "table_xinfo" if serving_adapter else "table_info"
        native_columns = {table: [tuple(column[:6]) for column in connection.execute(f"PRAGMA {column_pragma}({quote_identifier(table)})")] for table in tables}
        native_values = {table: native_value_digest(connection, table, native_columns[table]) for table in tables} if serving_adapter else {}
        generated_objects: dict[str, tuple[str, str]] = {}
        native_views = tuple(connection.execute(
            "SELECT name, sql FROM sqlite_master WHERE type='view' ORDER BY name"
        ))
        for table in tables:
            columns = native_columns[table]
            if not columns:
                raise ValueError(f"table {table!r} has no readable columns")
            original_count = connection.execute(f"SELECT COUNT(*) FROM {quote_identifier(table)}").fetchone()[0]
            generated_objects[table] = add_record_ids(connection, table, columns, key_format, serving_adapter)
            if connection.execute(f"SELECT COUNT(*) FROM {quote_identifier(table)}").fetchone()[0] != original_count:
                raise ValueError(f"row count changed for {table!r}")

            manifest.extend((f"    {json.dumps(manifest_identifier(table))}:", "      fields:"))
            for _, name, sql_type, _, _, _ in columns:
                manifest.append(f"        {json.dumps(manifest_identifier(name))}: {{type: {field_type(sql_type)}}}")
            manifest.append(f'        {json.dumps(generated_objects[table][0])}: {{type: string}}')

        for table in tables:
            if table_metadata(connection, table, generated_objects[table]) != native_metadata[table]:
                raise ValueError(f"adapter preparation changed native schema metadata for {table!r}")
            if serving_adapter and native_value_digest(connection, table, native_columns[table]) != native_values[table]:
                raise ValueError(f"adapter preparation changed native row values for {table!r}")
        if serving_adapter:
            record_keys = {table: generated_objects[table][0] for table in tables}
            manifest[1] = f"storage: {{engine: sqlite, path: ./{database_id}.sqlite, sqlite: {{record_keys: {json.dumps(record_keys)}, busy_timeout: 0s}}}}"
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
        if not consume_source and actual_sha256 != sha256_file(source):
            raise ValueError("provider source changed during preparation")
    except Exception:
        connection.close()
        database_path.unlink(missing_ok=True)
        raise
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
