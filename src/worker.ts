import { jsonResponse, methodNotAllowed, withAssetSecurityHeaders } from "./http";
import {
  proxyCloudDatabase,
  proxyDeviceAuthorization,
  type DeviceAuthEnv,
  type UpstreamFetch,
} from "./proxy";
import { handleDemoRequest, type DemoDependencies, type DemoEnv } from "./demo-session";

export function createWorker(
  upstreamFetch: UpstreamFetch = fetch,
  demoDependencies: DemoDependencies = {},
): ExportedHandler<Env> {
  return {
    async fetch(request, env): Promise<Response> {
      const url = new URL(request.url);
      try {
        const demoResponse = await handleDemoRequest(request, env as DemoEnv, {
          fetch: upstreamFetch,
          ...demoDependencies,
        });
        if (demoResponse) return demoResponse;
        if (url.pathname === "/.well-known/oauth-authorization-server") {
          return request.method === "GET"
            ? authorizationServerMetadata(env)
            : methodNotAllowed("GET");
        }
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
        if (url.pathname === "/api/demo/sessions") {
          if (request.method !== "POST") return methodNotAllowed("POST");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "demo-session-create"))) {
            return jsonResponse({ error: "Too many attempts. Try again in one minute." }, 429);
          }
          return proxyDeviceAuthorization(
            request,
            env as DeviceAuthEnv,
            "/v0/ovdb/demo/sessions",
            upstreamFetch,
          );
        }
        if (url.pathname.startsWith("/api/demo/sessions/")) {
          if (request.method !== "DELETE") return methodNotAllowed("DELETE");
          const sessionID = demoSessionIDFromPath(url.pathname);
          if (!sessionID) return jsonResponse({ error: "invalid_request" }, 400);
          const endRequest = new Request(request.url, {
            method: "POST",
            headers: {
              Authorization: request.headers.get("Authorization") ?? "",
              "Content-Type": "application/json",
            },
            body: JSON.stringify({ sessionId: sessionID }),
          });
          return proxyDeviceAuthorization(
            endRequest,
            env as DeviceAuthEnv,
            "/v0/ovdb/demo/session/end",
            upstreamFetch,
          );
        }
        if (url.pathname === "/api/demo/session") {
          if (request.method !== "GET") return methodNotAllowed("GET");
          const search = allowedDemoSessionSearch(url);
          if (!search) return jsonResponse({ error: "invalid_request" }, 400);
          const backendRequest = new Request(`${url.origin}${url.pathname}${search}`, {
            headers: { Authorization: request.headers.get("Authorization") ?? "" },
          });
          return proxyDeviceAuthorization(
            backendRequest,
            env as DeviceAuthEnv,
            "/v0/ovdb/demo/session",
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
        if (url.pathname === "/") {
          url.pathname = "/device";
          return Response.redirect(url.toString(), 302);
        }
        if (url.pathname === "/device") {
          url.pathname = "/device/";
          return Response.redirect(url.toString(), 302);
        }
        if (url.pathname === "/devices") {
          url.pathname = "/devices/";
          return Response.redirect(url.toString(), 302);
        }
        return withAssetSecurityHeaders(await env.ASSETS.fetch(request));
      } catch (error) {
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

function authorizationServerMetadata(env: Env): Response {
  const grantType = "urn:ietf:params:oauth:grant-type:device_code";
  return jsonResponse({
    issuer: env.PUBLIC_ORIGIN,
    device_authorization_endpoint: `${env.PUBLIC_ORIGIN}/oauth/device/code`,
    token_endpoint: `${env.PUBLIC_ORIGIN}/oauth/token`,
    revocation_endpoint: `${env.PUBLIC_ORIGIN}/oauth/revoke`,
    grant_types_supported: [grantType],
    token_endpoint_auth_methods_supported: ["none"],
    scopes_supported: ["account:read", "databases:read"],
  });
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

function demoSessionIDFromPath(pathname: string): string | undefined {
  const sessionID = pathname.slice("/api/demo/sessions/".length);
  return /^[A-Za-z0-9_-]{1,128}$/u.test(sessionID) ? sessionID : undefined;
}

function allowedDemoSessionSearch(url: URL): string | undefined {
  const spaceIDs = url.searchParams.getAll("spaceId");
  if (spaceIDs.length !== 1 || !/^[A-Za-z0-9_-]{1,128}$/u.test(spaceIDs[0])) return undefined;
  return `?spaceId=${encodeURIComponent(spaceIDs[0])}`;
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
