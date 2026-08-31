import { verifyFirebaseIdentity, type VerifyIdentity } from "./firebase";
import { jsonResponse, methodNotAllowed, withAssetSecurityHeaders } from "./http";
import {
  authorizationServerMetadata,
  cleanupExpiredState,
  handleAuthorizationDecision,
  handleAuthorizationPreview,
  handleDeviceAuthorization,
  handleTokenExchange,
  handleTokenRevocation,
  handleUserInfo,
} from "./oauth";

export function createWorker(
  verifyIdentity: VerifyIdentity = verifyFirebaseIdentity,
): ExportedHandler<Env> {
  return {
    async fetch(request, env, ctx): Promise<Response> {
      const url = new URL(request.url);
      try {
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
          return handleDeviceAuthorization(request, env);
        }
        if (url.pathname === "/oauth/token") {
          if (request.method !== "POST") return methodNotAllowed("POST");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "token"))) {
            return jsonResponse(
              { error: "slow_down", error_description: "Polling limit exceeded." },
              429,
            );
          }
          return handleTokenExchange(request, env);
        }
        if (url.pathname === "/oauth/userinfo") {
          return request.method === "GET"
            ? handleUserInfo(request, env, ctx)
            : methodNotAllowed("GET");
        }
        if (url.pathname === "/oauth/revoke") {
          return request.method === "POST"
            ? handleTokenRevocation(request, env)
            : methodNotAllowed("POST");
        }
        if (url.pathname === "/api/device-authorization") {
          if (request.method !== "GET") return methodNotAllowed("GET");
          if (!(await allowRequest(env.DEVICE_LOOKUP_LIMITER, request, "preview"))) {
            return jsonResponse({ error: "Too many attempts. Try again in one minute." }, 429);
          }
          return handleAuthorizationPreview(request, env);
        }
        if (url.pathname === "/api/device-authorization/decision") {
          return request.method === "POST"
            ? handleAuthorizationDecision(request, env, verifyIdentity)
            : methodNotAllowed("POST");
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
    scheduled(_controller, env, ctx): void {
      ctx.waitUntil(cleanupExpiredState(env.DB));
    },
  };
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
