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
    case "/v0/ovdb/device_auth/token":
      return Response.json(
        { error: "authorization_pending", error_description: "authorization is still pending" },
        { status: 400 },
      );
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
    expect(page.headers.get("Content-Security-Policy")).toContain(
      "frame-src https://auth.sneat.co",
    );
    expect(page.headers.get("Content-Security-Policy")).toContain("frame-ancestors 'none'");
    const pageHTML = await page.text();
    expect(pageHTML).toContain("Connect your command line");
    expect(pageHTML).toContain("OpenVaultDB is a Sneat Co. product");
  });

  it("publishes stable OAuth discovery metadata", async () => {
    const response = await call("/.well-known/oauth-authorization-server");
    expect(response.status).toBe(200);
    await expect(response.json()).resolves.toMatchObject({
      issuer: baseURL,
      device_authorization_endpoint: `${baseURL}/oauth/device/code`,
      token_endpoint: `${baseURL}/oauth/token`,
      revocation_endpoint: `${baseURL}/oauth/revoke`,
      scopes_supported: ["account:read"],
    });
  });

  it("maps public paths to authenticated backend requests", async () => {
    const start = await formPost("/oauth/device/code", {
      client_id: "ovdb-cli",
      scope: "account:read",
    });
    expect(start.status).toBe(200);
    await expect(start.json()).resolves.toMatchObject({ user_code: "BCDF-GHJK" });
    expect(upstreamRequests).toHaveLength(1);
    const upstreamStart = upstreamRequests[0];
    expect(upstreamStart.url).toBe("https://api.sneat.cloud/v0/ovdb/device_auth/code");
    expect(upstreamStart.headers.get(PROXY_SECRET_HEADER)).toBe("test-proxy-secret");
    expect(upstreamStart.headers.get("Origin")).toBeNull();
    expect(new TextDecoder().decode(await upstreamStart.arrayBuffer())).toContain(
      "client_id=ovdb-cli",
    );

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
