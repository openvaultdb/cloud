import { createECBPublicProxy, ECB_PUBLIC_PATH } from "./ecb-public";
import { DEMODB_HOST, handleDemoDB } from "./demodb";
import { jsonResponse, methodNotAllowed } from "./http";
import { proxyChinook, proxyECB, proxyIANA } from "./chinook";
import { canonicalOVDBPath, isOVDBPublicPath, limitOVDBPublicRequest, looksLikeOVDBPath, type OVDBLimitEnv } from "./ovdb-limits";
import {
  createNorthwindD1Handler,
  createNorthwindNeonHandler,
  northwindRateLimited,
  type NorthwindEnv,
  type NorthwindNeonQueryFactory,
} from "./northwind";
import {
  proxyCloudDatabase,
  proxyDeviceAuthorization,
  type DeviceAuthEnv,
  type UpstreamFetch,
} from "./proxy";

export function createWorker(
  upstreamFetch: UpstreamFetch = fetch,
  northwindNeonQueryFactory?: NorthwindNeonQueryFactory,
): ExportedHandler<Env> {
  const publicECB = createECBPublicProxy(upstreamFetch);
  return {
    async fetch(request, env): Promise<Response> {
      const url = new URL(request.url);
      try {
        // ovdb.demodb.dev serves only the DemoDB OVDB API; no cloud.openvaultdb.com
        // route (OAuth, devices, Northwind, ECB, IANA, assets) is reachable there.
        if (url.hostname === DEMODB_HOST) {
          return await handleDemoDB(request, async (backendRequest) =>
            (await forwardPublicOVDB(backendRequest, env, upstreamFetch)) ?? jsonResponse({ error: "Not found." }, 404));
        }
        if (url.pathname === ECB_PUBLIC_PATH || url.pathname === "/ecb-public" || url.pathname.startsWith("/ecb-public/")) return publicECB(request, env);
        if (url.pathname === "/iana/v1/databases/iana-http-status/dtql") {
          if ((env as Env & { IANA_ENABLED?: string }).IANA_ENABLED !== "true") {
            return jsonResponse({ error: "IANA OVDB is not configured." }, 503);
          }
          if (request.method !== "POST" || url.search !== "" || request.url.includes("?")) {
            return jsonResponse({ error: "IANA query request refused." }, 405);
          }
          return proxyIANA(request, env, upstreamFetch);
        }
        if (url.pathname === "/ecb/v1/databases/ecb/dtql") {
          if ((env as Env & { ECB_ENABLED?: string }).ECB_ENABLED !== "true") {
            return jsonResponse({ error: "ECB OVDB is not configured." }, 503);
          }
          if (request.method !== "POST" || url.search !== "") {
            return jsonResponse({ error: "ECB query request refused." }, 405);
          }
          return proxyECB(request, env, upstreamFetch);
        }
        if (url.pathname.startsWith("/northwind/") && request.method !== "OPTIONS" &&
          !(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "northwind"))) {
          return northwindRateLimited(request);
        }
        if (url.pathname.startsWith("/northwind/d1/")) {
          return await createNorthwindD1Handler(request, env as NorthwindEnv);
        }
        if (url.pathname.startsWith("/northwind/neon/")) {
          return await createNorthwindNeonHandler(request, env as NorthwindEnv, northwindNeonQueryFactory);
        }
        const ovdbResponse = await forwardPublicOVDB(request, env, upstreamFetch);
        if (ovdbResponse !== undefined) return ovdbResponse;
        if (url.pathname === "/oauth/device/code") {
          if (request.method !== "POST") return methodNotAllowed("POST");
          if (!(await allowRequest(env.DEVICE_START_LIMITER, request, "start"))) {
            return jsonResponse(
              { error: "temporarily_unavailable", error_description: "Try again in one minute." },
              429,
            );
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/device_auth/code",
            upstreamFetch,
          );
        }
        if (url.pathname === "/oauth/token") {
          if (request.method !== "POST") return methodNotAllowed("POST");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "token"))) {
            return jsonResponse(
              { error: "slow_down", error_description: "Polling limit exceeded." },
              429,
            );
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/device_auth/token",
            upstreamFetch,
          );
        }
        if (url.pathname === "/oauth/userinfo") {
          if (request.method !== "GET") return methodNotAllowed("GET");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "userinfo"))) {
            return jsonResponse({ error: "temporarily_unavailable" }, 429);
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/device_auth/userinfo",
            upstreamFetch,
          );
        }
        if (url.pathname === "/oauth/revoke") {
          if (request.method !== "POST") return methodNotAllowed("POST");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "revoke"))) {
            return jsonResponse({ error: "temporarily_unavailable" }, 429);
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/device_auth/revoke",
            upstreamFetch,
          );
        }
        if (url.pathname === "/api/device-authorization") {
          if (request.method !== "GET") return methodNotAllowed("GET");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "preview"))) {
            return jsonResponse({ error: "Too many attempts. Try again in one minute." }, 429);
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/device_auth/preview",
            upstreamFetch,
          );
        }
        if (url.pathname === "/api/device-authorization/decision") {
          if (request.method !== "POST") return methodNotAllowed("POST");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "decision"))) {
            return jsonResponse({ error: "Too many attempts. Try again in one minute." }, 429);
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/device_auth/decision",
            upstreamFetch,
          );
        }
        if (url.pathname === "/api/devices") {
          if (request.method !== "GET") return methodNotAllowed("GET");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "devices"))) {
            return jsonResponse({ error: "Too many attempts. Try again in one minute." }, 429);
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/device_auth/devices",
            upstreamFetch,
          );
        }
        if (url.pathname === "/api/devices/revoke") {
          if (request.method !== "POST") return methodNotAllowed("POST");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "device-revoke"))) {
            return jsonResponse({ error: "Too many attempts. Try again in one minute." }, 429);
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/device_auth/devices/revoke",
            upstreamFetch,
          );
        }
        if (url.pathname === "/api/databases") {
          if (request.method !== "GET") return methodNotAllowed("GET");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "databases"))) {
            return jsonResponse({ error: "Too many attempts. Try again in one minute." }, 429);
          }
          return proxyCloudDatabase(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/cloud/databases",
            allowedDatabaseListSearch(url),
            upstreamFetch,
          );
        }
        if (url.pathname.startsWith("/api/databases/")) {
          if (request.method !== "GET") return methodNotAllowed("GET");
          const databaseID = databaseIDFromPath(url.pathname);
          if (!databaseID) {
            return jsonResponse(
              { error: "invalid_request", error_description: "Database id must be one URL-safe path segment." },
              400,
            );
          }
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "database"))) {
            return jsonResponse({ error: "Too many attempts. Try again in one minute." }, 429);
          }
          return proxyCloudDatabase(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/cloud/database",
            `?id=${encodeURIComponent(databaseID)}`,
            upstreamFetch,
          );
        }

        if (request.method !== "GET" && request.method !== "HEAD") {
          return jsonResponse({ error: "Not found." }, 404);
        }
        // Only a non-navigation miss outside run_worker_first reaches this
        // line; the asset layer applies public/_headers and _redirects.
        return await env.ASSETS.fetch(request);
      } catch (error) {
        if (url.pathname.startsWith("/iana/")) {
          console.error("iana_worker_internal_error");
          return jsonResponse({ error: "IANA OVDB is temporarily unavailable." }, 503);
        }
        if (url.pathname.startsWith("/ecb/")) {
          console.error("ecb_worker_internal_error");
          return jsonResponse({ error: "ECB OVDB is temporarily unavailable." }, 500);
        }
        console.error(
          JSON.stringify({
            message: "Unhandled OpenVaultDB Cloud request error",
            method: request.method,
            path: url.pathname,
            error: error instanceof Error ? error.message : "unexpected error",
          }),
        );
        return jsonResponse({ error: "The service could not complete the request." }, 500);
      }
    },
  };
}

/**
 * The public OVDB pipeline shared by cloud.openvaultdb.com and the demodb.dev
 * gateway: canonicalise the path, apply the per-IP limit, forward to Cloud Run.
 * Returns undefined when the path is not a public OVDB route.
 */
async function forwardPublicOVDB(request: Request, env: Env, upstreamFetch: UpstreamFetch): Promise<Response | undefined> {
  const url = new URL(request.url);
  const ovdbPath = canonicalOVDBPath(url.pathname);
  if (ovdbPath !== null && isOVDBPublicPath(ovdbPath)) {
    // Classify and forward the same canonical path (see canonicalOVDBPath).
    const limited = await limitOVDBPublicRequest(request, env as OVDBLimitEnv, ovdbPath);
    if (limited !== undefined) return limited;
    const forwarded = new URL(url);
    forwarded.pathname = ovdbPath;
    return proxyChinook(new Request(forwarded, request), env, upstreamFetch);
  }
  if (ovdbPath === null && looksLikeOVDBPath(url.pathname)) {
    return jsonResponse({ error: { code: "invalid_path", message: "Request path is not in canonical form." } }, 400);
  }
  return undefined;
}

function allowedDatabaseListSearch(url: URL): string {
  const search = new URLSearchParams();
  for (const name of ["space", "pageSize", "pageToken"] as const) {
    for (const value of url.searchParams.getAll(name)) search.append(name, value);
  }
  const encoded = search.toString();
  return encoded ? `?${encoded}` : "";
}

function databaseIDFromPath(pathname: string): string | undefined {
  const encodedID = pathname.slice("/api/databases/".length);
  if (!encodedID || encodedID.includes("/")) return undefined;
  try {
    const databaseID = decodeURIComponent(encodedID);
    return /^[A-Za-z0-9_-]+$/u.test(databaseID) ? databaseID : undefined;
  } catch {
    return undefined;
  }
}

async function allowRequest(
  limiter: RateLimit,
  request: Request,
  route: string,
): Promise<boolean> {
  const actor = request.headers.get("CF-Connecting-IP") ?? "local-or-unknown";
  const { success } = await limiter.limit({ key: `${route}:${actor}` });
  return success;
}
