import { jsonResponse } from "./http";

/**
 * Per-IP limits for the public OVDB routes the Worker forwards to Cloud Run
 * (`/v1/*`, `/ovdb/*`, `/.well-known/openvaultdb`). They are separate buckets
 * from the Northwind/OAuth `DEVICE_LOOKUP_LIMITER`, so DemoDB traffic cannot
 * exhaust device-authorization polling and vice versa.
 *
 * - `OVDB_QUERY_LIMITER`: 60 requests per 60 s per IP for query routes
 *   (`/v1/dtql`, `/v1/databases/{db}/dtql|query|batch|read`, any method but
 *   OPTIONS). The service behind them runs at most 2 instances x 2 concurrent
 *   requests with a 10 s query timeout, so one client sustaining 1 query/s is
 *   already a meaningful share; DataTug issues one or two queries per view.
 * - `OVDB_READ_LIMITER`: 300 requests per 60 s per IP for every other forwarded
 *   OVDB route (descriptors, records, HTML profiles, discovery), which are cheap.
 *
 * Preflight (OPTIONS) is never limited. Cloudflare's rate limiting binding is
 * per location and eventually consistent: these are abuse ceilings, not quotas.
 */
export const OVDB_QUERY_LIMIT_PER_MINUTE = 60;
export const OVDB_READ_LIMIT_PER_MINUTE = 300;

/** Every browser origin the Cloud Run service grants CORS (server/providers.json and DemoDB PostgreSQL editions). */
export const OVDB_PUBLIC_ALLOWED_ORIGINS = [
  "https://adventureworks.demodb.dev",
  "https://chinook.demodb.dev",
  "https://chinookdb.com",
  "https://datatug.app",
  "https://demodb.dev",
  "https://employees.demodb.dev",
  "https://northwind.demodb.dev",
  "https://pubs.demodb.dev",
  "https://sakila.demodb.dev",
  "https://www.chinookdb.com",
  "https://www.demodb.dev",
] as const;

const allowedOrigins = new Set<string>(OVDB_PUBLIC_ALLOWED_ORIGINS);

export type OVDBLimitEnv = Env & {
  OVDB_QUERY_LIMITER?: RateLimit;
  OVDB_READ_LIMITER?: RateLimit;
};

export function isOVDBPublicPath(pathname: string): boolean {
  return pathname === "/.well-known/openvaultdb" || pathname === "/ovdb" || pathname.startsWith("/ovdb/") ||
    pathname === "/v1" || pathname.startsWith("/v1/");
}

/** Query routes take the tighter bucket; everything else forwarded takes the read bucket. */
export function isOVDBQueryPath(pathname: string): boolean {
  return pathname === "/v1/dtql" || /^\/v1\/databases\/[^/]+\/(?:dtql|query|batch|read)\/?$/u.test(pathname);
}

/**
 * Returns a 429 when the caller's bucket is exhausted, or undefined to proceed.
 * A missing binding fails open, as before this limiter existed, so a
 * configuration slip cannot take the public demo down.
 */
export async function limitOVDBPublicRequest(request: Request, env: OVDBLimitEnv, pathname: string): Promise<Response | undefined> {
  if (request.method === "OPTIONS") return undefined;
  const query = isOVDBQueryPath(pathname);
  const limiter = query ? env.OVDB_QUERY_LIMITER : env.OVDB_READ_LIMITER;
  if (!limiter) return undefined;
  const actor = request.headers.get("CF-Connecting-IP") ?? "local-or-unknown";
  const { success } = await limiter.limit({ key: `${query ? "ovdb-query" : "ovdb-read"}:${actor}` });
  return success ? undefined : ovdbRateLimited(request);
}

/** A CORS-correct 429: allowed origins can read it, and caches key on Origin. */
export function ovdbRateLimited(request: Request): Response {
  const response = jsonResponse({ error: { code: "rate_limited", message: "Too many requests. Try again in one minute." } }, 429);
  const result = new Response(response.body, response);
  result.headers.set("Retry-After", "60");
  result.headers.set("Vary", "Origin");
  const origin = request.headers.get("Origin");
  if (origin !== null && allowedOrigins.has(origin)) {
    result.headers.set("Access-Control-Allow-Origin", origin);
    result.headers.set("Access-Control-Expose-Headers", "Retry-After");
  }
  return result;
}
