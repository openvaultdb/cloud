import { neon } from "@neondatabase/serverless";
import {
  createD1ReadHandler,
  type D1Binding,
  type D1QueryRequest,
} from "@dalgo/d1";
import { jsonResponse } from "./http";
import {
  NORTHWIND_PG_TYPES,
  NORTHWIND_SCHEMA_VERSION,
  NORTHWIND_SEED_VERSION,
  NORTHWIND_TABLES,
} from "./northwind-schema";

export const NORTHWIND_ALLOWED_ORIGINS = [
  "https://northwind.demodb.dev",
  "https://demodb.dev",
  "https://www.demodb.dev",
] as const;

export type NorthwindEnv = Env & {
  NORTHWIND_D1?: D1Binding;
  NORTHWIND_NEON_DATABASE_URL?: string;
};

export type NorthwindNeonQuery = (
  statement: string,
  values: readonly (string | number | Uint8Array | null)[],
  signal: AbortSignal,
) => Promise<readonly Readonly<Record<string, unknown>>[]>;

export type NorthwindNeonQueryFactory = (connectionString: string) => NorthwindNeonQuery;

const allowedOrigins = new Set<string>(NORTHWIND_ALLOWED_ORIGINS);
const maxQueryLimit = 1_000;
const maxBoundValues = 100;
const maxMembershipValues = 100;
const maxRequestBytes = 1_048_576;
const maxResponseBytes = 4_194_304;
const neonQueryTimeoutMs = 15_000;
const neonStatementTimeoutMs = 10_000;
export const NORTHWIND_MAX_OFFSET = 10_000;
const requestKeys = new Set(["version", "collection", "columns", "filters", "orders", "limit", "offset"]);
const operators = new Set(["==", "!=", "<", "<=", ">", ">=", "in", "not-in"]);

/** An unknown collection: 422 on both routes, like the D1 handler. */
class UnsupportedCollectionError extends Error {}

interface QueryDto extends D1QueryRequest {
  readonly columns?: readonly string[];
}

interface ValidatedQuery {
  readonly dto: QueryDto;
  readonly tableName: string;
  readonly table: (typeof NORTHWIND_TABLES)[keyof typeof NORTHWIND_TABLES];
  readonly columns: readonly string[];
}

function corsHeaders(request: Request): Headers {
  const headers = new Headers({
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    Vary: "Origin",
  });
  const origin = request.headers.get("Origin");
  if (origin !== null && allowedOrigins.has(origin)) {
    headers.set("Access-Control-Allow-Origin", origin);
    headers.set("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
    headers.set("Access-Control-Allow-Headers", "Content-Type, X-Dalgo-Schema-Version, X-Dalgo-Seed-Version");
    headers.set("Access-Control-Max-Age", "600");
  }
  return headers;
}

function withCors(response: Response, headers: Headers): Response {
  const result = new Response(response.body, response);
  headers.forEach((value, name) => result.headers.set(name, value));
  return result;
}

function unavailable(request: Request, message: string): Response {
  return withCors(jsonResponse({ version: 1, error: { code: "temporarily_unavailable", message } }, 503), corsHeaders(request));
}

function dtoError(request: Request, code: string, status: number, message = "request could not be processed"): Response {
  return withCors(jsonResponse({ version: 1, error: { code, message } }, status), corsHeaders(request));
}

function hasVersionMismatch(request: Request): boolean {
  const schema = request.headers.get("X-Dalgo-Schema-Version");
  const seed = request.headers.get("X-Dalgo-Seed-Version");
  return (schema !== null && schema !== NORTHWIND_SCHEMA_VERSION) ||
    (seed !== null && seed !== NORTHWIND_SEED_VERSION);
}

function readOnlyNorthwindConnection(connectionString: string): boolean {
  try {
    const url = new URL(connectionString);
    const user = decodeURIComponent(url.username);
    const database = decodeURIComponent(url.pathname.replace(/^\//u, ""));
    return (url.protocol === "postgres:" || url.protocol === "postgresql:") &&
      url.hostname.endsWith(".neon.tech") && user === "northwind_reader" && database === "northwind";
  } catch (error) {
    return false;
  }
}

async function readRequestText(request: { readonly body: ReadableStream<Uint8Array> | null }, maxBytes: number): Promise<string> {
  if (request.body === null) return "";
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    for (;;) {
      const result = await reader.read();
      if (result.done) break;
      size += result.value.byteLength;
      if (size > maxBytes) {
        await reader.cancel();
        throw new RangeError("request is too large");
      }
      chunks.push(result.value);
    }
  } finally {
    reader.releaseLock();
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return new TextDecoder().decode(bytes);
}

function plainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value) &&
    (Object.getPrototypeOf(value) === Object.prototype || Object.getPrototypeOf(value) === null);
}

function safeInteger(value: unknown, minimum: number): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= minimum;
}

function decodeBlob(value: Record<string, unknown>): Uint8Array {
  if (Object.keys(value).length !== 2 || value.$type !== "blob" || typeof value.base64 !== "string") {
    throw new TypeError("Blob filters must use the versioned base64 encoding.");
  }
  let bytes: Uint8Array;
  try {
    const decoded = atob(value.base64);
    bytes = Uint8Array.from(decoded, (character) => character.charCodeAt(0));
  } catch (error) {
    throw new TypeError("Blob filter encoding is invalid.");
  }
  let canonical = "";
  for (const byte of bytes) canonical += String.fromCharCode(byte);
  if (btoa(canonical) !== value.base64) throw new TypeError("Blob filter encoding is invalid.");
  return bytes;
}

function jsonScalar(value: unknown, label: string): string | number | null | Uint8Array {
  if (value === null || typeof value === "string") return value;
  if (typeof value === "number" && Number.isFinite(value) && (!Number.isInteger(value) || Number.isSafeInteger(value))) return value;
  if (typeof value === "boolean") return value ? 1 : 0;
  if (plainObject(value)) return decodeBlob(value);
  throw new TypeError(`${label} must be a JSON scalar or an encoded blob.`);
}

function validateQuery(value: unknown): ValidatedQuery {
  if (!plainObject(value) || Object.keys(value).some((key) => !requestKeys.has(key))) throw new TypeError("Request contains unsupported fields.");
  if (value.version !== 1 || typeof value.collection !== "string") throw new TypeError("Request version or collection is invalid.");
  if (!Object.hasOwn(NORTHWIND_TABLES, value.collection)) throw new UnsupportedCollectionError("Collection is not allowlisted.");
  const table = NORTHWIND_TABLES[value.collection as keyof typeof NORTHWIND_TABLES];
  if (table === undefined) throw new UnsupportedCollectionError("Collection is not allowlisted.");
  const declared = new Set(Object.keys(table.columns));
  const columnsValue = value.columns;
  if (columnsValue !== undefined && (!Array.isArray(columnsValue) || columnsValue.length === 0 ||
    !columnsValue.every((item) => typeof item === "string" && declared.has(item)))) {
    throw new TypeError("Projection contains an unknown column.");
  }
  const filtersValue = value.filters ?? [];
  if (!Array.isArray(filtersValue)) throw new TypeError("Filters must be an array.");
  const filters = filtersValue.map((item) => {
    if (!plainObject(item) || Object.keys(item).some((key) => !["field", "operator", "value"].includes(key)) ||
      typeof item.field !== "string" || (!declared.has(item.field) && !(item.field === "__name__" && table.primaryKey.length === 1)) ||
      typeof item.operator !== "string" || !operators.has(item.operator) || !Object.hasOwn(item, "value")) {
      throw new TypeError("Filter is invalid.");
    }
    if (item.operator === "in" || item.operator === "not-in") {
      if (!Array.isArray(item.value) || item.value.length < 1 || item.value.length > maxMembershipValues) throw new TypeError("Membership filters require 1 to 100 values.");
      return { field: item.field, operator: item.operator, value: item.value.map((part) => jsonScalar(part, "Filter value")) };
    }
    return { field: item.field, operator: item.operator, value: jsonScalar(item.value, "Filter value") };
  });
  const ordersValue = value.orders ?? [];
  if (!Array.isArray(ordersValue)) throw new TypeError("Orders must be an array.");
  const orders = ordersValue.map((item) => {
    if (!plainObject(item) || Object.keys(item).some((key) => !["field", "direction"].includes(key)) ||
      typeof item.field !== "string" || (!declared.has(item.field) && !(item.field === "__name__" && table.primaryKey.length === 1)) ||
      (item.direction !== "asc" && item.direction !== "desc")) throw new TypeError("Order is invalid.");
    return { field: item.field, direction: item.direction };
  });
  const limit = value.limit === undefined ? maxQueryLimit : value.limit;
  const offset = value.offset === undefined ? 0 : value.offset;
  if (!safeInteger(limit, 1) || limit > maxQueryLimit) throw new TypeError(`Limit must be between 1 and ${String(maxQueryLimit)}.`);
  if (!safeInteger(offset, 0) || offset > NORTHWIND_MAX_OFFSET) throw new TypeError(`Offset must be between 0 and ${String(NORTHWIND_MAX_OFFSET)}.`);
  const columns = [...new Set([...(columnsValue as string[] | undefined ?? Object.keys(table.columns)), ...table.primaryKey])];
  return {
    dto: {
      version: 1,
      collection: value.collection,
      columns: columnsValue as string[] | undefined,
      filters: filters as QueryDto["filters"],
      orders: orders as QueryDto["orders"],
      limit,
      offset,
    },
    tableName: table.table,
    table,
    columns,
  };
}

function quoteIdentifier(identifier: string): string {
  return `"${identifier.replaceAll('"', '""')}"`;
}

type PgType = "text" | "integer" | "bigint" | "numeric" | "double precision" | "bytea";
const pgTypes: Readonly<Record<string, Readonly<Record<string, PgType>>>> = NORTHWIND_PG_TYPES;

/** The real PostgreSQL type of a column; unknown columns are refused rather than guessed. */
export function northwindPgType(table: string, column: string): PgType {
  const type = pgTypes[table]?.[column];
  if (type === undefined) throw new TypeError("Column has no PostgreSQL type.");
  return type;
}

/**
 * Checks a filter value against the column's PostgreSQL type and returns the value to bind.
 * A value that cannot be compared with the column is a client error (400), never a database error.
 * Text columns accept numbers (bound as their decimal text); other types accept only their own kind.
 */
export function checkNorthwindValue(value: string | number | null | Uint8Array, type: PgType): string | number | null | Uint8Array {
  if (value === null) return value;
  switch (type) {
    case "text":
      if (value instanceof Uint8Array) throw new TypeError("Filter value does not match the column type.");
      return String(value);
    case "integer":
    case "bigint":
      if (typeof value !== "number" || !Number.isSafeInteger(value)) throw new TypeError("Filter value does not match the column type.");
      if (type === "integer" && (value > 2_147_483_647 || value < -2_147_483_648)) throw new TypeError("Filter value does not match the column type.");
      return value;
    case "numeric":
    case "double precision":
      if (typeof value !== "number" || !Number.isFinite(value)) throw new TypeError("Filter value does not match the column type.");
      return value;
    case "bytea":
      if (!(value instanceof Uint8Array)) throw new TypeError("Filter value does not match the column type.");
      return value;
  }
}

/** Collation applies to text columns only: COLLATE on any other type is a PostgreSQL error. */
export function northwindCollation(table: string, column: string): string {
  return northwindPgType(table, column) === "text" ? ' COLLATE "C"' : "";
}

function checkQueryTypes(query: ValidatedQuery): void {
  const resolve = (field: string): string => field === "__name__" ? (query.table.primaryKey[0] ?? "") : field;
  for (const filter of query.dto.filters ?? []) {
    const type = northwindPgType(query.tableName, resolve(filter.field));
    const parts = filter.operator === "in" || filter.operator === "not-in" ? (filter.value as readonly unknown[]) : [filter.value];
    for (const part of parts) checkNorthwindValue(part as string | number | null | Uint8Array, type);
  }
}

function compilePostgresQuery(query: ValidatedQuery): { readonly statement: string; readonly values: readonly (string | number | Uint8Array | null)[] } {
  const { dto, table, tableName, columns } = query;
  const physicalColumns = table.columns as Readonly<Record<string, { readonly column: string }>>;
  const select = columns.map((column) => {
    const physical = physicalColumns[column]?.column;
    if (physical === undefined) throw new TypeError("Projection column is not allowlisted.");
    return `t.${quoteIdentifier(physical)} AS ${quoteIdentifier(column)}`;
  });
  const values: (string | number | Uint8Array | null)[] = [];
  const predicates: string[] = [];
  const bind = (field: string, value: string | number | null | Uint8Array): string => {
    const column = field === "__name__" ? table.primaryKey[0] : field;
    if (column === undefined) throw new TypeError("Document name filters require a single-column primary key.");
    values.push(checkNorthwindValue(value, northwindPgType(tableName, column)));
    if (values.length > maxBoundValues) throw new TypeError("Query has too many bound values.");
    return `$${String(values.length)}`;
  };
  for (const filter of dto.filters ?? []) {
    const field = filter.field === "__name__" ? table.primaryKey[0] : filter.field;
    if (field === undefined) throw new TypeError("Document name filters require a single-column primary key.");
    const column = physicalColumns[field]?.column;
    if (column === undefined) throw new TypeError("Filter column is not allowlisted.");
    const expression = `t.${quoteIdentifier(column)}${northwindCollation(tableName, field)}`;
    if ((filter.operator === "==" || filter.operator === "!=") && filter.value === null) {
      predicates.push(`${expression} IS ${filter.operator === "!=" ? "NOT " : ""}NULL`);
      continue;
    }
    if (filter.operator === "in" || filter.operator === "not-in") {
      const parts = filter.value as readonly (string | number | null | Uint8Array)[];
      const placeholders = parts.map((part) => bind(field, part));
      predicates.push(`${expression} ${filter.operator === "not-in" ? "NOT IN" : "IN"} (${placeholders.join(", ")})`);
      continue;
    }
    const operator = filter.operator === "==" ? "=" : filter.operator === "!=" ? "<>" : filter.operator;
    predicates.push(`${expression} ${operator} ${bind(field, filter.value as string | number | null | Uint8Array)}`);
  }
  const orders = [...(dto.orders ?? [])];
  const tieFields = table.primaryKey.length > 0 ? table.primaryKey : Object.keys(table.columns);
  for (const field of tieFields) if (!orders.some((order) => order.field === field)) orders.push({ field, direction: "asc" });
  const orderBy = orders.length === 0 ? "" : ` ORDER BY ${orders.map((order) => {
    const field = order.field === "__name__" ? table.primaryKey[0] : order.field;
    const column = field === undefined ? undefined : physicalColumns[field]?.column;
    if (column === undefined) throw new TypeError("Order column is not allowlisted.");
    const collation = field === undefined ? "" : northwindCollation(tableName, field);
    const nulls = order.direction === "desc" ? "NULLS LAST" : "NULLS FIRST";
    return `t.${quoteIdentifier(column)}${collation} ${order.direction === "desc" ? "DESC" : "ASC"} ${nulls}`;
  }).join(", ")}`;
  const where = predicates.length === 0 ? "" : ` WHERE ${predicates.join(" AND ")}`;
  const selected = select.length === 0 ? "*" : select.join(", ");
  const statement = `SELECT ${selected} FROM ${quoteIdentifier("northwind")}.${quoteIdentifier(tableName)} AS t${where}${orderBy} LIMIT ${String(dto.limit ?? maxQueryLimit)} OFFSET ${String(dto.offset ?? 0)}`;
  return { statement, values };
}

function rowValue(value: unknown, field: string, tableName: string): unknown {
  if (value === null) return null;
  const blobColumn = pgTypes[tableName]?.[field] === "bytea";
  if (blobColumn && value instanceof ArrayBuffer) {
    const bytes = new Uint8Array(value);
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return { $type: "blob", base64: btoa(binary) };
  }
  if (blobColumn && ArrayBuffer.isView(value)) {
    const bytes = new Uint8Array(value.buffer, value.byteOffset, value.byteLength);
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return { $type: "blob", base64: btoa(binary) };
  }
  if (blobColumn && typeof value === "string" && value.startsWith("\\x")) {
    const hex = value.slice(2);
    if (hex.length % 2 !== 0 || !/^[0-9a-f]*$/iu.test(hex)) throw new TypeError("PostgreSQL returned an invalid binary field.");
    const bytes = Uint8Array.from(hex.match(/.{2}/gu) ?? [], (pair) => Number.parseInt(pair, 16));
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return { $type: "blob", base64: btoa(binary) };
  }
  if (blobColumn) {
    // The serverless driver's HTTP response parser may leave bytea in PostgreSQL hex format.
    throw new TypeError("PostgreSQL returned an invalid binary field.");
  }
  if (typeof value === "string" || typeof value === "boolean" || typeof value === "number") {
    if (typeof value === "number" && (!Number.isFinite(value) || (Number.isInteger(value) && !Number.isSafeInteger(value)))) {
      throw new TypeError("PostgreSQL returned a number that cannot be represented safely as JSON.");
    }
    return value;
  }
  throw new TypeError("PostgreSQL returned an unsupported value.");
}

function safeRow(row: Readonly<Record<string, unknown>>, columns: readonly string[], tableName: string): Readonly<Record<string, unknown>> {
  const output: Record<string, unknown> = {};
  for (const column of columns) {
    if (!Object.hasOwn(row, column)) throw new TypeError("PostgreSQL omitted a projected column.");
    output[column] = rowValue(row[column], column, tableName);
  }
  return output;
}

/** The statements sent for one query: a server-side timeout, then the read, in one READ ONLY transaction. */
export function northwindReadOnlyTransaction(
  statement: string,
  values: readonly (string | number | Uint8Array | null)[],
): readonly { readonly sql: string; readonly params: readonly (string | number | Uint8Array | null)[] }[] {
  return [
    { sql: `SET LOCAL statement_timeout = ${String(neonStatementTimeoutMs)}`, params: [] },
    { sql: statement, params: values },
  ];
}

function createNeonQuery(connectionString: string): NorthwindNeonQuery {
  const sql = neon(connectionString);
  return async (statement, values, signal) => {
    const plan = northwindReadOnlyTransaction(statement, values);
    const results = await sql.transaction(
      (txn) => plan.map((step) => txn.query(step.sql, [...step.params])),
      { readOnly: true, fetchOptions: { signal } },
    );
    const rows = results[results.length - 1];
    if (!Array.isArray(rows)) throw new TypeError("Neon returned an invalid result.");
    return rows as readonly Readonly<Record<string, unknown>>[];
  };
}

const d1Handlers = new WeakMap<object, (request: Request) => Promise<Response>>();

function d1Handler(binding: D1Binding): (request: Request) => Promise<Response> {
  let handle = d1Handlers.get(binding);
  if (handle === undefined) {
    handle = createD1ReadHandler(binding, {
      tables: NORTHWIND_TABLES,
      path: "/northwind/d1/v1/query",
      metadataPath: "/northwind/d1/v1/metadata",
      schemaVersion: NORTHWIND_SCHEMA_VERSION,
      seedVersion: NORTHWIND_SEED_VERSION,
      allowedOrigins: NORTHWIND_ALLOWED_ORIGINS,
      maxQueryLimit,
      maxRequestBytes,
      maxResponseBytes,
    });
    d1Handlers.set(binding, handle);
  }
  return handle;
}

/**
 * Same request policy as the Neon route: a filter value that cannot be compared with its column,
 * an unknown collection and an excessive offset are all 400, never an empty page.
 */
async function d1PreCheck(request: Request): Promise<Response | undefined> {
  const url = new URL(request.url);
  if (request.method !== "POST" || url.pathname !== "/northwind/d1/v1/query" || hasVersionMismatch(request)) return undefined;
  if (!/^application\/json(?:\s*;|$)/iu.test(request.headers.get("Content-Type") ?? "")) return undefined;
  const length = request.headers.get("Content-Length");
  if (length !== null && (!/^\d+$/u.test(length) || Number(length) > maxRequestBytes)) return undefined;
  try {
    const body = await readRequestText(request.clone(), maxRequestBytes);
    checkQueryTypes(validateQuery(JSON.parse(body) as unknown));
  } catch (error) {
    if (error instanceof TypeError || error instanceof SyntaxError) return dtoError(request, "invalid_request", 400);
  }
  return undefined;
}

export async function createNorthwindD1Handler(request: Request, env: NorthwindEnv): Promise<Response> {
  if (!env.NORTHWIND_D1) return unavailable(request, "Northwind D1 is not configured.");
  const refused = await d1PreCheck(request);
  if (refused !== undefined) return refused;
  const response = await d1Handler(env.NORTHWIND_D1)(request);
  const result = new Response(response.body, response);
  result.headers.set("Cache-Control", "no-store");
  result.headers.set("X-Content-Type-Options", "nosniff");
  return result;
}

export async function createNorthwindNeonHandler(
  request: Request,
  env: NorthwindEnv,
  makeQuery: NorthwindNeonQueryFactory = createNeonQuery,
): Promise<Response> {
  const cors = corsHeaders(request);
  const url = new URL(request.url);
  const metadataPath = "/northwind/neon/v1/metadata";
  const queryPath = "/northwind/neon/v1/query";
  if (request.method === "OPTIONS") return new Response(null, { status: 204, headers: cors });
  if (url.pathname !== metadataPath && url.pathname !== queryPath) return dtoError(request, "not_found", 404);
  if (hasVersionMismatch(request)) return dtoError(request, "version_mismatch", 409, "deployment version mismatch");
  if (request.method === "GET" && url.pathname === metadataPath) {
    const connectionString = env.NORTHWIND_NEON_DATABASE_URL;
    if (!connectionString) return unavailable(request, "Northwind PostgreSQL is not configured.");
    if (!readOnlyNorthwindConnection(connectionString)) return unavailable(request, "Northwind PostgreSQL reader configuration is invalid.");
    return withCors(Response.json({
      version: 1,
      schemaVersion: NORTHWIND_SCHEMA_VERSION,
      seedVersion: NORTHWIND_SEED_VERSION,
      collections: Object.entries(NORTHWIND_TABLES).map(([name, table]) => ({
        name,
        columns: Object.keys(table.columns),
        primaryKey: table.primaryKey,
      })),
    }), cors);
  }
  if (request.method !== "POST" || url.pathname !== queryPath) {
    const response = dtoError(request, "method_not_allowed", 405);
    response.headers.set("Allow", url.pathname === metadataPath ? "GET, OPTIONS" : "POST, OPTIONS");
    return response;
  }
  const connectionString = env.NORTHWIND_NEON_DATABASE_URL;
  if (!connectionString) return unavailable(request, "Northwind PostgreSQL is not configured.");
  if (!readOnlyNorthwindConnection(connectionString)) return unavailable(request, "Northwind PostgreSQL reader configuration is invalid.");
  if (!/^application\/json(?:\s*;|$)/iu.test(request.headers.get("Content-Type") ?? "")) return dtoError(request, "unsupported_media_type", 415);
  const contentLength = request.headers.get("Content-Length");
  if (contentLength !== null && (!/^\d+$/u.test(contentLength) || Number(contentLength) > maxRequestBytes)) return dtoError(request, "request_too_large", 413);
  let query: ValidatedQuery;
  let compiled: ReturnType<typeof compilePostgresQuery>;
  try {
    const body = await readRequestText(request, maxRequestBytes);
    query = validateQuery(JSON.parse(body) as unknown);
    checkQueryTypes(query);
    compiled = compilePostgresQuery(query);
  } catch (error) {
    if (error instanceof RangeError) return dtoError(request, "request_too_large", 413);
    if (error instanceof UnsupportedCollectionError) return dtoError(request, "unsupported_query", 422);
    return dtoError(request, "invalid_request", 400);
  }

  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), neonQueryTimeoutMs);
  try {
    const rows = await makeQuery(connectionString)(compiled.statement, compiled.values, controller.signal);
    const records = rows.map((row) => safeRow(row, query.columns, query.tableName));
    const payload = JSON.stringify({ version: 1, columns: query.columns, primaryKey: query.table.primaryKey, records });
    if (new TextEncoder().encode(payload).byteLength > maxResponseBytes) return dtoError(request, "response_too_large", 413);
    return withCors(new Response(payload, { headers: { "Content-Type": "application/json; charset=utf-8" } }), cors);
  } catch {
    // Driver errors can contain connection details; return only a stable public error.
    return dtoError(request, "query_failed", 500);
  } finally {
    clearTimeout(timeout);
  }
}
