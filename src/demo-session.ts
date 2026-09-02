import { jsonResponse, methodNotAllowed } from "./http";

const MAX_JSON_BYTES = 64 * 1024;
const MAX_RESPONSE_BYTES = 1024 * 1024;
const REQUEST_TIMEOUT_MS = 10_000;
const JWKS_TIMEOUT_MS = 3_000;
const MAX_JWKS_BYTES = 64 * 1024;
const UNKNOWN_KID_COOLDOWN_SECONDS = 60;
const FIREBASE_JWKS_URL = "https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com";
const SESSION_KEY_PREFIX = "demo:session:";
const SPACE_KEY_PREFIX = "demo:space:";
const OWNER_KEY_PREFIX = "demo:owner:";
const ORIGIN_KEY_PREFIX = "demo:origin:";
const stablePrefix = "/users/";
const stableDatabaseID = "demo-sneat-space";

export type DemoEnv = Env & {
  OVDB_DEMO_ENABLED?: string;
  OVDB_DEMO_CONTROL_SECRET?: string;
  OVDB_DEMO_ENCRYPTION_KEY?: string;
  OVDB_DEMO_FIREBASE_PROJECT_ID?: string;
  OVDB_DEMO_CORS_ORIGIN?: string;
  OVDB_DEMO_ORIGIN_HOST_SUFFIX?: string;
  OVDB_DEMO_SESSIONS?: KVNamespace;
};

export type DemoFetch = (input: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

export type DemoDependencies = {
  fetch?: DemoFetch;
  firebaseJwksFetch?: DemoFetch;
  now?: () => number;
};

type StoredSession = {
  sessionId: string;
  ownerUserId: string;
  spaceId: string;
  databaseId: string;
  expiresAt: string;
  originUrl: string;
  encryptedOriginToken: EncryptedValue;
};

type EncryptedValue = { iv: string; ciphertext: string };
type FirebaseClaims = { sub: string; aud: string; iss: string; exp: number; iat: number; auth_time: number };
type FirebaseHeader = { alg: string; kid: string };
type FirebaseJwk = JsonWebKey & { kid?: string; alg?: string; use?: string };
type FirebaseJwkSet = { keys: FirebaseJwk[] };

export async function handleDemoRequest(
  request: Request,
  env: DemoEnv,
  dependencies: DemoDependencies = {},
): Promise<Response | undefined> {
  const url = new URL(request.url);
  if (!isDemoEnabled(env)) {
    if (isDemoPath(url.pathname) || isPotentialDemoOriginHost(url.hostname)) return withDemoCors(jsonResponse({ error: "demo_unavailable" }, 503), request, env);
    return undefined;
  }
  if (!demoConfigured(env)) {
    if (isDemoPath(url.pathname) || isPotentialDemoOriginHost(url.hostname)) return withDemoCors(jsonResponse({ error: "demo_unavailable" }, 503), request, env);
    return undefined;
  }

  if (url.pathname.startsWith("/internal/demo/sessions/")) {
    return handleInternalSession(request, env, dependencies);
  }
  if (url.pathname.startsWith(stablePrefix)) {
    return handleStableDatabaseRequest(request, env, dependencies);
  }
  const sessionID = await env.OVDB_DEMO_SESSIONS!.get(originKey(url.hostname));
  if (sessionID) return handleOriginHostRequest(request, env, sessionID, dependencies);
  if (isPotentialDemoOriginHost(url.hostname)) return jsonResponse({ error: "session_unavailable" }, 503);
  return undefined;
}

function isDemoEnabled(env: DemoEnv): boolean {
  return String(env.OVDB_DEMO_ENABLED) === "true";
}

function demoConfigured(env: DemoEnv): boolean {
  return Boolean(
    env.OVDB_DEMO_CONTROL_SECRET &&
      env.OVDB_DEMO_ENCRYPTION_KEY &&
      env.OVDB_DEMO_FIREBASE_PROJECT_ID &&
      env.OVDB_DEMO_CORS_ORIGIN &&
      env.OVDB_DEMO_ORIGIN_HOST_SUFFIX &&
      env.OVDB_DEMO_SESSIONS,
  );
}

function isDemoPath(pathname: string): boolean {
  return pathname.startsWith("/internal/demo/") || pathname.startsWith("/api/demo/") || pathname.startsWith(stablePrefix);
}

function isPotentialDemoOriginHost(hostname: string): boolean {
  return /^ovdb-demo-[a-z0-9-]+\.openvaultdb\.com$/u.test(hostname.toLowerCase());
}

async function handleInternalSession(
  request: Request,
  env: DemoEnv,
  dependencies: DemoDependencies,
): Promise<Response> {
  const sessionID = sessionIDFromInternalPath(new URL(request.url).pathname);
  if (!sessionID) return jsonResponse({ error: "invalid_request" }, 400);
  const controlSecret = request.headers.get("X-OVDB-Demo-Control-Secret") ?? "";
  if (!(await timingSafeEqual(controlSecret, env.OVDB_DEMO_CONTROL_SECRET!))) {
    return jsonResponse({ error: "unauthorized" }, 401);
  }
  if (request.method === "DELETE") return deleteSession(env, sessionID);
  if (request.method !== "PUT") return methodNotAllowed("PUT, DELETE");

  let candidate: unknown;
  try {
    candidate = await readBoundedJSON(request);
  } catch {
    return jsonResponse({ error: "invalid_request" }, 400);
  }
  const session = await parseIncomingSession(candidate, sessionID, env, dependencies.now ?? Date.now);
  if (!session) return jsonResponse({ error: "invalid_request" }, 400);

  const existing = await env.OVDB_DEMO_SESSIONS!.get(sessionKey(sessionID));
  if (existing) {
    const stored = parseStoredSession(existing);
    if (!stored || !(await sameSession(stored, session, env.OVDB_DEMO_ENCRYPTION_KEY!))) {
      return jsonResponse({ error: "session_conflict" }, 409);
    }
    if (!(await repairSessionIndexes(env, stored, (dependencies.now ?? Date.now)()))) {
      return jsonResponse({ error: "session_conflict" }, 409);
    }
    return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
  }

  const expirySeconds = kvExpirationSeconds(Date.parse(session.expiresAt), (dependencies.now ?? Date.now)());
  const replacedSessionID = await env.OVDB_DEMO_SESSIONS!.get(ownerKey(session.ownerUserId));
  if (replacedSessionID && replacedSessionID !== sessionID) await deleteSession(env, replacedSessionID);
  await env.OVDB_DEMO_SESSIONS!.put(sessionKey(sessionID), JSON.stringify(session), { expiration: expirySeconds });
  await env.OVDB_DEMO_SESSIONS!.put(spaceKey(session.spaceId), sessionID, { expiration: expirySeconds });
  await env.OVDB_DEMO_SESSIONS!.put(ownerKey(session.ownerUserId), sessionID, { expiration: expirySeconds });
  await env.OVDB_DEMO_SESSIONS!.put(originKey(new URL(session.originUrl).hostname), sessionID, {
    expiration: expirySeconds,
  });
  if ((await env.OVDB_DEMO_SESSIONS!.get(ownerKey(session.ownerUserId))) !== sessionID) {
    return jsonResponse({ error: "session_conflict" }, 409);
  }
  return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
}

async function repairSessionIndexes(env: DemoEnv, session: StoredSession, now: number): Promise<boolean> {
  const expiration = kvExpirationSeconds(Date.parse(session.expiresAt), now);
  const indexes = [
    [spaceKey(session.spaceId), session.sessionId],
    [ownerKey(session.ownerUserId), session.sessionId],
    [originKey(new URL(session.originUrl).hostname), session.sessionId],
  ] as const;
  for (const [key, value] of indexes) {
    const current = await env.OVDB_DEMO_SESSIONS!.get(key);
    if (current && current !== value) return false;
  }
  for (const [key, value] of indexes) {
    if ((await env.OVDB_DEMO_SESSIONS!.get(key)) === null) {
      await env.OVDB_DEMO_SESSIONS!.put(key, value, { expiration });
    }
  }
  return true;
}

async function deleteSession(env: DemoEnv, sessionID: string): Promise<Response> {
  const serialized = await env.OVDB_DEMO_SESSIONS!.get(sessionKey(sessionID));
  if (!serialized) return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
  const session = parseStoredSession(serialized);
  await env.OVDB_DEMO_SESSIONS!.delete(sessionKey(sessionID));
  if (session) {
    await deleteIndexIfCurrent(env, spaceKey(session.spaceId), sessionID);
    await deleteIndexIfCurrent(env, ownerKey(session.ownerUserId), sessionID);
    await deleteIndexIfCurrent(env, originKey(new URL(session.originUrl).hostname), sessionID);
  }
  return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
}

async function deleteIndexIfCurrent(env: DemoEnv, key: string, sessionID: string): Promise<void> {
  if ((await env.OVDB_DEMO_SESSIONS!.get(key)) === sessionID) await env.OVDB_DEMO_SESSIONS!.delete(key);
}

async function handleStableDatabaseRequest(
  request: Request,
  env: DemoEnv,
  dependencies: DemoDependencies,
): Promise<Response> {
  const parsed = parseStablePath(new URL(request.url).pathname);
  if (!parsed) return withDemoCors(jsonResponse({ error: "not_found" }, 404), request, env);
  const sessionID = await env.OVDB_DEMO_SESSIONS!.get(ownerKey(parsed.ownerUserId));
  if (!sessionID) return withDemoCors(jsonResponse({ error: "session_unavailable" }, 404), request, env);
  const session = await loadAuthorizedSession(request, env, sessionID, dependencies);
  if (session instanceof Response) return withDemoCors(session, request, env);
  if (session.ownerUserId !== parsed.ownerUserId || session.databaseId !== stableDatabaseID) {
    return withDemoCors(jsonResponse({ error: "not_found" }, 404), request, env);
  }
  return withDemoCors(await proxySessionRequest(request, env, session, parsed.suffix, dependencies), request, env);
}

async function handleOriginHostRequest(
  request: Request,
  env: DemoEnv,
  sessionID: string,
  dependencies: DemoDependencies,
): Promise<Response> {
  if (!isNativeDatabaseRecordPath(new URL(request.url).pathname)) {
    return withDemoCors(jsonResponse({ error: "not_found" }, 404), request, env);
  }
  const session = await loadAuthorizedSession(request, env, sessionID, dependencies);
  if (session instanceof Response) return withDemoCors(session, request, env);
  return withDemoCors(await proxySessionRequest(request, env, session, new URL(request.url).pathname, dependencies), request, env);
}

async function loadAuthorizedSession(
  request: Request,
  env: DemoEnv,
  sessionID: string,
  dependencies: DemoDependencies,
): Promise<StoredSession | Response> {
  const serialized = await env.OVDB_DEMO_SESSIONS!.get(sessionKey(sessionID));
  const session = serialized ? parseStoredSession(serialized) : undefined;
  if (!session || !Number.isFinite(Date.parse(session.expiresAt))) {
    return jsonResponse({ error: "session_unavailable" }, 404);
  }
  if ((dependencies.now ?? Date.now)() >= Date.parse(session.expiresAt)) {
    return jsonResponse({ error: "session_expired" }, 410);
  }
  const cors = corsResponse(request, env);
  if (cors) return cors;
  if (!allowedMethod(request.method)) return methodNotAllowed("GET, POST, PUT, PATCH, DELETE, OPTIONS");
  if (!requestIsBoundedJSON(request)) return jsonResponse({ error: "invalid_request" }, 400);
  const claims = await verifyFirebaseIDToken(request, env, dependencies);
  if (claims instanceof Response) return claims;
  if (claims.sub !== session.ownerUserId) return jsonResponse({ error: "forbidden" }, 403);
  return session;
}

async function proxySessionRequest(
  request: Request,
  env: DemoEnv,
  session: StoredSession,
  suffix: string,
  dependencies: DemoDependencies,
): Promise<Response> {
  const token = await decrypt(session.encryptedOriginToken, env.OVDB_DEMO_ENCRYPTION_KEY!);
  if (!token) return jsonResponse({ error: "session_unavailable" }, 503);
  const origin = new URL(session.originUrl);
  origin.pathname = joinPath(origin.pathname, suffix);
  origin.search = new URL(request.url).search;
  const headers = new Headers();
  headers.set("Accept", "application/json");
  headers.set("Authorization", `Bearer ${token}`);
  const contentType = request.headers.get("Content-Type");
  if (contentType) headers.set("Content-Type", contentType);
  const body = request.method === "GET" || request.method === "DELETE" ? undefined : boundedBody(request.body);
  try {
    const upstream = await (dependencies.fetch ?? fetch)(origin, {
      method: request.method,
      headers,
      body,
      redirect: "manual",
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
    if (upstream.status >= 300 && upstream.status < 400) return jsonResponse({ error: "origin_rejected" }, 502);
    const length = Number(upstream.headers.get("Content-Length") ?? "0");
    if (Number.isFinite(length) && length > MAX_RESPONSE_BYTES) {
      return jsonResponse({ error: "response_too_large" }, 502);
    }
    if ([204, 205, 304].includes(upstream.status)) {
      const responseHeaders = new Headers({ "Cache-Control": "no-store", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff" });
      applyCors(responseHeaders, request, env);
      return new Response(null, { status: upstream.status, headers: responseHeaders });
    }
    if (!upstream.headers.get("Content-Type")?.toLowerCase().startsWith("application/json")) {
      return jsonResponse({ error: "origin_rejected" }, 502);
    }
    const responseHeaders = new Headers({
      "Cache-Control": "no-store",
      "Content-Type": upstream.headers.get("Content-Type")!,
      "Referrer-Policy": "no-referrer",
      "X-Content-Type-Options": "nosniff",
    });
    applyCors(responseHeaders, request, env);
    return new Response(boundedBody(upstream.body, MAX_RESPONSE_BYTES), {
      status: upstream.status,
      headers: responseHeaders,
    });
  } catch (error) {
    if (error instanceof DOMException && error.name === "TimeoutError") {
      return jsonResponse({ error: "origin_timeout" }, 504);
    }
    return jsonResponse({ error: "origin_unavailable" }, 503);
  }
}

function parseStablePath(pathname: string): { ownerUserId: string; suffix: string } | undefined {
  const prefix = "/users/";
  const marker = "/demo-sneat-space/ovdb";
  if (!pathname.startsWith(prefix)) return undefined;
  const markerIndex = pathname.indexOf(marker, prefix.length);
  if (markerIndex < 0) return undefined;
  const ownerUserId = pathname.slice(prefix.length, markerIndex);
  const suffix = pathname.slice(markerIndex + marker.length);
  if (!isNativeDatabaseRecordPath(suffix)) return undefined;
  if (!safeID(ownerUserId) || !safePathSuffix(suffix)) return undefined;
  return { ownerUserId, suffix };
}

function isNativeDatabaseRecordPath(pathname: string): boolean {
  return pathname.startsWith(`/v1/databases/${stableDatabaseID}/records/`);
}

function sessionIDFromInternalPath(pathname: string): string | undefined {
  const value = pathname.slice("/internal/demo/sessions/".length);
  return safeSessionID(value) ? value : undefined;
}

function safeSessionID(value: string): boolean { return /^[a-z0-9-]{16,48}$/u.test(value); }

function safeID(value: string): boolean {
  return /^[A-Za-z0-9_-]{1,128}$/u.test(value);
}

function safePathSuffix(value: string): boolean {
  return /^\/(?:[A-Za-z0-9._~-]|%[0-9A-Fa-f]{2}|\/)*$/u.test(value) &&
    !/%2f|%5c|%2e%2e/iu.test(value) && !value.includes("//");
}

function allowedMethod(method: string): boolean {
  return ["GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"].includes(method);
}

function requestIsBoundedJSON(request: Request): boolean {
  if (["GET", "DELETE"].includes(request.method)) return true;
  const contentType = request.headers.get("Content-Type")?.split(";", 1)[0];
  const length = Number(request.headers.get("Content-Length") ?? "0");
  return contentType === "application/json" && (!Number.isFinite(length) || length <= MAX_JSON_BYTES);
}

function corsResponse(request: Request, env: DemoEnv): Response | undefined {
  const origin = request.headers.get("Origin");
  if (origin && origin !== env.OVDB_DEMO_CORS_ORIGIN) return jsonResponse({ error: "cors_forbidden" }, 403);
  if (request.method !== "OPTIONS") return undefined;
  const headers = new Headers({ "Cache-Control": "no-store", "Access-Control-Allow-Methods": "GET, POST, PUT, PATCH, DELETE, OPTIONS", "Access-Control-Allow-Headers": "Authorization, Content-Type", "Access-Control-Max-Age": "600" });
  applyCors(headers, request, env);
  return new Response(null, { status: 204, headers });
}

function applyCors(headers: Headers, request: Request, env: DemoEnv): void {
  if (request.headers.get("Origin") === env.OVDB_DEMO_CORS_ORIGIN) {
    headers.set("Access-Control-Allow-Origin", env.OVDB_DEMO_CORS_ORIGIN!);
    headers.set("Vary", "Origin");
  }
}

function withDemoCors(response: Response, request: Request, env: DemoEnv): Response {
  const headers = new Headers(response.headers);
  applyCors(headers, request, env);
  return new Response(response.body, { status: response.status, statusText: response.statusText, headers });
}

async function parseIncomingSession(
  value: unknown,
  sessionID: string,
  env: DemoEnv,
  now: () => number,
): Promise<StoredSession | undefined> {
  if (!value || typeof value !== "object") return undefined;
  const input = value as Record<string, unknown>;
  const ownerUserId = stringField(input, "ownerUserId");
  const spaceId = stringField(input, "spaceId");
  const databaseId = stringField(input, "databaseId");
  const expiresAt = stringField(input, "expiresAt");
  const originUrl = stringField(input, "originUrl");
  const originToken = stringField(input, "originToken");
  if (!ownerUserId || !spaceId || !databaseId || !expiresAt || !originUrl || !originToken) return undefined;
  if (!safeID(ownerUserId) || !safeID(spaceId) || databaseId !== stableDatabaseID) return undefined;
  const expiry = Date.parse(expiresAt);
  if (!Number.isFinite(expiry) || expiry <= now() || expiry > now() + 60 * 60 * 1000) return undefined;
  let origin: URL;
  try { origin = new URL(originUrl); } catch { return undefined; }
  const originSuffix = env.OVDB_DEMO_ORIGIN_HOST_SUFFIX!.toLowerCase();
  const expectedHostname = `ovdb-demo-${sessionID}.${originSuffix}`;
  if (origin.protocol !== "https:" || origin.username || origin.password || origin.search || origin.hash ||
      origin.port || origin.pathname !== "/" || origin.hostname.toLowerCase() !== expectedHostname) return undefined;
  const encryptedOriginToken = await encrypt(originToken, env.OVDB_DEMO_ENCRYPTION_KEY!);
  return { sessionId: sessionID, ownerUserId, spaceId, databaseId, expiresAt: new Date(expiry).toISOString(), originUrl: origin.toString(), encryptedOriginToken };
}

function stringField(input: Record<string, unknown>, field: string): string | undefined {
  const value = input[field];
  return typeof value === "string" && value.length > 0 && value.length <= 4096 ? value : undefined;
}

function parseStoredSession(value: string): StoredSession | undefined {
  try {
    const parsed = JSON.parse(value) as StoredSession;
    return parsed && safeSessionID(parsed.sessionId) && safeID(parsed.ownerUserId) && safeID(parsed.spaceId) && parsed.databaseId === stableDatabaseID && typeof parsed.expiresAt === "string" && typeof parsed.originUrl === "string" && parsed.encryptedOriginToken ? parsed : undefined;
  } catch { return undefined; }
}

async function sameSession(left: StoredSession, right: StoredSession, key: string): Promise<boolean> {
  return left.sessionId === right.sessionId && left.ownerUserId === right.ownerUserId &&
    left.spaceId === right.spaceId && left.databaseId === right.databaseId &&
    left.expiresAt === right.expiresAt && left.originUrl === right.originUrl &&
    (await decrypt(left.encryptedOriginToken, key)) === (await decrypt(right.encryptedOriginToken, key));
}

async function verifyFirebaseIDToken(request: Request, env: DemoEnv, dependencies: DemoDependencies): Promise<FirebaseClaims | Response> {
  const bearer = request.headers.get("Authorization")?.match(/^Bearer ([^\s]+)$/u)?.[1];
  if (!bearer) return jsonResponse({ error: "unauthorized" }, 401);
  const parts = bearer.split(".");
  if (parts.length !== 3) return jsonResponse({ error: "unauthorized" }, 401);
  try {
    const header = decodeJSON<FirebaseHeader>(parts[0]);
    const claims = decodeJSON<FirebaseClaims>(parts[1]);
    const nowSeconds = Math.floor((dependencies.now ?? Date.now)() / 1000);
    const project = env.OVDB_DEMO_FIREBASE_PROJECT_ID!;
    if (header.alg !== "RS256" || !header.kid || claims.aud !== project || claims.iss !== `https://securetoken.google.com/${project}` || !safeID(claims.sub) || !validNumericDate(claims.exp) || !validNumericDate(claims.iat) || !validNumericDate(claims.auth_time) || claims.exp <= nowSeconds || claims.exp <= claims.iat || claims.exp - claims.iat > 3600 || claims.iat > nowSeconds + 60 || claims.auth_time > nowSeconds + 60) throw new Error("invalid claims");
    let keys = await loadFirebaseJwks(dependencies);
    let jwk = keys.keys.find(key => key.kid === header.kid && key.kty === "RSA" && key.alg === "RS256" && key.use === "sig");
    if (!jwk && await claimUnknownKidRefresh(header.kid)) {
      keys = await loadFirebaseJwks(dependencies, true);
      jwk = keys.keys.find(key => key.kid === header.kid && key.kty === "RSA" && key.alg === "RS256" && key.use === "sig");
      if (jwk) await clearUnknownKidCooldown(header.kid);
    }
    if (!jwk) throw new Error("unknown key");
    const publicKey = await crypto.subtle.importKey("jwk", jwk, { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" }, false, ["verify"]);
    const valid = await crypto.subtle.verify("RSASSA-PKCS1-v1_5", publicKey, toArrayBuffer(base64UrlDecode(parts[2])), new TextEncoder().encode(`${parts[0]}.${parts[1]}`));
    if (!valid) throw new Error("invalid signature");
    return claims;
  } catch { return jsonResponse({ error: "unauthorized" }, 401); }
}

function decodeJSON<T>(value: string): T { return JSON.parse(new TextDecoder().decode(base64UrlDecode(value))) as T; }
function validNumericDate(value: unknown): value is number { return typeof value === "number" && Number.isSafeInteger(value) && value > 0; }
async function loadFirebaseJwks(dependencies: DemoDependencies, refresh = false): Promise<FirebaseJwkSet> {
  const jwksFetch = dependencies.firebaseJwksFetch ?? fetch;
  const cacheKey = new Request(FIREBASE_JWKS_URL);
  const cache = await caches.open("ovdb-demo-jwks");
  const cached = refresh ? undefined : await cache.match(cacheKey);
  if (cached) return readJwks(cached);
  const response = await jwksFetch(FIREBASE_JWKS_URL, { signal: AbortSignal.timeout(JWKS_TIMEOUT_MS) });
  const body = await readJwksBody(response);
  const parsed = parseJwks(body);
  const maxAge = cacheMaxAgeSeconds(response.headers.get("Cache-Control"));
  if (maxAge > 0) {
    const headers = new Headers(response.headers);
    headers.set("Cache-Control", `public, max-age=${maxAge}`);
    await cache.put(cacheKey, new Response(toArrayBuffer(body), { status: response.status, headers }));
  }
  return parsed;
}
async function claimUnknownKidRefresh(kid: string): Promise<boolean> {
  const cache = await caches.open("ovdb-demo-jwks");
  const key = new Request(`${FIREBASE_JWKS_URL}?unknown-kid=${encodeURIComponent(kid)}`);
  if (await cache.match(key)) return false;
  await cache.put(key, new Response(null, { headers: { "Cache-Control": `public, max-age=${UNKNOWN_KID_COOLDOWN_SECONDS}` } }));
  return true;
}
async function clearUnknownKidCooldown(kid: string): Promise<void> {
  const cache = await caches.open("ovdb-demo-jwks");
  await cache.delete(new Request(`${FIREBASE_JWKS_URL}?unknown-kid=${encodeURIComponent(kid)}`));
}
async function readJwks(response: Response): Promise<FirebaseJwkSet> {
  return parseJwks(await readJwksBody(response));
}
async function readJwksBody(response: Response): Promise<Uint8Array> {
  if (!response.ok) throw new Error("keys unavailable");
  const length = Number(response.headers.get("Content-Length") ?? "0");
  if (Number.isFinite(length) && length > MAX_JWKS_BYTES) throw new Error("keys too large");
  return readBoundedBytes(response.body, MAX_JWKS_BYTES);
}
function parseJwks(body: Uint8Array): FirebaseJwkSet {
  const parsed = JSON.parse(new TextDecoder().decode(body)) as FirebaseJwkSet;
  if (!Array.isArray(parsed.keys) || parsed.keys.length > 32) throw new Error("invalid keys");
  return parsed;
}
function cacheMaxAgeSeconds(cacheControl: string | null): number {
  const match = /(?:^|,)\s*max-age=(\d+)/iu.exec(cacheControl ?? "");
  return match ? Math.min(Number(match[1]), 3600) : 0;
}
function base64Decode(value: string): Uint8Array { return Uint8Array.from(atob(value), char => char.charCodeAt(0)); }
function base64UrlDecode(value: string): Uint8Array { return base64Decode(value.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - value.length % 4) % 4)); }
function base64UrlEncode(value: ArrayBuffer | Uint8Array): string { return btoa(String.fromCharCode(...new Uint8Array(value))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=/g, ""); }
function toArrayBuffer(value: Uint8Array): ArrayBuffer { return value.buffer.slice(value.byteOffset, value.byteOffset + value.byteLength) as ArrayBuffer; }

async function cryptoKey(encoded: string): Promise<CryptoKey> {
  const bytes = base64UrlDecode(encoded);
  if (bytes.byteLength !== 32) throw new Error("invalid encryption key");
  return crypto.subtle.importKey("raw", toArrayBuffer(bytes), "AES-GCM", false, ["encrypt", "decrypt"]);
}
async function encrypt(value: string, key: string): Promise<EncryptedValue> {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ciphertext = await crypto.subtle.encrypt({ name: "AES-GCM", iv }, await cryptoKey(key), new TextEncoder().encode(value));
  return { iv: base64UrlEncode(iv), ciphertext: base64UrlEncode(ciphertext) };
}
async function decrypt(value: EncryptedValue, key: string): Promise<string | undefined> {
  try { return new TextDecoder().decode(await crypto.subtle.decrypt({ name: "AES-GCM", iv: toArrayBuffer(base64UrlDecode(value.iv)) }, await cryptoKey(key), toArrayBuffer(base64UrlDecode(value.ciphertext)))); } catch { return undefined; }
}
async function timingSafeEqual(left: string, right: string): Promise<boolean> {
  const a = new TextEncoder().encode(left); const b = new TextEncoder().encode(right);
  if (a.byteLength !== b.byteLength) return false;
  const digestA = await crypto.subtle.digest("SHA-256", a); const digestB = await crypto.subtle.digest("SHA-256", b);
  const aa = new Uint8Array(digestA); const bb = new Uint8Array(digestB); let diff = 0;
  for (let i = 0; i < aa.length; i += 1) diff |= aa[i] ^ bb[i];
  return diff === 0;
}
async function readBoundedJSON(request: Request): Promise<unknown> {
  if (request.headers.get("Content-Type")?.split(";", 1)[0] !== "application/json") throw new Error("content type");
  const length = Number(request.headers.get("Content-Length") ?? "0"); if (Number.isFinite(length) && length > MAX_JSON_BYTES) throw new Error("too large");
  const body = await readBoundedBytes(request.body, MAX_JSON_BYTES); return JSON.parse(new TextDecoder().decode(body));
}
async function readBoundedBytes(body: ReadableStream<Uint8Array> | null, limit: number): Promise<Uint8Array> {
  if (!body) return new Uint8Array();
  const reader = body.getReader(); const chunks: Uint8Array[] = []; let total = 0;
  try {
    while (true) {
      const next = await reader.read();
      if (next.done) break;
      total += next.value.byteLength;
      if (total > limit) throw new Error("too large");
      chunks.push(next.value);
    }
  } finally { reader.releaseLock(); }
  const result = new Uint8Array(total); let offset = 0;
  for (const chunk of chunks) { result.set(chunk, offset); offset += chunk.byteLength; }
  return result;
}
function boundedBody(body: ReadableStream<Uint8Array> | null, limit = MAX_JSON_BYTES): ReadableStream<Uint8Array> | undefined {
  if (!body) return undefined; let total = 0;
  return body.pipeThrough(new TransformStream({ transform(chunk, controller) { total += chunk.byteLength; if (total > limit) throw new Error("body too large"); controller.enqueue(chunk); } }));
}
function joinPath(base: string, suffix: string): string { return `${base.replace(/\/$/u, "")}${suffix.startsWith("/") ? suffix : `/${suffix}`}`; }
function sessionKey(id: string): string { return `${SESSION_KEY_PREFIX}${id}`; }
function spaceKey(id: string): string { return `${SPACE_KEY_PREFIX}${id}`; }
function ownerKey(id: string): string { return `${OWNER_KEY_PREFIX}${id}`; }
function originKey(host: string): string { return `${ORIGIN_KEY_PREFIX}${host}`; }
function kvExpirationSeconds(expiresAt: number, now: number): number {
  return Math.max(Math.floor(expiresAt / 1000), Math.floor(now / 1000) + 60);
}
