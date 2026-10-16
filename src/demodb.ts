import demodbOVDB from "./demodb-ovdb.json";

/**
 * The DemoDB OVDB API host: `https://ovdb.demodb.dev/v1`, a Custom Domain of
 * this Worker (see wrangler.jsonc). It replaces `https://demodb.dev/ovdb/v1`,
 * which demodb.dev now redirects here (308).
 *
 * `/v1/...` on this host behaves as `/ovdb/v1/...` did on demodb.dev, response
 * for response, as the demodb-websites Worker (demo-db/websites src/worker.ts:
 * serverApiIndex, proxyOVDB, databaseApiIndex) returned it when it re-fetched
 * https://cloud.openvaultdb.com. Two things differ by design:
 * - the backend call is made in-process through `forward`, the same
 *   rate-limited, origin-secret path cloud.openvaultdb.com uses, so one API call
 *   costs one Worker invocation instead of two;
 * - URLs this host advertises (server index `apiUrl`s, the per-database
 *   `endpoints.dtql`) name ovdb.demodb.dev/v1, so clients never round-trip
 *   through the redirect (a CORS preflight cannot follow one).
 * Only the DemoDB allowlist is served; every other database id is 404.
 *
 * The allowlist (src/demodb-ovdb.json) is a build-time copy of demo-db's
 * generated ovdb.json; scripts/demodb-ovdb.mjs checks it for drift.
 */
export const DEMODB_HOST = "ovdb.demodb.dev";
const BACKEND_ORIGIN = "https://cloud.openvaultdb.com";
const PUBLIC_API = `https://${DEMODB_HOST}/v1`;
const MAX_QUERY_BODY_BYTES = 1 * 1024 * 1024;

type DemoDBDatabase = { localId: string; query: boolean };
const queryCapability: Record<string, boolean> = demodbOVDB.query;
const databases = new Map<string, DemoDBDatabase>(
  demodbOVDB.server.databases.map((database) => [
    database.localId,
    { localId: database.localId, query: queryCapability[database.localId] === true },
  ]),
);
const serverIndexBody = JSON.stringify({
  ...demodbOVDB.server,
  apiUrl: PUBLIC_API,
  databases: demodbOVDB.server.databases.map((database) => ({ ...database, apiUrl: `${PUBLIC_API}/databases/${database.localId}` })),
});

/** Sends one backend request (path `/v1/...`) through the public OVDB pipeline; never throws for upstream failures. */
export type DemoDBForward = (request: Request) => Promise<Response>;

export async function handleDemoDB(request: Request, forward: DemoDBForward): Promise<Response> {
  const url = new URL(request.url);
  if (unsafePath(url.pathname)) return new Response("Bad Request", { status: 400, headers: securityHeaders() });
  if (url.pathname === "/v1" || url.pathname === "/v1/") return serverApiIndex(request);
  if (url.pathname.startsWith("/v1/")) return proxyOVDB(request, url, forward);
  // Nothing else lives on this host (demodb-websites answered such paths from
  // its static bundle: 405 for other methods, else not found).
  if (request.method !== "GET" && request.method !== "HEAD") {
    return new Response("Method Not Allowed", { status: 405, headers: { Allow: "GET, HEAD" } });
  }
  return notFound();
}

function serverApiIndex(request: Request): Response {
  if (request.method === "OPTIONS") return new Response(null, { status: 204, headers: readonlyCorsHeaders() });
  if (request.method !== "GET" && request.method !== "HEAD") {
    return new Response("Method Not Allowed", { status: 405, headers: { ...readonlyCorsHeaders(), Allow: "GET, HEAD, OPTIONS" } });
  }
  return new Response(request.method === "HEAD" ? null : serverIndexBody, {
    headers: { ...readonlyCorsHeaders(), "Content-Type": "application/json; charset=utf-8", "Cache-Control": "public, max-age=300", ...securityHeaders() },
  });
}

async function proxyOVDB(request: Request, url: URL, forward: DemoDBForward): Promise<Response> {
  const match = /^\/v1\/databases\/([a-z][a-z0-9-]{0,39})(\/.*)?$/u.exec(url.pathname);
  const database = match && databases.get(match[1]);
  if (!match || !database) return notFound();
  const suffix = match[2] ?? "";
  if (suffix === "" || suffix === "/") return databaseApiIndex(request, database, forward);
  const query = suffix === "/dtql" || suffix === "/query";
  const recordRead = /^\/records\/[^/]+(?:\/[^/]+)+$/u.test(suffix);
  const readPath = suffix === "/read" || suffix === "/inferred-schema" || recordRead;
  if (!query && !readPath) return notFound();
  if (query && !database.query) return notFound();
  const methods = query ? ["GET", "HEAD", "POST", "OPTIONS"] : ["GET", "HEAD", "OPTIONS"];
  if (request.method === "OPTIONS") return new Response(null, { status: 204, headers: apiCorsHeaders(methods) });
  if (["POST", "PUT", "PATCH", "DELETE"].includes(request.method) && !(query && request.method === "POST")) {
    return new Response("OpenVaultDB is read-only", { status: 403, headers: { ...apiCorsHeaders(methods), Allow: methods.join(", ") } });
  }
  if (!methods.includes(request.method)) {
    return new Response("Method Not Allowed", { status: 405, headers: { ...apiCorsHeaders(methods), Allow: methods.join(", ") } });
  }
  const target = new URL(`/v1/databases/${database.localId}${suffix}${url.search}`, BACKEND_ORIGIN);
  const headers = backendHeaders(request, request.headers.get("Accept") ?? "application/json");
  let body: ArrayBuffer | undefined;
  if (query && request.method === "POST") {
    const length = Number(request.headers.get("Content-Length"));
    if (Number.isFinite(length) && length > MAX_QUERY_BODY_BYTES) {
      return new Response("Query request is too large", { status: 413, headers: apiCorsHeaders(methods) });
    }
    const limitedBody = await readLimitedBody(request, MAX_QUERY_BODY_BYTES);
    if (!limitedBody) return new Response("Query request is too large", { status: 413, headers: apiCorsHeaders(methods) });
    body = limitedBody;
    const contentType = request.headers.get("Content-Type");
    if (contentType) headers.set("Content-Type", contentType);
  }
  for (const name of ["OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close"]) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  try {
    const response = await forward(new Request(target, { method: request.method, headers, body, redirect: "manual", signal: request.signal }));
    if (response.status >= 300 && response.status < 400) {
      return new Response("The OVDB backend returned a redirect", { status: 502, headers: securityHeaders() });
    }
    const outputHeaders = new Headers();
    for (const name of ["Content-Type", "Cache-Control", "ETag", "Last-Modified", "Vary", "Link", "OVDB-Page-Size", "OVDB-Page-Token"]) {
      const value = response.headers.get(name);
      if (value) outputHeaders.set(name, value);
    }
    for (const [name, value] of Object.entries(apiCorsHeaders(methods))) outputHeaders.set(name, value);
    for (const [name, value] of Object.entries(securityHeaders())) outputHeaders.set(name, value);
    return new Response(request.method === "HEAD" ? null : response.body, { status: response.status, statusText: response.statusText, headers: outputHeaders });
  } catch (error) {
    logGatewayFailure(query ? "query" : "read", database, error);
    return new Response("OpenVaultDB is temporarily unavailable", { status: 502, headers: { ...apiCorsHeaders(methods), ...securityHeaders() } });
  }
}

async function databaseApiIndex(request: Request, database: DemoDBDatabase, forward: DemoDBForward): Promise<Response> {
  if (request.method === "OPTIONS") return new Response(null, { status: 204, headers: readonlyCorsHeaders() });
  if (["POST", "PUT", "PATCH", "DELETE"].includes(request.method)) {
    return new Response("OpenVaultDB is read-only", { status: 403, headers: { ...readonlyCorsHeaders(), Allow: "GET, HEAD, OPTIONS" } });
  }
  if (request.method !== "GET" && request.method !== "HEAD") {
    return new Response("Method Not Allowed", { status: 405, headers: { ...readonlyCorsHeaders(), Allow: "GET, HEAD, OPTIONS" } });
  }
  const target = new URL(`/v1/databases/${database.localId}`, BACKEND_ORIGIN);
  try {
    const upstream = await forward(new Request(target, { method: request.method, headers: backendHeaders(request, "application/json"), redirect: "manual", signal: request.signal }));
    if (upstream.status >= 300 && upstream.status < 400) {
      return new Response("The OVDB backend returned a redirect", { status: 502, headers: securityHeaders() });
    }
    const outputHeaders = new Headers();
    for (const name of ["Content-Type", "Cache-Control", "ETag", "Last-Modified", "Vary", "Link"]) {
      const value = upstream.headers.get(name);
      if (value) outputHeaders.set(name, value);
    }
    for (const [name, value] of Object.entries({ ...readonlyCorsHeaders(), "Access-Control-Expose-Headers": "Cache-Control, ETag, Last-Modified, Link, Vary", ...securityHeaders() })) {
      outputHeaders.set(name, value);
    }
    if (request.method === "HEAD" || !upstream.ok) {
      return new Response(request.method === "HEAD" ? null : upstream.body, { status: upstream.status, statusText: upstream.statusText, headers: outputHeaders });
    }
    const metadata = await upstream.json() as { id?: unknown; endpoints?: { dtql?: unknown }; [key: string]: unknown };
    if (!metadata || metadata.id !== database.localId || typeof metadata.endpoints?.dtql !== "string") {
      return new Response("The OVDB backend returned invalid database metadata", { status: 502, headers: { ...readonlyCorsHeaders(), ...securityHeaders() } });
    }
    if (metadata.endpoints.dtql !== `${BACKEND_ORIGIN}/v1/databases/${database.localId}/dtql`) {
      return new Response("The OVDB backend returned an unexpected query endpoint", { status: 502, headers: { ...readonlyCorsHeaders(), ...securityHeaders() } });
    }
    metadata.endpoints.dtql = `${PUBLIC_API}/databases/${database.localId}/dtql`;
    outputHeaders.set("Content-Type", "application/json; charset=utf-8");
    outputHeaders.delete("ETag");
    return new Response(JSON.stringify(metadata), { status: upstream.status, statusText: upstream.statusText, headers: outputHeaders });
  } catch (error) {
    logGatewayFailure("metadata", database, error);
    return new Response("OpenVaultDB is temporarily unavailable", { status: 502, headers: { ...readonlyCorsHeaders(), ...securityHeaders() } });
  }
}

/**
 * The headers demodb-websites sent to cloud.openvaultdb.com, plus the client
 * address so the per-IP limiter keys on the real client. (The cross-Worker hop
 * replaced CF-Connecting-IP with a Cloudflare egress address.) The forwarder
 * never passes CF-Connecting-IP on to Cloud Run.
 */
function backendHeaders(request: Request, accept: string): Headers {
  const headers = new Headers({ Accept: accept });
  const clientIP = request.headers.get("CF-Connecting-IP");
  if (clientIP) headers.set("CF-Connecting-IP", clientIP);
  return headers;
}

function logGatewayFailure(operation: "metadata" | "query" | "read", database: DemoDBDatabase, error: unknown): void {
  const errorName = error instanceof Error
    ? error.name.replace(/[^A-Za-z0-9_.-]/gu, "").slice(0, 40) || "Error"
    : "NonErrorRejection";
  const errorMessage = error instanceof Error
    ? error.message
      .replace(/https?:\/\/[^\s"'<>]+/giu, "[url]")
      .replace(/\b(authorization|cookie|set-cookie|ovdb[-_ ]?page[-_ ]?token|page[-_ ]?token|token|secret|password)\b\s*[:=]\s*(?:Bearer\s+)?(?:"[^"]*"|'[^']*'|[^\s,;]+)/giu, "$1=[redacted]")
      .replace(/\bBearer\s+[A-Za-z0-9._~+/=-]+/giu, "Bearer [redacted]")
      .replace(/([?&][A-Za-z0-9_.-]+=)[^&#\s]*/gu, "$1[redacted]")
      .replace(/[\u0000-\u001f\u007f]/gu, " ")
      .slice(0, 200)
    : "non-error rejection";
  console.error(JSON.stringify({ event: "ovdb_gateway_fetch_failed", operation, database: database.localId, errorName, errorMessage }));
}

async function readLimitedBody(request: Request, maximum: number): Promise<ArrayBuffer | null> {
  if (!request.body) return new ArrayBuffer(0);
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > maximum) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  const combined = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) {
    combined.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return combined.buffer;
}

function unsafePath(pathname: string): boolean {
  if (/%2f|%5c|%00/iu.test(pathname)) return true;
  for (const raw of pathname.split("/")) {
    let segment: string;
    try { segment = decodeURIComponent(raw); } catch { return true; }
    if (segment === "." || segment === ".." || segment.includes("/") || segment.includes("\\") || segment.includes("\0") || segment.includes("%")) return true;
  }
  return false;
}

function apiCorsHeaders(methods: string[]): Record<string, string> {
  const allowed = [...new Set([...methods, "GET", "HEAD", "OPTIONS"])];
  return {
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Methods": allowed.join(", "),
    "Access-Control-Allow-Headers": "Content-Type, Accept, OVDB-Page-Size, OVDB-Page-Token, OVDB-Page-Close",
    "Access-Control-Expose-Headers": "Cache-Control, ETag, Last-Modified, Link, OVDB-Page-Size, OVDB-Page-Token, Vary",
    "Access-Control-Max-Age": "86400",
  };
}

function readonlyCorsHeaders(): Record<string, string> {
  return { "Access-Control-Allow-Origin": "*", "Access-Control-Allow-Methods": "GET, HEAD, OPTIONS", "Access-Control-Allow-Headers": "Content-Type", "Access-Control-Max-Age": "86400" };
}

function securityHeaders(): Record<string, string> {
  return { "X-Content-Type-Options": "nosniff", "Referrer-Policy": "strict-origin-when-cross-origin" };
}

function notFound(): Response {
  return new Response("Not Found", { status: 404, headers: { "Content-Type": "text/plain; charset=utf-8", ...securityHeaders() } });
}
