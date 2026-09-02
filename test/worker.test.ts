import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";

import { PROXY_SECRET_HEADER } from "../src/proxy";
import { createWorker } from "../src/worker";

const baseURL = "https://cloud.openvaultdb.com";
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
});

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
