import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";

import { PROXY_SECRET_HEADER } from "../src/proxy";
import { createWorker } from "../src/worker";

const baseURL = "https://cloud.openvaultdb.com";
const demoEncryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY";
const demoNow = Date.parse("2026-09-02T12:00:00.000Z");
const upstreamRequests: Request[] = [];
const upstreamFetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
  const request = new Request(input, init);
  upstreamRequests.push(request);
  switch (new URL(request.url).pathname) {
    case "/v0/ovdb/device_auth/code":
      return Response.json({
        device_code: "device-secret",
        user_code: "BCDF-GHJK",
        verification_uri: `${baseURL}/device`,
        verification_uri_complete: `${baseURL}/device?user_code=BCDF-GHJK`,
        expires_in: 600,
        interval: 5,
      });
    case "/v0/ovdb/device_auth/decision":
      return Response.json({ decision: "approve", user_code: "BCDF-GHJK" });
    case "/v0/ovdb/device_auth/devices":
      return Response.json({
        devices: [{ id: "dvc_test", status: "active", can_revoke: true }],
        has_more: false,
      });
    case "/v0/ovdb/device_auth/devices/revoke":
      return Response.json({ device_id: "dvc_test", status: "revoked" });
    case "/v0/ovdb/device_auth/token":
      return Response.json(
        { error: "authorization_pending", error_description: "authorization is still pending" },
        { status: 400 },
      );
    case "/v0/ovdb/cloud/databases":
      return Response.json(
        { databases: [] },
        {
          headers: {
            Authorization: "Bearer backend-secret",
            [PROXY_SECRET_HEADER]: "backend-proxy-secret",
            "X-Untrusted": "discard",
          },
        },
      );
    case "/v0/ovdb/cloud/database":
      return Response.json({ database: { id: new URL(request.url).searchParams.get("id") } });
    case "/v0/ovdb/demo/sessions":
      return Response.json({ sessionId: "session_test", tunnelToken: "secret-only-in-body" });
    case "/v0/ovdb/demo/session":
      return Response.json({ sessionId: "session_test", spaceId: new URL(request.url).searchParams.get("spaceId") });
    case "/v0/ovdb/demo/session/browser":
      return Response.json({ sessionId: "session_test", spaceId: new URL(request.url).searchParams.get("spaceId") });
    case "/v0/ovdb/demo/session/end":
      return new Response(null, { status: 204 });
    default:
      return Response.json({ error: "not_found" }, { status: 404 });
  }
};
const worker = createWorker(upstreamFetch);
const fetchWorker = worker.fetch as unknown as (
  request: Request,
  environment: Env,
  context: ExecutionContext,
) => Response | Promise<Response>;

beforeEach(() => {
  upstreamRequests.length = 0;
});

describe("OpenVaultDB Cloud device authorization facade", () => {
  it("serves the approval page with browser security headers", async () => {
    const redirect = await call("/device");
    expect(redirect.status).toBe(302);
    expect(redirect.headers.get("Location")).toBe(`${baseURL}/device/`);

    const page = await call("/device/");
    expect(page.status).toBe(200);
    const contentSecurityPolicy = page.headers.get("Content-Security-Policy");
    expect(contentSecurityPolicy).toContain(
      "script-src 'self' https://www.gstatic.com https://apis.google.com",
    );
    expect(contentSecurityPolicy).toContain(
      "connect-src 'self' https://www.gstatic.com https://*.googleapis.com",
    );
    expect(contentSecurityPolicy).toContain("frame-src https://auth.sneat.co");
    expect(contentSecurityPolicy).toContain("frame-ancestors 'none'");
    const pageHTML = await page.text();
    expect(pageHTML).toContain("Connect your command line");
    expect(pageHTML).toContain("OpenVaultDB is a Sneat Co. product");
    expect(pageHTML).toContain("Sign in with GitHub");
    expect(pageHTML).toContain("Sign in with Google");
    expect(pageHTML).toContain("Sign in with email");
    expect(pageHTML).not.toContain("Continue with GitHub");
    expect(pageHTML).toContain("View authorized devices");

    const devicesRedirect = await call("/devices");
    expect(devicesRedirect.status).toBe(302);
    expect(devicesRedirect.headers.get("Location")).toBe(`${baseURL}/devices/`);
    const devicesPage = await call("/devices/");
    expect(devicesPage.status).toBe(200);
    expect(await devicesPage.text()).toContain("Authorized devices");
  });

  it("publishes stable OAuth discovery metadata", async () => {
    const response = await call("/.well-known/oauth-authorization-server");
    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      issuer: baseURL,
      device_authorization_endpoint: `${baseURL}/oauth/device/code`,
      token_endpoint: `${baseURL}/oauth/token`,
      revocation_endpoint: `${baseURL}/oauth/revoke`,
      scopes_supported: ["account:read", "databases:read"],
    });
  });

  it("maps public paths to authenticated backend requests", async () => {
    const start = await formPost("/oauth/device/code", {
      client_id: "ovdb-cli",
      scope: "account:read",
      device_name: "Test Mac",
      os: "darwin",
      arch: "arm64",
      client_version: "0.2.0",
    });
    expect(start.status).toBe(200);
    await expect(start.json()).resolves.toMatchObject({ user_code: "BCDF-GHJK" });
    expect(upstreamRequests).toHaveLength(1);
    const upstreamStart = upstreamRequests[0];
    expect(upstreamStart.url).toBe("https://api.sneat.cloud/v0/ovdb/device_auth/code");
    expect(upstreamStart.headers.get(PROXY_SECRET_HEADER)).toBe("test-proxy-secret");
    expect(upstreamStart.headers.get("Origin")).toBeNull();
    const startBody = new URLSearchParams(
      new TextDecoder().decode(await upstreamStart.arrayBuffer()),
    );
    expect(startBody.get("client_id")).toBe("ovdb-cli");
    expect(startBody.get("device_name")).toBe("Test Mac");
    expect(startBody.get("client_version")).toBe("0.2.0");

    const decision = await call("/api/device-authorization/decision", {
      method: "POST",
      headers: {
        Authorization: "Bearer firebase-id-token",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ user_code: "BCDF-GHJK", decision: "approve" }),
    });
    expect(decision.status).toBe(200);
    expect(upstreamRequests[1].headers.get("Authorization")).toBe(
      "Bearer firebase-id-token",
    );
    expect(new URL(upstreamRequests[1].url).pathname).toBe(
      "/v0/ovdb/device_auth/decision",
    );
  });

  it("proxies authenticated device listing and revocation", async () => {
    const list = await call("/api/devices", {
      headers: { Authorization: "Bearer firebase-id-token" },
    });
    expect(list.status).toBe(200);
    await expect(list.json()).resolves.toMatchObject({
      devices: [{ id: "dvc_test", status: "active" }],
    });
    expect(upstreamRequests[0].headers.get("Authorization")).toBe(
      "Bearer firebase-id-token",
    );
    expect(new URL(upstreamRequests[0].url).pathname).toBe(
      "/v0/ovdb/device_auth/devices",
    );

    const revoke = await call("/api/devices/revoke", {
      method: "POST",
      headers: {
        Authorization: "Bearer firebase-id-token",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ device_id: "dvc_test" }),
    });
    expect(revoke.status).toBe(200);
    expect(new URL(upstreamRequests[1].url).pathname).toBe(
      "/v0/ovdb/device_auth/devices/revoke",
    );
    expect(await upstreamRequests[1].text()).toContain("dvc_test");
  });

  it("preserves backend OAuth errors and no-store headers", async () => {
    const response = await formPost("/oauth/token", {
      grant_type: "urn:ietf:params:oauth:grant-type:device_code",
      client_id: "ovdb-cli",
      device_code: "device-secret",
    });
    expect(response.status).toBe(400);
    expect(response.headers.get("Cache-Control")).toBe("no-store");
    await expect(response.json()).resolves.toMatchObject({ error: "authorization_pending" });
  });

  it("returns a stable unavailable error when the backend cannot be reached", async () => {
    const unavailableWorker = createWorker(async () => {
      throw new Error("backend offline");
    });
    const unavailableFetch = unavailableWorker.fetch as unknown as typeof fetchWorker;
    const response = await unavailableFetch(
      new Request(`${baseURL}/oauth/device/code`, {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({ client_id: "ovdb-cli" }),
      }),
      env,
      createExecutionContext(),
    );
    expect(response.status).toBe(503);
    await expect(response.json()).resolves.toMatchObject({ error: "temporarily_unavailable" });
  });

  it("rejects unsupported methods before contacting the backend", async () => {
    const response = await call("/oauth/device/code", { method: "GET" });
    expect(response.status).toBe(405);
    expect(response.headers.get("Allow")).toBe("POST");
    expect(upstreamRequests).toHaveLength(0);
  });

  it("proxies an allowlisted database list query with bearer auth and safe headers", async () => {
    const response = await call(
      "/api/databases?space=personal&pageSize=100&pageToken=next&upstream=https://attacker.example",
      { headers: { Authorization: "Bearer cloud-access-token", "X-OVDB-Proxy-Secret": "attacker" } },
    );
    expect(response.status).toBe(200);
    expect(response.headers.get("Cache-Control")).toBe("no-store");
    expect(response.headers.get("Authorization")).toBeNull();
    expect(response.headers.get(PROXY_SECRET_HEADER)).toBeNull();
    expect(response.headers.get("X-Untrusted")).toBeNull();
    expect(upstreamRequests).toHaveLength(1);
    const upstream = upstreamRequests[0];
    expect(upstream.url).toBe(
      "https://api.sneat.cloud/v0/ovdb/cloud/databases?space=personal&pageSize=100&pageToken=next",
    );
    expect(upstream.headers.get("Authorization")).toBe("Bearer cloud-access-token");
    expect(upstream.headers.get(PROXY_SECRET_HEADER)).toBe("test-proxy-secret");
  });

  it("routes one safe decoded database id to the fixed backend detail endpoint", async () => {
    const response = await call("/api/databases/db_test-42?%69d=attacker", {
      headers: { Authorization: "Bearer cloud-access-token" },
    });
    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({ database: { id: "db_test-42" } });
    expect(upstreamRequests).toHaveLength(1);
    expect(upstreamRequests[0].url).toBe(
      "https://api.sneat.cloud/v0/ovdb/cloud/database?id=db_test-42",
    );
  });

  it.each(["/api/databases/", "/api/databases/a/b", "/api/databases/%2F", "/api/databases/%ZZ"])(
    "rejects malformed database id %s before contacting the backend",
    async (pathname) => {
      const response = await call(pathname, { headers: { Authorization: "Bearer cloud-access-token" } });
      expect(response.status).toBe(400);
      await expect(response.json()).resolves.toMatchObject({ error: "invalid_request" });
      expect(upstreamRequests).toHaveLength(0);
    },
  );

  it.each(["POST", "PUT", "DELETE", "PATCH"])(
    "returns 405 for %s database writes without contacting the backend",
    async (method) => {
      const response = await call("/api/databases/db_test", { method });
      expect(response.status).toBe(405);
      expect(response.headers.get("Allow")).toBe("GET");
      expect(upstreamRequests).toHaveLength(0);
    },
  );

  it("preserves safe backend database error status without exposing credentials", async () => {
    const deniedWorker = createWorker(async () =>
      Response.json(
        { error: "insufficient_scope", error_description: "Read access is required." },
        { status: 403, headers: { Authorization: "Bearer backend-secret" } },
      ),
    );
    const deniedFetch = deniedWorker.fetch as unknown as typeof fetchWorker;
    const response = await deniedFetch(
      new Request(`${baseURL}/api/databases`, {
        headers: { Authorization: "Bearer cloud-access-token" },
      }),
      env,
      createExecutionContext(),
    );
    expect(response.status).toBe(403);
    expect(response.headers.get("Authorization")).toBeNull();
    await expect(response.json()).resolves.toMatchObject({ error: "insufficient_scope" });
  });

  it("maps the bounded demo control routes to the backend without treating them as data relay requests", async () => {
    const kv = new MemoryKV();
    const created = await demoCall(worker, kv, "/api/demo/sessions", {
      method: "POST", headers: { Authorization: "Bearer device-token", "Content-Type": "application/json" },
      body: JSON.stringify({ app: "listus" }),
    });
    expect(created.status).toBe(200);
    expect(upstreamRequests[0].url).toBe("https://api.sneat.cloud/v0/ovdb/demo/sessions");
    expect(upstreamRequests[0].headers.get("Authorization")).toBe("Bearer device-token");
    const metadata = await demoCall(worker, kv, "/api/demo/session?spaceId=space_1&upstream=attacker", {
      headers: { Authorization: "Bearer firebase-token" },
    });
    expect(metadata.status).toBe(200);
    expect(upstreamRequests[1].url).toBe("https://api.sneat.cloud/v0/ovdb/demo/session?spaceId=space_1");
    expect(upstreamRequests[1].headers.get("Authorization")).toBe("Bearer firebase-token");
    const browserMetadata = await demoCall(worker, kv, "/api/demo/session/browser?spaceId=space_1&upstream=attacker", {
      headers: { Authorization: "Bearer firebase-id-token", Origin: "https://listus.app" },
    });
    expect(browserMetadata.status).toBe(200);
    expect(browserMetadata.headers.get("Access-Control-Allow-Origin")).toBe("https://listus.app");
    expect(upstreamRequests[2].url).toBe("https://api.sneat.cloud/v0/ovdb/demo/session/browser?spaceId=space_1");
    expect(upstreamRequests[2].headers.get("Authorization")).toBe("Bearer firebase-id-token");
    const ended = await demoCall(worker, kv, "/api/demo/sessions/session_test", {
      method: "DELETE", headers: { Authorization: "Bearer device-token" },
    });
    expect(ended.status).toBe(204);
    expect(upstreamRequests[3].url).toBe("https://api.sneat.cloud/v0/ovdb/demo/session/end");
    await expect(upstreamRequests[3].json()).resolves.toEqual({ sessionId: "session_test" });
  });
});

describe("Listus demo session relay", () => {
  it("stores an encrypted immutable session, replaces an old session, and revokes it", async () => {
    const kv = new MemoryKV();
    const demoWorker = createWorker(async () => Response.json({ ok: true }), { now: () => demoNow });
    const put = await demoCall(demoWorker, kv, "/internal/demo/sessions/session-demo-000001", {
      method: "PUT", headers: controlHeaders(), body: JSON.stringify(session("session-demo-000001")),
    });
    expect(put.status).toBe(204);
    expect(kv.values.get("demo:session:session-demo-000001")).not.toContain("database-secret");
    const retry = await demoCall(demoWorker, kv, "/internal/demo/sessions/session-demo-000001", {
      method: "PUT", headers: controlHeaders(), body: JSON.stringify(session("session-demo-000001")),
    });
    expect(retry.status).toBe(204);
    kv.values.delete("demo:owner:owner_1");
    kv.values.delete("demo:space:space_1");
    kv.values.delete("demo:origin:ovdb-demo-session-demo-000001.openvaultdb.com");
    const repaired = await demoCall(demoWorker, kv, "/internal/demo/sessions/session-demo-000001", {
      method: "PUT", headers: controlHeaders(), body: JSON.stringify(session("session-demo-000001")),
    });
    expect(repaired.status).toBe(204);
    expect(kv.values.get("demo:owner:owner_1")).toBe("session-demo-000001");
    const replacement = await demoCall(demoWorker, kv, "/internal/demo/sessions/session-demo-000002", {
      method: "PUT", headers: controlHeaders(), body: JSON.stringify(session("session-demo-000002")),
    });
    expect(replacement.status).toBe(204);
    expect(kv.values.has("demo:session:session-demo-000001")).toBe(false);
    const deleted = await demoCall(demoWorker, kv, "/internal/demo/sessions/session-demo-000002", {
      method: "DELETE", headers: controlHeaders(),
    });
    expect(deleted.status).toBe(204);
    expect(kv.values.has("demo:session:session-demo-000002")).toBe(false);
  });

  it("rejects an invalid control secret, an oversized body, and an expired session at equality", async () => {
    const kv = new MemoryKV();
    const demoWorker = createWorker(async () => Response.json({ ok: true }), { now: () => demoNow });
    const denied = await demoCall(demoWorker, kv, "/internal/demo/sessions/session-demo-000001", {
      method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify(session("session-demo-000001")),
    });
    expect(denied.status).toBe(401);
    const huge = await demoCall(demoWorker, kv, "/internal/demo/sessions/session-demo-000001", {
      method: "PUT", headers: { ...controlHeaders(), "Content-Length": "70000" }, body: JSON.stringify(session("session-demo-000001")),
    });
    expect(huge.status).toBe(400);
    await putSession(demoWorker, kv, session("session-demo-000001", undefined, new Date(demoNow + 1_000).toISOString()));
    const equalityWorker = createWorker(async () => Response.json({ ok: true }), { now: () => demoNow + 1_000 });
    const expired = await demoCall(equalityWorker, kv, stablePath, { headers: { Authorization: "Bearer invalid", Origin: "https://listus.app" } });
    expect(expired.status).toBe(410);
    expect(expired.headers.get("Access-Control-Allow-Origin")).toBe("https://listus.app");
  });

  it("verifies Firebase signature, issuer and audience before owner-only proxying", async () => {
    const kv = new MemoryKV();
    const received: Request[] = [];
    const key = await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"]);
    const firebaseJwksFetch = async () => Response.json(await firebaseJwks(key.publicKey));
    const demoWorker = createWorker(async (input, init) => {
      received.push(new Request(input, init));
      return Response.json({ lists: [] }, { headers: { "X-OVDB-Demo-Control-Secret": "never-forward" } });
    }, { now: () => demoNow - 1, firebaseJwksFetch });
    await putSession(demoWorker, kv, session("session-demo-000001"));
    const valid = await signedFirebaseToken(key.privateKey, { sub: "owner_1" });
    const response = await demoCall(demoWorker, kv, stablePath, {
      headers: { Authorization: `Bearer ${valid}`, Origin: "https://listus.app", Cookie: "must-not-forward" },
    });
    expect(response.status).toBe(200);
    expect(response.headers.get("Access-Control-Allow-Origin")).toBe("https://listus.app");
    expect(response.headers.get("X-OVDB-Demo-Control-Secret")).toBeNull();
    expect(received).toHaveLength(1);
    expect(new URL(received[0].url).pathname).toBe("/v1/databases/demo-sneat-space/records/lists/do%3Ademo");
    expect(received[0].headers.get("Authorization")).toBe("Bearer database-secret");
    expect(received[0].headers.get("Cookie")).toBeNull();
    const badAudience = await signedFirebaseToken(key.privateKey, { sub: "owner_1", aud: "attacker" });
    const refused = await demoCall(demoWorker, kv, stablePath, { headers: { Authorization: `Bearer ${badAudience}` } });
    expect(refused.status).toBe(401);
    const otherOwner = await signedFirebaseToken(key.privateKey, { sub: "owner_2" });
    const forbidden = await demoCall(demoWorker, kv, stablePath, { headers: { Authorization: `Bearer ${otherOwner}` } });
    expect(forbidden.status).toBe(403);
    const nonNumericAuthTime = await signedFirebaseToken(key.privateKey, { sub: "owner_1", auth_time: "not-a-time" });
    const malformed = await demoCall(demoWorker, kv, stablePath, { headers: { Authorization: `Bearer ${nonNumericAuthTime}` } });
    expect(malformed.status).toBe(401);
  });

  it("refreshes JWKs once for a rotated key rather than trusting a stale cache miss", async () => {
    const kv = new MemoryKV(); let calls = 0;
    const key = await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"]);
    const worker = createWorker(async () => Response.json({ ok: true }), {
      now: () => demoNow - 1,
      firebaseJwksFetch: async () => Response.json(calls++ === 0 ? { keys: [] } : await firebaseJwks(key.publicKey)),
    });
    await putSession(worker, kv, session("session-demo-000001"));
    const token = await signedFirebaseToken(key.privateKey, { sub: "owner_1" });
    const response = await demoCall(worker, kv, stablePath, { headers: { Authorization: `Bearer ${token}` } });
    expect(response.status).toBe(200);
    expect(calls).toBe(2);
  });

  it("uses Cache API cooldown to avoid refreshing an unknown JWK kid on every invalid request", async () => {
    const jwksURL = "https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com";
    const cache = await caches.open("ovdb-demo-jwks");
    await cache.delete(new Request(jwksURL));
    await cache.delete(new Request(`${jwksURL}?unknown-kid=cooldown-kid`));
    const kv = new MemoryKV(); let calls = 0;
    const key = await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"]);
    const worker = createWorker(async () => Response.json({ ok: true }), {
      now: () => demoNow - 1, firebaseJwksFetch: async () => { calls += 1; return Response.json({ keys: [] }, { headers: { "Cache-Control": "public, max-age=60" } }); },
    });
    await putSession(worker, kv, session("session-demo-000001"));
    const token = await signedFirebaseToken(key.privateKey, { sub: "owner_1" }, "cooldown-kid");
    expect((await demoCall(worker, kv, stablePath, { headers: { Authorization: `Bearer ${token}` } })).status).toBe(401);
    expect((await demoCall(worker, kv, stablePath, { headers: { Authorization: `Bearer ${token}` } })).status).toBe(401);
    expect(calls).toBe(2);
  });

  it("applies the same lease and owner checks to the exact origin hostname", async () => {
    const kv = new MemoryKV();
    let proxied = 0;
    const key = await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"]);
    const worker = createWorker(async () => { proxied += 1; return Response.json({ ok: true }); }, {
      now: () => demoNow - 1, firebaseJwksFetch: async () => Response.json(await firebaseJwks(key.publicKey)),
    });
    await putSession(worker, kv, session("session-demo-000001"));
    const directWithoutIdentity = await demoCall(worker, kv, "/v1/databases/demo-sneat-space/records/lists/do%3Ademo", { host: "ovdb-demo-session-demo-000001.openvaultdb.com" });
    expect(directWithoutIdentity.status).toBe(401);
    expect(proxied).toBe(0);
    const owner = await signedFirebaseToken(key.privateKey, { sub: "owner_1" });
    const directOwner = await demoCall(worker, kv, "/v1/databases/demo-sneat-space/records/lists/do%3Ademo", { host: "ovdb-demo-session-demo-000001.openvaultdb.com", headers: { Authorization: `Bearer ${owner}` } });
    expect(directOwner.status).toBe(200);
    expect(proxied).toBe(1);
    const adminPath = await demoCall(worker, kv, "/admin/tokens", { host: "ovdb-demo-session-demo-000001.openvaultdb.com", headers: { Authorization: `Bearer ${owner}` } });
    expect(adminPath.status).toBe(404);
    expect(proxied).toBe(1);
  });

  it("keeps a valid near-expiry session in KV for the platform minimum without extending its lease", async () => {
    const kv = new MemoryKV();
    const worker = createWorker(async () => Response.json({ ok: true }), { now: () => demoNow });
    await putSession(worker, kv, session("session-demo-000001", undefined, new Date(demoNow + 40_000).toISOString()));
    expect(kv.expirations.get("demo:session:session-demo-000001")).toBeGreaterThanOrEqual(Math.floor(demoNow / 1000) + 60);
  });

  it("fails closed for disabled or unconfigured exact origin hostnames", async () => {
    const request = new Request("https://ovdb-demo-session-demo-000001.openvaultdb.com/v1/databases/demo-sneat-space/records/lists/do%3Ademo");
    const disabled = createWorker(async () => Response.json({ asset: true }));
    const disabledResponse = await (disabled.fetch as unknown as (request: Request, environment: Env, context: ExecutionContext) => Promise<Response>)(request, env, createExecutionContext());
    expect(disabledResponse.status).toBe(503);
    const disabledControl = await (disabled.fetch as unknown as (request: Request, environment: Env, context: ExecutionContext) => Promise<Response>)(new Request("https://cloud.openvaultdb.com/api/demo/session?spaceId=space_1"), env, createExecutionContext());
    expect(disabledControl.status).toBe(503);
    const unconfigured = { ...env, OVDB_DEMO_ENABLED: "true" } as unknown as Env;
    const unconfiguredResponse = await (disabled.fetch as unknown as (request: Request, environment: Env, context: ExecutionContext) => Promise<Response>)(request, unconfigured, createExecutionContext());
    expect(unconfiguredResponse.status).toBe(503);
  });

  it("bounds origin failures without exposing a redirect or timing out forever", async () => {
    const kv = new MemoryKV();
    const key = await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"]);
    const timeoutWorker = createWorker(async () => { throw new DOMException("timed out", "TimeoutError"); }, {
      now: () => demoNow - 1, firebaseJwksFetch: async () => Response.json(await firebaseJwks(key.publicKey)),
    });
    await putSession(timeoutWorker, kv, session("session-demo-000001"));
    const token = await signedFirebaseToken(key.privateKey, { sub: "owner_1" });
    const timeout = await demoCall(timeoutWorker, kv, stablePath, { headers: { Authorization: `Bearer ${token}` } });
    expect(timeout.status).toBe(504);
    const redirectWorker = createWorker(async () => new Response(null, { status: 302, headers: { Location: "https://attacker.example" } }), {
      now: () => demoNow - 1, firebaseJwksFetch: async () => Response.json(await firebaseJwks(key.publicKey)),
    });
    const redirect = await demoCall(redirectWorker, kv, stablePath, { headers: { Authorization: `Bearer ${token}` } });
    expect(redirect.status).toBe(502);
    expect(redirect.headers.get("Location")).toBeNull();
  });

  it("accepts native bodyless write acknowledgements and refuses an arbitrary origin URL", async () => {
    const kv = new MemoryKV();
    const key = await crypto.subtle.generateKey({ name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" }, true, ["sign", "verify"]);
    const nativePaths: string[] = [];
    const worker = createWorker(async input => { nativePaths.push(new URL(input.toString()).pathname); return new Response(null, { status: 204 }); }, {
      now: () => demoNow - 1, firebaseJwksFetch: async () => Response.json(await firebaseJwks(key.publicKey)),
    });
    const invalidOrigin = await demoCall(worker, kv, "/internal/demo/sessions/session-demo-000001", {
      method: "PUT", headers: controlHeaders(), body: JSON.stringify(session("session-demo-000001", "https://attacker.example/")),
    });
    expect(invalidOrigin.status).toBe(400);
    await putSession(worker, kv, session("session-demo-000001"));
    const token = await signedFirebaseToken(key.privateKey, { sub: "owner_1" });
    const write = await demoCall(worker, kv, stablePath, { method: "PUT", headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" }, body: "{}" });
    expect(write.status).toBe(204);
    expect(write.headers.get("Content-Type")).toBeNull();
    expect(nativePaths).toEqual(["/v1/databases/demo-sneat-space/records/lists/do%3Ademo"]);
  });

  it("adds strict Listus CORS to the explicit browser metadata control response", async () => {
    const kv = new MemoryKV();
    const worker = createWorker(async () => Response.json({ sessionId: "safe", spaceId: "space_1" }));
    const preflight = await demoCall(worker, kv, "/api/demo/session/browser?spaceId=space_1", {
      method: "OPTIONS", headers: { Origin: "https://listus.app", "Access-Control-Request-Method": "GET" },
    });
    expect(preflight.status).toBe(204);
    expect(preflight.headers.get("Access-Control-Allow-Origin")).toBe("https://listus.app");
    const metadata = await demoCall(worker, kv, "/api/demo/session/browser?spaceId=space_1", {
      headers: { Origin: "https://listus.app", Authorization: "Bearer firebase-id-token" },
    });
    expect(metadata.status).toBe(200);
    expect(metadata.headers.get("Access-Control-Allow-Origin")).toBe("https://listus.app");
  });

  it.each(["/users/owner_1/demo-sneat-space/ovdb/v1/databases/demo-sneat-space/%2fsecret", "/users/owner_1/demo-sneat-space/ovdb/v1/databases/demo-sneat-space/../secret"]) (
    "rejects malicious stable path %s before contacting the origin", async (path) => {
      const kv = new MemoryKV(); let called = false;
      const worker = createWorker(async () => { called = true; return Response.json({ ok: true }); });
      const response = await demoCall(worker, kv, path);
      expect(response.status).toBe(404);
      expect(called).toBe(false);
    },
  );
});

const stablePath = "/users/owner_1/demo-sneat-space/ovdb/v1/databases/demo-sneat-space/records/lists/do%3Ademo";

class MemoryKV {
  readonly values = new Map<string, string>();
  readonly expirations = new Map<string, number>();
  async get(key: string): Promise<string | null> { return this.values.get(key) ?? null; }
  async put(key: string, value: string, options?: { expiration?: number }): Promise<void> { this.values.set(key, value); if (options?.expiration) this.expirations.set(key, options.expiration); }
  async delete(key: string): Promise<void> { this.values.delete(key); }
}

function session(sessionId: string, originUrl = `https://ovdb-demo-${sessionId}.openvaultdb.com/`, expiresAt = new Date(demoNow + 60_000).toISOString()) {
  return { sessionId, ownerUserId: "owner_1", spaceId: "space_1", databaseId: "demo-sneat-space", expiresAt, originUrl, originToken: "database-secret" };
}

function controlHeaders(): Record<string, string> { return { "Content-Type": "application/json", "X-OVDB-Demo-Control-Secret": "control-secret" }; }

async function putSession(worker: ReturnType<typeof createWorker>, kv: MemoryKV, value: ReturnType<typeof session>) {
  const response = await demoCall(worker, kv, `/internal/demo/sessions/${value.sessionId}`, { method: "PUT", headers: controlHeaders(), body: JSON.stringify(value) });
  expect(response.status).toBe(204);
}

function demoCall(worker: ReturnType<typeof createWorker>, kv: MemoryKV, pathname: string, init: RequestInit & { host?: string } = {}): Promise<Response> {
  const host = init.host ?? "cloud.openvaultdb.com";
  const { host: _host, ...requestInit } = init;
  const demoEnv = {
    ...env,
    OVDB_DEMO_ENABLED: "true",
    OVDB_DEMO_CONTROL_SECRET: "control-secret",
    OVDB_DEMO_ENCRYPTION_KEY: demoEncryptionKey,
    OVDB_DEMO_FIREBASE_PROJECT_ID: "sneat-eur3-1",
    OVDB_DEMO_CORS_ORIGIN: "https://listus.app",
    OVDB_DEMO_ORIGIN_HOST_SUFFIX: "openvaultdb.com",
    OVDB_DEMO_SESSIONS: kv,
  } as unknown as Env;
  return (worker.fetch as unknown as (request: Request, environment: Env, context: ExecutionContext) => Promise<Response>)(new Request(`https://${host}${pathname}`, requestInit), demoEnv, createExecutionContext());
}

async function signedFirebaseToken(privateKey: CryptoKey, override: Partial<Record<string, string | number>>, kid = "test_key"): Promise<string> {
  const claims = { sub: "owner_1", aud: "sneat-eur3-1", iss: "https://securetoken.google.com/sneat-eur3-1", exp: Math.floor((demoNow + 60_000) / 1000), iat: Math.floor((demoNow - 60_000) / 1000), auth_time: Math.floor((demoNow - 60_000) / 1000), ...override };
  const header = encode({ alg: "RS256", kid }); const body = encode(claims);
  const signature = await crypto.subtle.sign("RSASSA-PKCS1-v1_5", privateKey, new TextEncoder().encode(`${header}.${body}`));
  return `${header}.${body}.${base64url(signature)}`;
}

function encode(value: unknown): string { return base64url(new TextEncoder().encode(JSON.stringify(value))); }
function base64url(value: ArrayBuffer | Uint8Array): string { return btoa(String.fromCharCode(...new Uint8Array(value))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=/g, ""); }
async function firebaseJwks(publicKey: CryptoKey): Promise<{ keys: Array<JsonWebKey & { kid: string; alg: string; use: string }> }> {
  const key = await crypto.subtle.exportKey("jwk", publicKey);
  return { keys: [{ ...key, kid: "test_key", alg: "RS256", use: "sig" }] };
}

function formPost(pathname: string, values: Record<string, string>): Promise<Response> {
  return call(pathname, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams(values),
  });
}

function call(pathname: string, init?: RequestInit): Promise<Response> {
  return fetchWorker(
    new Request(`${baseURL}${pathname}`, init),
    env,
    createExecutionContext(),
  ) as Promise<Response>;
}
