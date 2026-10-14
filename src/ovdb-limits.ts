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
 * - `OVDB_PREFLIGHT_LIMITER`: 600 CORS preflights (OPTIONS) per 60 s per IP;
 *   a browser sends at most one per distinct request shape, cached for
 *   Access-Control-Max-Age.
 *
 * IPv6 clients are keyed by their /64 prefix. Cloudflare's rate limiting binding is
 * per location and eventually consistent: these are abuse ceilings, not quotas.
 */
export const OVDB_QUERY_LIMIT_PER_MINUTE = 60;
export const OVDB_READ_LIMIT_PER_MINUTE = 300;
export const OVDB_PREFLIGHT_LIMIT_PER_MINUTE = 600;

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
  OVDB_PREFLIGHT_LIMITER?: RateLimit;
};

export function isOVDBPublicPath(pathname: string): boolean {
  return pathname === "/.well-known/openvaultdb" || pathname === "/ovdb" || pathname.startsWith("/ovdb/") ||
    pathname === "/v1" || pathname.startsWith("/v1/");
}

/** Query routes take the tighter bucket; everything else forwarded takes the read bucket. */
export function isOVDBQueryPath(pathname: string): boolean {
  return pathname === "/v1/dtql" || pathname === "/v1/dtql/" ||
    /^\/v1\/databases\/[^/]+\/(?:dtql|query|batch|read)\/?$/u.test(pathname);
}

const unreserved = /^[A-Za-z0-9\-._~]$/u;

/**
 * The one canonical spelling of a request path, used both to classify it for
 * rate limiting and as the path forwarded to Cloud Run, so the two can never
 * disagree. Go's ServeMux decodes percent-escapes before routing, so `d%74ql`
 * would otherwise reach the dtql handler while being classified as a read.
 *
 * Escapes of unreserved characters (RFC 3986 2.3) are decoded, which does not
 * change meaning; every other escape is kept with upper-case hex. A path is
 * refused (null) when it has a malformed escape, an encoded slash or backslash
 * (`%2F`, `%5C`), a raw backslash, an empty segment (`//`), a `.` or `..`
 * segment after decoding, or a `;` parameter: each could route differently on
 * the two sides.
 */
export function canonicalOVDBPath(pathname: string): string | null {
  let out = "";
  for (let i = 0; i < pathname.length; i++) {
    const char = pathname[i];
    if (char !== "%") {
      out += char;
      continue;
    }
    const hex = pathname.slice(i + 1, i + 3);
    if (!/^[0-9A-Fa-f]{2}$/u.test(hex)) return null;
    const decoded = String.fromCharCode(parseInt(hex, 16));
    const upper = hex.toUpperCase();
    if (upper === "2F" || upper === "5C") return null;
    out += unreserved.test(decoded) ? decoded : `%${upper}`;
    i += 2;
  }
  if (!out.startsWith("/") || out.includes("//") || out.includes("\\") || out.includes(";") ||
    /(?:^|\/)\.{1,2}(?:\/|$)/u.test(out)) {
    return null;
  }
  return out;
}

/** Whether a path that could not be canonicalised still aims at an OVDB route. */
export function looksLikeOVDBPath(pathname: string): boolean {
  let loose = pathname;
  try { loose = decodeURIComponent(pathname); } catch { /* keep the raw spelling */ }
  const trimmed = "/" + loose.replace(/^[/\\]+/u, "");
  return isOVDBPublicPath(trimmed) || /^\/(?:v1|ovdb|\.well-known)(?:[/\\;%.]|$)/iu.test(trimmed);
}

/**
 * The rate-limit key of a client: an IPv4 address as is, an IPv6 address by its
 * /64 prefix, because one subscriber usually holds a whole /64 and could
 * otherwise rotate through 2^64 keys.
 */
export function rateLimitActor(ip: string | null): string {
  if (!ip) return "local-or-unknown";
  if (!ip.includes(":")) return ip;
  const mapped = /^::ffff:(\d{1,3}(?:\.\d{1,3}){3})$/iu.exec(ip);
  if (mapped) return mapped[1];
  const [head, tail] = ip.toLowerCase().split("::", 2);
  const headGroups = head ? head.split(":") : [];
  const tailGroups = tail === undefined ? [] : tail ? tail.split(":") : [];
  const groups = tail === undefined ? headGroups : [...headGroups, ...Array(Math.max(0, 8 - headGroups.length - tailGroups.length)).fill("0"), ...tailGroups];
  return `${groups.slice(0, 4).map((group) => (group || "0").replace(/^0+(?=.)/u, "")).join(":")}::/64`;
}

const warnedMissing = new Set<string>();

/** Logs once per isolate that a limiter binding is missing (fail-open stays visible). */
function warnMissingLimiter(name: string): void {
  if (warnedMissing.has(name)) return;
  warnedMissing.add(name);
  console.warn(`${name} binding is missing: public OVDB requests in this bucket are not rate limited`);
}

/** For tests: forget which missing bindings were already reported. */
export function resetMissingLimiterWarnings(): void {
  warnedMissing.clear();
}

/**
 * Returns a 429 when the caller's bucket is exhausted, or undefined to proceed.
 * Preflights have their own bucket. A missing binding fails open, as before this
 * limiter existed, and is reported once per isolate with console.warn.
 */
export async function limitOVDBPublicRequest(request: Request, env: OVDBLimitEnv, pathname: string): Promise<Response | undefined> {
  const [name, bucket] = request.method === "OPTIONS"
    ? ["OVDB_PREFLIGHT_LIMITER", "ovdb-preflight"] as const
    : isOVDBQueryPath(pathname)
      ? ["OVDB_QUERY_LIMITER", "ovdb-query"] as const
      : ["OVDB_READ_LIMITER", "ovdb-read"] as const;
  const limiter = env[name];
  if (!limiter) {
    warnMissingLimiter(name);
    return undefined;
  }
  const { success } = await limiter.limit({ key: `${bucket}:${rateLimitActor(request.headers.get("CF-Connecting-IP"))}` });
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
