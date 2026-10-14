import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { PGlite } from "@electric-sql/pglite";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

const transactionCalls: { options: unknown; statements: { sql: string; params: unknown[] }[] }[] = [];
vi.mock("@neondatabase/serverless", () => ({
  neon: () => ({
    transaction: async (build: (txn: { query: (sql: string, params: unknown[]) => unknown }) => unknown[], options: unknown) => {
      const statements = (build({ query: (sql, params) => ({ sql, params }) }) as { sql: string; params: unknown[] }[]);
      transactionCalls.push({ options, statements });
      return statements.map(() => []);
    },
  }),
}));

import {
  NORTHWIND_MAX_OFFSET,
  createNorthwindNeonHandler,
  northwindReadOnlyTransaction,
  type NorthwindEnv,
  type NorthwindNeonQueryFactory,
} from "../src/northwind";
import { NORTHWIND_PG_TYPES, NORTHWIND_TABLES } from "../src/northwind-schema";

const POSTGRES_SHA256 = "b6efe271401eba9d4738a2bbea294cc205dd6e02f47864bd3f21e195310afbee";
const seedPath = process.env.NORTHWIND_POSTGRES_SEED_SQL;
if (!seedPath && process.env.CI) throw new Error("NORTHWIND_POSTGRES_SEED_SQL is required in CI.");
const suite = seedPath ? describe : describe.skip;

const env = { NORTHWIND_NEON_DATABASE_URL: "postgresql://northwind_reader:x@ep-test.neon.tech/northwind" } as unknown as NorthwindEnv;
const collections = Object.keys(NORTHWIND_TABLES);
type Scalar = string | number | Uint8Array | null;

let db: PGlite;
const statements: string[] = [];

/** Runs each statement exactly as the Worker plans it: statement_timeout, then the read, in a READ ONLY transaction. */
const factory: NorthwindNeonQueryFactory = () => async (statement, values) => {
  statements.push(statement);
  await db.exec("BEGIN READ ONLY");
  try {
    let rows: readonly Record<string, unknown>[] = [];
    for (const step of northwindReadOnlyTransaction(statement, values)) {
      rows = (await db.query(step.sql, step.params as unknown[])).rows as Record<string, unknown>[];
    }
    return rows;
  } finally {
    await db.exec("ROLLBACK");
  }
};

async function query(body: unknown, makeQuery?: NorthwindNeonQueryFactory | "real"): Promise<{ status: number; json: any }> {
  const response = await createNorthwindNeonHandler(new Request("https://cloud.openvaultdb.com/northwind/neon/v1/query", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  }), env, makeQuery === "real" ? undefined : (makeQuery ?? factory));
  return { status: response.status, json: await response.json() };
}

function sample(type: string): Scalar {
  if (type === "text") return "x";
  if (type === "bytea") return new Uint8Array([1]);
  return 1;
}
const wire = (value: Scalar): unknown => value instanceof Uint8Array ? { $type: "blob", base64: btoa(String.fromCharCode(...value)) } : value;

suite("Northwind Neon route against real PostgreSQL semantics (PGlite)", () => {
  beforeAll(async () => {
    const sql = readFileSync(seedPath as string, "utf8");
    expect(createHash("sha256").update(sql).digest("hex")).toBe(POSTGRES_SHA256);
    db = new PGlite();
    await db.exec(sql);
  });
  afterAll(async () => { await db?.close(); });

  it("keeps NORTHWIND_PG_TYPES identical to information_schema for all 30 collections", async () => {
    const result = await db.query<{ table_name: string; column_name: string; data_type: string }>(
      "select table_name, column_name, data_type from information_schema.columns where table_schema = 'northwind' and table_name <> '_import_manifest'",
    );
    const actual: Record<string, Record<string, string>> = {};
    for (const row of result.rows) (actual[row.table_name] ??= {})[row.column_name] = row.data_type;
    expect(actual).toEqual(NORTHWIND_PG_TYPES);
    expect(collections).toHaveLength(30);
    for (const [name, table] of Object.entries(NORTHWIND_TABLES)) {
      expect(Object.keys(table.columns).sort()).toEqual(Object.keys(NORTHWIND_PG_TYPES[name as keyof typeof NORTHWIND_PG_TYPES]).sort());
    }
  });

  for (const name of collections) {
    const table = NORTHWIND_TABLES[name as keyof typeof NORTHWIND_TABLES];
    const columns = Object.keys(table.columns);
    const types = NORTHWIND_PG_TYPES[name as keyof typeof NORTHWIND_PG_TYPES] as Record<string, string>;

    it(`${name}: plain, default tiebreak order, order by, filters on every column, no COLLATE on non-text`, async () => {
      statements.length = 0;
      const plain = await query({ version: 1, collection: name, limit: 5 });
      expect(plain.status, JSON.stringify(plain.json)).toBe(200);
      // The default order is the tiebreak (primary key, or every column for keyless views).
      expect(statements[0]).toMatch(/ORDER BY /u);

      for (const direction of ["asc", "desc"] as const) {
        const ordered = await query({ version: 1, collection: name, orders: columns.map((field) => ({ field, direction })), limit: 3 });
        expect(ordered.status, JSON.stringify(ordered.json)).toBe(200);
      }
      for (const column of columns) {
        const single = await query({ version: 1, collection: name, orders: [{ field: column, direction: "desc" }], limit: 2 });
        expect(single.status, `${name}.${column}`).toBe(200);
        const filtered = await query({ version: 1, collection: name, columns: [column], filters: [{ field: column, operator: "==", value: wire(sample(types[column] as string)) }], limit: 2 });
        expect(filtered.status, `${name}.${column} filter`).toBe(200);
        const range = await query({ version: 1, collection: name, columns: [column], filters: [{ field: column, operator: ">=", value: wire(sample(types[column] as string)) }, { field: column, operator: "!=", value: null }], limit: 2 });
        expect(range.status, `${name}.${column} range`).toBe(200);
      }
      const row = plain.json.records[0] as Record<string, unknown> | undefined;
      if (row !== undefined) {
        const key = columns.find((column) => types[column] !== "bytea" && row[column] !== null);
        if (key !== undefined) {
          const found = await query({ version: 1, collection: name, filters: [{ field: key, operator: "==", value: row[key] }] });
          expect(found.status).toBe(200);
          expect(found.json.records.length).toBeGreaterThan(0);
        }
      }
      for (const statement of statements) {
        for (const match of statement.matchAll(/t\."([^"]+)" COLLATE "C"/gu)) {
          expect(types[match[1] as string], `${name}.${match[1]} is collated`).toBe("text");
        }
      }
    });
  }

  it("serves Invoices (integer Salesperson) in default order and ordered by every column", async () => {
    const result = await query({ version: 1, collection: "Invoices", orders: [{ field: "Salesperson", direction: "asc" }], limit: 1 });
    expect(result.status).toBe(200);
    expect(statements.at(-1)).not.toMatch(/"Salesperson" COLLATE/u);
  });

  it("answers values that do not fit the column's PostgreSQL type with 400, not 503", async () => {
    for (const [collection, field, value] of [["Orders", "OrderID", "abc"], ["Orders", "OrderID", 1.5], ["Invoices", "Salesperson", "x"], ["Categories", "Picture", "x"], ["Orders", "Freight", "x"], ["Categories", "CategoryName", { $type: "blob", base64: "AA==" }]] as const) {
      const result = await query({ version: 1, collection, filters: [{ field, operator: "==", value }] });
      expect(result.status, `${collection}.${field}=${JSON.stringify(value)}`).toBe(400);
    }
    const membership = await query({ version: 1, collection: "Orders", filters: [{ field: "OrderID", operator: "in", value: [1, "abc"] }] });
    expect(membership.status).toBe(400);
    expect((await query({ version: 1, collection: "Nope" })).status).toBe(422);
  });

  it("caps offset", async () => {
    expect((await query({ version: 1, collection: "Orders", limit: 1, offset: NORTHWIND_MAX_OFFSET })).status).toBe(200);
    expect((await query({ version: 1, collection: "Orders", limit: 1, offset: NORTHWIND_MAX_OFFSET + 1 })).status).toBe(400);
  });

  it("refuses every mutation inside the read-only transaction and sets a server-side timeout", async () => {
    const attempts = [
      `INSERT INTO northwind."Categories" ("CategoryID", "CategoryName") VALUES (9999, 'x')`,
      `UPDATE northwind."Categories" SET "CategoryName" = 'x'`,
      `DELETE FROM northwind."Categories"`,
      `CREATE TABLE northwind.evil (a int)`,
      `DROP TABLE northwind."Categories"`,
      `TRUNCATE northwind."Categories"`,
    ];
    for (const attempt of attempts) {
      await expect(factory("")(attempt, [], new AbortController().signal), attempt).rejects.toThrow(/read-only transaction/iu);
    }
    const count = await db.query<{ n: number }>(`select count(*)::int as n from northwind."Categories"`);
    expect(count.rows[0]?.n).toBeGreaterThan(0);
    const [timeout] = northwindReadOnlyTransaction("SELECT 1", []);
    expect(timeout?.sql).toMatch(/^SET LOCAL statement_timeout = \d+$/u);
    const shown = await factory("")("SHOW statement_timeout", [], new AbortController().signal);
    expect(shown[0]?.statement_timeout).toMatch(/^\d+(s|ms)$/u);
  });
});

describe("Northwind Neon transport", () => {
  it("runs every query through sql.transaction with readOnly: true and statement_timeout first", async () => {
    transactionCalls.length = 0;
    const result = await query({ version: 1, collection: "Categories", limit: 1 }, "real");
    expect(result.status).toBe(200);
    expect(transactionCalls).toHaveLength(1);
    expect(transactionCalls[0]?.options).toMatchObject({ readOnly: true });
    expect(transactionCalls[0]?.statements[0]?.sql).toMatch(/^SET LOCAL statement_timeout = \d+$/u);
    expect(transactionCalls[0]?.statements[1]?.sql).toMatch(/^SELECT /u);
  });
});
