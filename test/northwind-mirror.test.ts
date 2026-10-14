import { env } from "cloudflare:workers";
import { beforeAll, describe, expect, it } from "vitest";
import type { D1Binding } from "@dalgo/d1";

import {
  NORTHWIND_ALLOWED_ORIGINS,
  type NorthwindEnv,
} from "../src/northwind";
import {
  NORTHWIND_EXPECTED_TABLE_ROWS,
  NORTHWIND_EXPECTED_VIEW_ROWS,
  NORTHWIND_D1_IMPORT_SHA256,
  NORTHWIND_COLUMN_TYPES,
  NORTHWIND_SCHEMA_VERSION,
  NORTHWIND_SEED_VERSION,
  NORTHWIND_TABLES,
} from "../src/northwind-schema";
import { createWorker } from "../src/worker";

declare const __NORTHWIND_D1_SEED_SQL__: string;

interface TestD1PreparedStatement extends ReturnType<D1Binding["prepare"]> {
  run(): Promise<unknown>;
}

interface TestD1Binding extends D1Binding {
  exec(sql: string): Promise<unknown>;
  prepare(sql: string): TestD1PreparedStatement;
}

const baseURL = "https://cloud.openvaultdb.com";
const hasPinnedSeed = __NORTHWIND_D1_SEED_SQL__.length > 0;
const neonTestURL = (env as typeof env & { NORTHWIND_NEON_TEST_URL?: string }).NORTHWIND_NEON_TEST_URL ?? "";
const hasNeonTestURL = (env as typeof env & { NORTHWIND_NEON_TEST_ENABLED?: string }).NORTHWIND_NEON_TEST_ENABLED === "true" && neonTestURL.length > 0;
const worker = createWorker();
const fetchWorker = worker.fetch as unknown as (
  request: Request,
  environment: Env,
  context: ExecutionContext,
) => Response | Promise<Response>;

function database(): TestD1Binding {
  const binding = (env as typeof env & { NORTHWIND_D1?: TestD1Binding }).NORTHWIND_D1;
  if (!binding) throw new Error("Miniflare Northwind D1 test binding is missing.");
  return binding;
}

function splitSql(source: string): string[] {
  const statements: string[] = [];
  const addStatement = (statement: string): void => {
    const clean = statement.replace(/^(?:\s*--[^\n]*(?:\n|$))+\s*/u, "").trim();
    if (clean) statements.push(clean);
  };
  let start = 0;
  let quote: "single" | "double" | "backtick" | "bracket" | undefined;
  let lineComment = false;
  let blockComment = false;
  for (let index = 0; index < source.length; index += 1) {
    const current = source[index];
    const next = source[index + 1];
    if (lineComment) {
      if (current === "\n") lineComment = false;
      continue;
    }
    if (blockComment) {
      if (current === "*" && next === "/") {
        blockComment = false;
        index += 1;
      }
      continue;
    }
    if (quote === "single" || quote === "double" || quote === "backtick") {
      const end = quote === "single" ? "'" : quote === "double" ? '"' : "`";
      if (current === end && next === end) {
        index += 1;
      } else if (current === end) {
        quote = undefined;
      }
      continue;
    }
    if (quote === "bracket") {
      if (current === "]" && next === "]") index += 1;
      else if (current === "]") quote = undefined;
      continue;
    }
    if (current === "-" && next === "-") {
      lineComment = true;
      index += 1;
    } else if (current === "/" && next === "*") {
      blockComment = true;
      index += 1;
    } else if (current === "'") quote = "single";
    else if (current === '"') quote = "double";
    else if (current === "`") quote = "backtick";
    else if (current === "[") quote = "bracket";
    else if (current === ";") {
      addStatement(source.slice(start, index));
      start = index + 1;
    }
  }
  addStatement(source.slice(start));
  return statements;
}

async function call(pathname: string, body?: unknown, headers?: HeadersInit): Promise<Response> {
  return fetchWorker(
    new Request(`${baseURL}${pathname}`, {
      method: body === undefined ? "GET" : "POST",
      headers: body === undefined ? headers : { "Content-Type": "application/json", ...Object.fromEntries(new Headers(headers)) },
      body: body === undefined ? undefined : JSON.stringify(body),
    }),
    { ...env, NORTHWIND_D1: database() } as NorthwindEnv,
    {} as ExecutionContext,
  ) as Promise<Response>;
}

beforeAll(async () => {
  const db = database();
  const sql = hasPinnedSeed
    ? __NORTHWIND_D1_SEED_SQL__
    : "CREATE TABLE [Categories] ([CategoryID] INTEGER PRIMARY KEY, [CategoryName] TEXT, [Description] TEXT, [Picture] BLOB); INSERT INTO [Categories] VALUES (1, 'Beverages', NULL, X'0001ff'); INSERT INTO [Categories] VALUES (2, 'Condiments', 'Sauces', NULL); CREATE VIEW [ProductDetails_V] AS SELECT [CategoryID], [CategoryName], [Picture] FROM [Categories];";
  for (const statement of splitSql(sql)) await db.prepare(statement).run();
});

describe("Northwind D1 and Neon read-only mirrors", () => {
  it("returns pinned metadata and enforces public CORS and version markers", async () => {
    const response = await call("/northwind/d1/v1/metadata", undefined, { Origin: NORTHWIND_ALLOWED_ORIGINS[0] });
    expect(response.status).toBe(200);
    expect(response.headers.get("Access-Control-Allow-Origin")).toBe(NORTHWIND_ALLOWED_ORIGINS[0]);
    await expect(response.json()).resolves.toMatchObject({
      version: 1,
      schemaVersion: NORTHWIND_SCHEMA_VERSION,
      seedVersion: NORTHWIND_SEED_VERSION,
      collections: expect.arrayContaining([
        { name: "Order Details", columns: ["OrderID", "ProductID", "UnitPrice", "Quantity", "Discount"], primaryKey: ["OrderID", "ProductID"] },
        {
          name: "ProductDetails_V",
          columns: Object.keys(NORTHWIND_TABLES.ProductDetails_V.columns),
          primaryKey: [],
        },
      ]),
    });
    const mismatch = await fetchWorker(
      new Request(`${baseURL}/northwind/d1/v1/metadata`, { headers: { "X-Dalgo-Seed-Version": "wrong" } }),
      { ...env, NORTHWIND_D1: database() } as NorthwindEnv,
      {} as ExecutionContext,
    ) as Response;
    expect(mismatch.status).toBe(409);

    const options = await fetchWorker(
      new Request(`${baseURL}/northwind/neon/v1/query`, {
        method: "OPTIONS",
        headers: { Origin: NORTHWIND_ALLOWED_ORIGINS[1], "Access-Control-Request-Headers": "content-type,x-dalgo-schema-version,x-dalgo-seed-version" },
      }),
      env,
      {} as ExecutionContext,
    ) as Response;
    expect(options.status).toBe(204);
    expect(options.headers.get("Access-Control-Allow-Headers")).toContain("X-Dalgo-Schema-Version");
  });

  it("executes native D1 queries with NULL, BLOB, projection, and stable offset ordering", async () => {
    const categories = await call("/northwind/d1/v1/query", {
      version: 1,
      collection: "Categories",
      columns: ["CategoryName", "Description", "Picture"],
      orders: [{ field: "CategoryID", direction: "asc" }],
      limit: 1,
      offset: 0,
    });
    expect(categories.status).toBe(200);
    const result = await categories.json() as { columns: string[]; primaryKey: string[]; records: Record<string, unknown>[] };
    expect(result.columns).toEqual(["CategoryName", "Description", "Picture", "CategoryID"]);
    expect(result.primaryKey).toEqual(["CategoryID"]);
    expect(result.records[0]?.CategoryName).toBe("Beverages");
    if (hasPinnedSeed) {
      expect(result.records[0]?.Description).toBe("Soft drinks, coffees, teas, beers, and ales");
      const picture = result.records[0]?.Picture as { $type: string; base64: string };
      expect(picture.$type).toBe("blob");
      const bytes = Uint8Array.from(atob(picture.base64), (character) => character.charCodeAt(0));
      expect(bytes).toHaveLength(10151);
      expect(Array.from(bytes.slice(0, 4))).toEqual([255, 216, 255, 224]);
      const digest = await crypto.subtle.digest("SHA-256", bytes);
      expect(Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")).toBe("aa834ba5769075289e2a919ce350bd9547531fcf8d18e370eb49f2262a64dd30");
    } else {
      expect(result.records[0]).toMatchObject({ Description: null, Picture: { $type: "blob", base64: "AAH/" } });
    }

    const second = await call("/northwind/d1/v1/query", {
      version: 1,
      collection: "Categories",
      columns: ["CategoryID", "CategoryName"],
      orders: [{ field: "CategoryID", direction: "asc" }],
      limit: 1,
      offset: 1,
    });
    await expect(second.json()).resolves.toMatchObject({ records: [{ CategoryID: 2, CategoryName: "Condiments" }] });

    const nulls = hasPinnedSeed
      ? await call("/northwind/d1/v1/query", {
        version: 1,
        collection: "Customers",
        columns: ["CompanyName", "Region"],
        filters: [{ field: "Region", operator: "==", value: null }],
        limit: 10,
      })
      : await call("/northwind/d1/v1/query", {
        version: 1,
        collection: "Categories",
        columns: ["CategoryName"],
        filters: [{ field: "Description", operator: "==", value: null }],
        limit: 10,
      });
    const nullBody = await nulls.json() as { records: Record<string, unknown>[] };
    expect(nulls.status).toBe(200);
    if (hasPinnedSeed) {
      expect(nullBody.records.length).toBeGreaterThan(0);
      expect(nullBody.records.every((row) => row.Region === null)).toBe(true);
    } else {
      expect(nullBody.records).toMatchObject([{ CategoryName: "Beverages", Description: null }]);
    }
  });

  it("rejects unsafe queries and reports an unconfigured Neon endpoint without leaking configuration", async () => {
    const injection = await call("/northwind/d1/v1/query", {
      version: 1,
      collection: "Categories; DROP TABLE Categories",
    });
    expect(injection.status).toBe(422);
    expect(await injection.text()).not.toContain("DROP TABLE");
    const unknownField = await call("/northwind/d1/v1/query", {
      version: 1,
      collection: "Categories",
      filters: [{ field: "secret", operator: "==", value: "x" }],
    });
    expect(unknownField.status).toBe(400);

    const wrongType = await call("/northwind/d1/v1/query", {
      version: 1,
      collection: "Orders",
      filters: [{ field: "OrderID", operator: "==", value: "abc" }],
    });
    expect(wrongType.status).toBe(400);
    const deepOffset = await call("/northwind/d1/v1/query", { version: 1, collection: "Categories", offset: 10_001 });
    expect(deepOffset.status).toBe(400);
    const okResponse = await call("/northwind/d1/v1/query", { version: 1, collection: "Categories", limit: 1 });
    expect(okResponse.headers.get("Cache-Control")).toBe("no-store");
    expect(okResponse.headers.get("X-Content-Type-Options")).toBe("nosniff");

    const missing = await fetchWorker(
      new Request(`${baseURL}/northwind/neon/v1/metadata`),
      env,
      {} as ExecutionContext,
    ) as Response;
    expect(missing.status).toBe(503);
    expect(await missing.text()).not.toContain("DATABASE_URL");
  });

  it("compiles Neon DTO filters as bound values and returns the same BLOB wire shape", async () => {
    const statements: { text: string; values: readonly (string | number | Uint8Array | null)[] }[] = [];
    const neonWorker = createWorker(fetch, (connectionString) => {
      expect(connectionString).toContain("northwind_reader");
      return async (text, values) => {
        statements.push({ text, values });
        if (text.includes('"Products"')) return [{ ProductID: 7, ProductName: "Test product" }];
        if (text.includes('"Order Subtotals"')) return [];
        return [{ CategoryID: 1, CategoryName: "Beverages", Description: null, Picture: Uint8Array.from([0, 1, 255]) }];
      };
    });
    const callNeon = async (body: unknown): Promise<Response> => {
      const fetch = neonWorker.fetch as unknown as typeof fetchWorker;
      return fetch(new Request(`${baseURL}/northwind/neon/v1/query`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(body),
      }), {
        ...env,
        NORTHWIND_NEON_DATABASE_URL: "postgresql://northwind_reader:test@ep-local.us-west-2.aws.neon.tech/northwind",
      } as NorthwindEnv, {} as ExecutionContext) as Promise<Response>;
    };
    const injection = "x' OR 1=1 --";
    const product = await callNeon({
      version: 1,
      collection: "Products",
      columns: ["ProductName"],
      filters: [
        { field: "Discontinued", operator: "==", value: 0 },
        { field: "ProductName", operator: "==", value: injection },
      ],
      orders: [{ field: "ProductName", direction: "asc" }],
      limit: 1,
      offset: 4,
    });
    expect(product.status).toBe(200);
    expect(statements[0]?.text).toContain('"northwind"."Products"');
    expect(statements[0]?.text).toContain('"Discontinued" COLLATE "C" = $1');
    expect(statements[0]?.text).toContain('"ProductName" COLLATE "C" = $2');
    expect(statements[0]?.text).toContain("LIMIT 1 OFFSET 4");
    expect(statements[0]?.text).not.toContain(injection);
    expect(statements[0]?.values).toEqual(["0", injection]);

    expect(NORTHWIND_COLUMN_TYPES["Order Subtotals"]?.Subtotal).toBe("NUMERIC");
    const subtotal = await callNeon({
      version: 1,
      collection: "Order Subtotals",
      columns: ["Subtotal"],
      filters: [{ field: "Subtotal", operator: ">", value: 100 }],
      limit: 1,
    });
    expect(subtotal.status).toBe(200);
    expect(statements[1]?.values).toEqual([100]);

    const picture = await callNeon({ version: 1, collection: "Categories", limit: 1 });
    await expect(picture.json()).resolves.toMatchObject({
      records: [{ Picture: { $type: "blob", base64: "AAH/" } }],
    });
  });

  it.skipIf(!hasNeonTestURL)("verifies the pinned Neon HTTP transport against the read-only Northwind role", async () => {
    const liveEnv = { ...env, NORTHWIND_NEON_DATABASE_URL: neonTestURL } as NorthwindEnv;
    const request = async (body: unknown, headers: HeadersInit = {}): Promise<Response> => fetchWorker(
      new Request(`${baseURL}/northwind/neon/v1/query`, {
        method: "POST",
        headers: { "Content-Type": "application/json", ...Object.fromEntries(new Headers(headers)) },
        body: JSON.stringify(body),
      }),
      liveEnv,
      {} as ExecutionContext,
    ) as Promise<Response>;

    const metadata = await fetchWorker(
      new Request(`${baseURL}/northwind/neon/v1/metadata`),
      liveEnv,
      {} as ExecutionContext,
    ) as Response;
    const metadataBody = await metadata.json() as { collections: unknown[]; seedVersion: string };
    expect(metadata.status).toBe(200);
    expect(metadataBody.collections).toHaveLength(30);
    expect(metadataBody.seedVersion).toBe(NORTHWIND_SEED_VERSION);

    const composite = await request({
      version: 1,
      collection: "Order Details",
      filters: [{ field: "OrderID", operator: "==", value: 10248 }],
      orders: [{ field: "ProductID", direction: "asc" }],
      limit: 2,
      offset: 1,
    });
    const compositeBody = await composite.json() as { primaryKey: string[]; records: Record<string, unknown>[] };
    expect(composite.status).toBe(200);
    expect(compositeBody.primaryKey).toEqual(["OrderID", "ProductID"]);
    expect(compositeBody.records).toHaveLength(2);
    expect(compositeBody.records[0]).toHaveProperty("OrderID", "10248");
    expect(compositeBody.records[0]).toHaveProperty("ProductID", "42");
    expect(compositeBody.records[0]?.UnitPrice).toMatch(/^\d+(?:\.\d+)?$/u);
    expect(typeof compositeBody.records[0]?.UnitPrice).toBe("string");

    const nullable = await request({
      version: 1,
      collection: "Customers",
      columns: ["CompanyName", "Region"],
      filters: [{ field: "Region", operator: "==", value: null }],
      limit: 5,
    });
    const nullableBody = await nullable.json() as { records: Record<string, unknown>[] };
    expect(nullable.status).toBe(200);
    expect(nullableBody.records.length).toBeGreaterThan(0);
    expect(nullableBody.records.every((row) => row.Region === null)).toBe(true);

    const picture = await request({ version: 1, collection: "Categories", filters: [{ field: "CategoryID", operator: "==", value: 1 }], limit: 1 });
    await expect(picture.json()).resolves.toMatchObject({ records: [{ Picture: { $type: "blob", base64: expect.any(String) } }] });
    const view = await request({ version: 1, collection: "ProductDetails_V", limit: 2, offset: 1 });
    await expect(view.json()).resolves.toMatchObject({ primaryKey: [], records: expect.any(Array) });
    const mismatch = await request({ version: 1, collection: "Orders", limit: 1 }, { "X-Dalgo-Seed-Version": "old-seed" });
    expect(mismatch.status).toBe(409);
    const invalid = await request({ version: 1, collection: "Orders; DROP TABLE Orders" });
    expect(invalid.status).toBe(400);
  });

  it("serves the full pinned seed with every source table, empty table, view, and composite key", async () => {
    if (!hasPinnedSeed) return;
    expect(NORTHWIND_D1_IMPORT_SHA256).toBe("fea7d05421f0297adc18c41196f22b498c669abf78c2392b50343091df8810d7");
    const db = database();
    for (const [name, expectedRows] of Object.entries(NORTHWIND_EXPECTED_TABLE_ROWS)) {
      const quoted = `"${name.replaceAll('"', '""')}"`;
      const result = await db.prepare(`SELECT COUNT(*) AS row_count FROM ${quoted}`).all();
      expect(result.results?.[0]?.row_count, name).toBe(expectedRows);
    }
    for (const [name, expectedRows] of Object.entries(NORTHWIND_EXPECTED_VIEW_ROWS)) {
      const quoted = `"${name.replaceAll('"', '""')}"`;
      const result = await db.prepare(`SELECT COUNT(*) AS row_count FROM ${quoted}`).all();
      expect(result.results?.[0]?.row_count, name).toBe(expectedRows);
    }
    const foreignKeyErrors = await db.prepare("PRAGMA foreign_key_check").all();
    expect(foreignKeyErrors.results ?? []).toEqual([]);
    const details = await call("/northwind/d1/v1/query", {
      version: 1,
      collection: "Order Details",
      columns: ["UnitPrice", "Quantity", "Discount"],
      limit: 2,
      offset: 5,
    });
    const page = await details.json() as { columns: string[]; primaryKey: string[]; records: Record<string, unknown>[] };
    expect(page.primaryKey).toEqual(["OrderID", "ProductID"]);
    expect(page.columns).toEqual(["UnitPrice", "Quantity", "Discount", "OrderID", "ProductID"]);
    expect(page.records).toHaveLength(2);
    const view = await call("/northwind/d1/v1/query", { version: 1, collection: "ProductDetails_V", limit: 2 });
    await expect(view.json()).resolves.toMatchObject({ primaryKey: [], records: expect.any(Array) });
    const binary = await call("/northwind/d1/v1/query", {
      version: 1,
      collection: "Categories",
      columns: ["CategoryName", "Picture"],
      filters: [{ field: "CategoryID", operator: "==", value: 1 }],
      limit: 1,
    });
    const binaryBody = await binary.json() as { records: Record<string, unknown>[] };
    expect(binaryBody.records[0]?.CategoryName).toBe("Beverages");
    expect(binaryBody.records[0]?.Picture).toMatchObject({ $type: "blob", base64: expect.any(String) });
  });
  it("rate limits every Northwind request except preflight", async () => {
    const calls: string[] = [];
    const limited = { ...env, DEVICE_LOOKUP_LIMITER: { limit: async ({ key }: { key: string }) => { calls.push(key); return { success: false }; } } } as unknown as NorthwindEnv;
    for (const pathname of ["/northwind/d1/v1/metadata", "/northwind/neon/v1/query"]) {
      const response = await fetchWorker(new Request(`${baseURL}${pathname}`, { method: pathname.endsWith("query") ? "POST" : "GET" }), limited, {} as ExecutionContext);
      expect(response.status).toBe(429);
    }
    expect(calls).toHaveLength(2);
    const preflight = await fetchWorker(new Request(`${baseURL}/northwind/d1/v1/query`, { method: "OPTIONS", headers: { Origin: "https://demodb.dev" } }), limited, {} as ExecutionContext);
    expect(preflight.status).toBe(204);
    expect(calls).toHaveLength(2);
  });
});
