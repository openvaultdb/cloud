import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { proxyChinook, createTrustedNoRetentionOVDBProxy, NO_RETENTION_OVDB_TIMEOUT_MS } from "../src/chinook";

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

describe("public Chinook OVDB proxy", () => {
  const proxyEnv = { ...env, CHINOOK_RUN_ORIGIN: "https://synthetic.example" } as Env;
  const futureProxy = createTrustedNoRetentionOVDBProxy();
  it.each([proxyChinook, futureProxy].flatMap((proxy, branch) => ["a".repeat(32), "bad", "a".repeat(32) + "," + "b".repeat(32), ""].map((id) => ({ proxy, branch, id }))))("preserves execution ID verbatim for backend validation: branch $branch, $id", async ({ proxy, branch, id }) => {
    const request = new Request(`${baseURL}/v1/databases/synthetic/query`, { headers: { "OVDB-Execution-ID": id } });
    const response = await proxy(request, proxyEnv, async (_input, init) => {
      expect(new Headers(init?.headers).get("OVDB-Execution-ID")).toBe(id);
      expect(init?.cache).toBe(branch === 0 ? undefined : "no-store");
      expect(init?.signal).toBeDefined();
      return new Response(null, { status: 400, headers: { "Cache-Control": "public,max-age=99" } });
    });
    expect(response.headers.get("Cache-Control")).toBe(branch === 0 ? "public,max-age=99" : "no-store");
  });

  it.each([proxyChinook, futureProxy])("cancels a blocked upstream body on downstream reader cancellation", async (proxy) => {
    let upstreamCancelled = false;
    let signal: AbortSignal | null | undefined;
    const accept = "application/vnd.openvaultdb.query-stream+json";
    let forwardedAccept: string | null = null;
    const response = await proxy(new Request(`${baseURL}/v1/databases/synthetic/dtql`, { headers: { Accept: accept } }), proxyEnv, async (_input, init) => {
      signal = init?.signal;
      forwardedAccept = new Headers(init?.headers).get("Accept");
      return new Response(new ReadableStream({
        start(controller) { controller.enqueue(new TextEncoder().encode('{"records":[')); },
        cancel() { upstreamCancelled = true; },
      }), { headers: { "Content-Type": accept, Vary: "Accept" } });
    });
    expect(forwardedAccept).toBe(accept);
    expect(response.headers.get("Content-Type")).toBe(accept);
    expect(response.headers.get("Vary")).toBe("Accept");
    const reader = response.body!.getReader();
    expect(new TextDecoder().decode((await reader.read()).value)).toBe('{"records":[');
    await reader.cancel();
    expect(signal?.aborted).toBe(true);
    expect(upstreamCancelled).toBe(true);
  });

  it("forwards negotiated query-stream chunks and an incomplete terminal verbatim before upstream EOF", async () => {
    const prefix = '{"records":[{"data":{"id":"first"}}],';
    const terminal = '"error":{"code":"query_failed","message":"source read failed"},"complete":false}\n';
    const encoder = new TextEncoder();
    let source!: ReadableStreamDefaultController<Uint8Array>;
    let upstreamFinished = false;
    let forwardedAccept: string | null = null;
    const request = new Request(`${baseURL}/v1/databases/chinook/dtql`, {
      method: "POST",
      headers: {
        Accept: "application/vnd.openvaultdb.query-stream+json",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ query: "from: {name: Album}\nlimit: 2\n" }),
    });
    const response = await proxyChinook(request, proxyEnv, async (_input, init) => {
      forwardedAccept = new Headers(init?.headers).get("Accept");
      return new Response(new ReadableStream<Uint8Array>({
        start(controller) {
          source = controller;
          controller.enqueue(encoder.encode(prefix));
        },
        cancel() { upstreamFinished = true; },
      }), {
        status: 200,
        headers: {
          "Content-Type": "application/vnd.openvaultdb.query-stream+json",
          Vary: "Accept",
        },
      });
    });
    expect(forwardedAccept).toBe("application/vnd.openvaultdb.query-stream+json");
    expect(response.status).toBe(200);
    expect(response.headers.get("Content-Type")).toBe("application/vnd.openvaultdb.query-stream+json");
    expect(response.headers.get("Vary")).toBe("Accept");

    const reader = response.body!.getReader();
    const first = await reader.read();
    expect(new TextDecoder().decode(first.value)).toBe(prefix);
    expect(upstreamFinished).toBe(false);

    source.enqueue(encoder.encode(terminal));
    source.close();
    upstreamFinished = true;
    const last = await reader.read();
    expect(new TextDecoder().decode(last.value)).toBe(terminal);
    expect((await reader.read()).done).toBe(true);
    expect(prefix + terminal).toBe('{"records":[{"data":{"id":"first"}}],"error":{"code":"query_failed","message":"source read failed"},"complete":false}\n');
  });

  it.each([proxyChinook, futureProxy])("propagates request abort after upstream headers and clears pending read", async (proxy) => {
    const abort = new AbortController();
    let upstreamCancelled = false;
    let signal: AbortSignal | null | undefined;
    const response = await proxy(new Request(`${baseURL}/v1/databases/synthetic/query`, { signal: abort.signal }), proxyEnv, async (_input, init) => {
      signal = init?.signal;
      return new Response(new ReadableStream({ cancel() { upstreamCancelled = true; } }));
    });
    const pending = response.body!.getReader().read();
    abort.abort();
    await expect(pending).rejects.toThrow("OVDB upstream cancelled");
    expect(signal?.aborted).toBe(true);
    expect(upstreamCancelled).toBe(true);
  });

  it("bounds stalled headers and emits fixed diagnostics", async () => {
    vi.useFakeTimers();
    const log = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const pending = futureProxy(new Request(`${baseURL}/v1/synthetic-sensitive-path`), proxyEnv, async (_input, init) =>
        new Promise((_resolve, reject) => init!.signal!.addEventListener("abort", () => reject(new Error("SYNTHETIC_SECRET_ERROR")), { once: true })));
      await vi.advanceTimersByTimeAsync(NO_RETENTION_OVDB_TIMEOUT_MS);
      const response = await pending;
      expect(response.status).toBe(503);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
      expect(await response.text()).not.toContain("SYNTHETIC_SECRET_ERROR");
      expect(log).toHaveBeenCalledWith("Chinook OVDB upstream failed");
      expect(JSON.stringify(log.mock.calls)).not.toContain("synthetic-sensitive-path");
    } finally { log.mockRestore(); vi.useRealTimers(); }
  });

  it.each([proxyChinook, futureProxy])("cancels before upstream headers arrive", async (proxy) => {
    const abort = new AbortController();
    let begin!: () => void;
    const begun = new Promise<void>((resolve) => { begin = resolve; });
    const pending = proxy(new Request(`${baseURL}/v1/synthetic`, { signal: abort.signal }), proxyEnv, async (_input, init) => {
      begin();
      return new Promise((_resolve, reject) => init!.signal!.addEventListener("abort", () => reject(new Error("cancelled")), { once: true }));
    });
    await begun;
    abort.abort();
    expect((await pending).status).toBe(503);
  });

  it("bounds a stalled response body and cancels its upstream reader", async () => {
    vi.useFakeTimers();
    let cancelled = false;
    try {
      const response = await futureProxy(new Request(`${baseURL}/v1/synthetic`), proxyEnv, async () =>
        new Response(new ReadableStream({ cancel() { cancelled = true; } })));
      const pending = response.body!.getReader().read();
      const rejected = expect(pending).rejects.toThrow("OVDB upstream cancelled");
      await vi.advanceTimersByTimeAsync(NO_RETENTION_OVDB_TIMEOUT_MS);
      await rejected;
      expect(cancelled).toBe(true);
    } finally { vi.useRealTimers(); }
  });

  it.each([200, 400, 503])("scopes forced no-store to the future trusted branch for status %s", async (status) => {
    for (const [proxy, expected] of [[proxyChinook, null], [futureProxy, "no-store"]] as const) {
      const response = await proxy(new Request(`${baseURL}/v1/synthetic`), proxyEnv, async () => new Response(null, { status }));
      expect(response.headers.get("Cache-Control")).toBe(expected);
      expect(response.headers.get("Pragma")).toBe(expected === null ? null : "no-cache");
    }
  });

  it("current routing ignores caller policy hints and retains slow sample headers/stream behavior", async () => {
    vi.useFakeTimers();
    try {
      let begin!: () => void;
      const begun = new Promise<void>((resolve) => { begin = resolve; });
      let headersReady!: (value: Response) => void;
      let source!: ReadableStreamDefaultController<Uint8Array>;
      let upstreamSignal: AbortSignal | null | undefined;
      const sampleWorker = createWorker(async (_input, init) => {
        expect(Object.hasOwn(init!, "cache")).toBe(false);
        upstreamSignal = init?.signal;
        begin();
        return new Promise<Response>((resolve) => { headersReady = resolve; });
      });
      const pending = (sampleWorker.fetch as unknown as typeof fetchWorker)(new Request(
        `${baseURL}/v1/databases/chinook/dtql?retention=none&timeoutMs=1`, {
          headers: { "Cache-Control": "no-store", "OVDB-Retention": "none", "OVDB-Read-Profile": "ecb-daily/1" },
        }), { ...proxyEnv, OVDB_RETENTION_POLICY: "none" } as Env, createExecutionContext());
      await begun;
      expect(vi.getTimerCount()).toBe(0);
      await vi.advanceTimersByTimeAsync(NO_RETENTION_OVDB_TIMEOUT_MS * 2);
      expect(upstreamSignal?.aborted).toBe(false);
      headersReady(new Response(new ReadableStream<Uint8Array>({ start(controller) { source = controller; } }), {
        headers: { "Cache-Control": "public, max-age=86400, s-maxage=86400" },
      }));
      const response = await pending;
      expect(response.headers.get("Cache-Control")).toBe("public, max-age=86400, s-maxage=86400");
      const reader = response.body!.getReader();
      const read = reader.read();
      await vi.advanceTimersByTimeAsync(NO_RETENTION_OVDB_TIMEOUT_MS * 2);
      expect(upstreamSignal?.aborted).toBe(false);
      expect(vi.getTimerCount()).toBe(0);
      source.enqueue(new Uint8Array([1]));
      source.close();
      expect((await read).value).toEqual(new Uint8Array([1]));
      expect((await reader.read()).done).toBe(true);
    } finally { vi.useRealTimers(); }
  });

  // Synthetic transport contract; these pins do not describe hosted providers.
  const expectedPins = {
    "OVDB-Provider-Revision": "a".repeat(40),
    "OVDB-Source-SHA256": "b".repeat(64),
    "OVDB-Serving-SHA256": "c".repeat(64),
    "OVDB-Manifest-SHA256": "d".repeat(64),
  };
  const pinNames = Object.keys(expectedPins);
  const requestedHeaders = ["Content-Type", "OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close", ...pinNames].join(",");

  it.each(["valid", ...pinNames.map((name) => `missing:${name}`), ...pinNames.map((name) => `changed:${name}`), ...pinNames.map((name) => `empty:${name}`)])(
    "preserves independently checked request pins: %s", async (control) => {
      const requests: Request[] = [];
      const checkedWorker = createWorker(async (input, init) => {
        const request = new Request(input, init);
        requests.push(request);
        expect(init?.redirect).toBe("manual");
        const mismatch = Object.entries(expectedPins).find(([name, value]) => request.headers.get(name) !== value);
        return Response.json(mismatch ? { error: "pin_mismatch", pin: mismatch[0] } : { records: [{ key: "synthetic" }] }, {
          status: mismatch ? 409 : 200,
          headers: {
            ...expectedPins,
            "Access-Control-Allow-Origin": "https://datatug.app",
            "Access-Control-Expose-Headers": pinNames.join(","),
            Authorization: "Bearer upstream-secret", "Set-Cookie": "secret=1",
            "Access-Control-Allow-Credentials": "true",
          },
        });
      });
      const headers = new Headers({ ...expectedPins, "Content-Type": "application/json", Origin: "https://datatug.app", Authorization: "Bearer caller-secret", Cookie: "secret=1" });
      if (control.startsWith("missing:")) headers.delete(control.slice(8));
      if (control.startsWith("changed:")) headers.set(control.slice(8), "0".repeat(64));
      if (control.startsWith("empty:")) headers.set(control.slice(6), "");
      const response = await (checkedWorker.fetch as unknown as typeof fetchWorker)(new Request(`${baseURL}/v1/databases/chinook/dtql`, {
        method: "POST", headers, body: JSON.stringify({ query: "from: {name: Album}\nlimit: 1" }),
      }), { ...env, CHINOOK_RUN_ORIGIN: "https://chinook-ovdb.example.run.app" } as Env, createExecutionContext());
      expect(requests).toHaveLength(1);
      for (const name of pinNames) expect(requests[0].headers.get(name)).toBe(headers.get(name));
      expect(requests[0].headers.get("Authorization")).toBeNull();
      expect(requests[0].headers.get("Cookie")).toBeNull();
      expect(response.status).toBe(control === "valid" ? 200 : 409);
      const body = await response.json() as { records?: unknown[]; error?: string };
      if (control === "valid") expect(body.records).toHaveLength(1);
      else { expect(body.error).toBe("pin_mismatch"); expect(body.records).toBeUndefined(); }
      for (const [name, value] of Object.entries(expectedPins)) expect(response.headers.get(name)).toBe(value);
      expect(response.headers.get("Access-Control-Expose-Headers")?.split(",")).toEqual(pinNames);
      expect(response.headers.get("Authorization")).toBeNull();
      expect(response.headers.get("Set-Cookie")).toBeNull();
      expect(response.headers.get("Access-Control-Allow-Credentials")).toBeNull();
    },
  );

  it.each([...pinNames, "Access-Control-Expose-Headers"])("does not manufacture an absent upstream response header %s", async (missing) => {
    const headers = new Headers({ ...expectedPins, "Access-Control-Expose-Headers": pinNames.join(",") });
    headers.delete(missing);
    const upstreamWorker = createWorker(async () => Response.json({ records: [] }, { headers }));
    const response = await (upstreamWorker.fetch as unknown as typeof fetchWorker)(new Request(`${baseURL}/v1/databases/chinook/dtql`, {
      method: "POST", headers: expectedPins, body: "{}",
    }), { ...env, CHINOOK_RUN_ORIGIN: "https://chinook-ovdb.example.run.app" } as Env, createExecutionContext());
    for (const name of [...pinNames, "Access-Control-Expose-Headers"]) expect(response.headers.get(name)).toBe(headers.get(name));
    expect(response.headers.get(missing)).toBeNull();
  });

  it.each(pinNames)("preserves a changed upstream response pin %s for client rejection", async (changed) => {
    const headers = new Headers({ ...expectedPins, "Access-Control-Expose-Headers": pinNames.join(",") });
    headers.set(changed, "unexpected-serving-pin");
    const upstreamWorker = createWorker(async () => Response.json({ records: [] }, { headers }));
    const response = await (upstreamWorker.fetch as unknown as typeof fetchWorker)(new Request(`${baseURL}/v1/databases/chinook/dtql`, {
      method: "POST", headers: expectedPins, body: "{}",
    }), { ...env, CHINOOK_RUN_ORIGIN: "https://chinook-ovdb.example.run.app" } as Env, createExecutionContext());
    for (const name of pinNames) expect(response.headers.get(name)).toBe(headers.get(name));
    expect(response.headers.get(changed)).toBe("unexpected-serving-pin");
  });
  it.each(["https://datatug.app", "https://datatug.app.evil.example", "https://evil.datatug.app", "http://datatug.app", "https://datatug.app:444", "null"])("passes through OPTIONS without synthesizing CORS for %s", async (origin) => {
    const forwarded: Request[] = [];
    const allowed = origin === "https://datatug.app";
    const chinookWorker = createWorker(async (input, init) => {
      const request = new Request(input, init);
      forwarded.push(request);
      expect(init?.redirect).toBe("manual");
      return new Response(null, { status: 204, headers: {
        Vary: "Origin",
        ...(allowed ? {
          "Access-Control-Allow-Origin": origin,
          "Access-Control-Allow-Methods": "GET,HEAD,POST",
          "Access-Control-Allow-Headers": requestedHeaders,
          "Access-Control-Max-Age": "600",
        } : {}),
        "Set-Cookie": "must-not-leak=1",
      } });
    });
    const fetchChinook = chinookWorker.fetch as unknown as typeof fetchWorker;
    const response = await fetchChinook(new Request(`${baseURL}/v1/databases/chinook/dtql`, {
      method: "OPTIONS", headers: {
        Origin: origin, "Access-Control-Request-Method": "POST",
        "Access-Control-Request-Headers": requestedHeaders,
        Authorization: "Bearer must-not-forward", Cookie: "secret=1",
      },
    }), { ...env, CHINOOK_RUN_ORIGIN: "https://chinook-ovdb.example.run.app" } as Env, createExecutionContext());
    expect(response.status).toBe(204);
    expect(response.headers.get("Access-Control-Allow-Origin")).toBe(allowed ? origin : null);
    expect(response.headers.get("Access-Control-Allow-Headers")).toBe(allowed ? requestedHeaders : null);
    expect(response.headers.get("Access-Control-Allow-Methods")).toBe(allowed ? "GET,HEAD,POST" : null);
    expect(response.headers.get("Vary")).toBe("Origin");
    expect(response.headers.get("Set-Cookie")).toBeNull();
    expect(forwarded).toHaveLength(1);
    expect(forwarded[0].method).toBe("OPTIONS");
    expect(forwarded[0].headers.get("Origin")).toBe(origin);
    expect(forwarded[0].headers.get("Access-Control-Request-Method")).toBe("POST");
    expect(forwarded[0].headers.get("Access-Control-Request-Headers")).toBe(requestedHeaders);
    expect(forwarded[0].headers.get("Authorization")).toBeNull();
    expect(forwarded[0].headers.get("Cookie")).toBeNull();
  });

  it("returns an upstream redirect without following its untrusted location", async () => {
    let calls = 0;
    const chinookWorker = createWorker(async (_input, init) => {
      calls++;
      expect(init?.redirect).toBe("manual");
      return new Response(null, { status: 302, headers: { Location: "https://evil.example/", "Set-Cookie": "must-not-leak=1" } });
    });
    const fetchChinook = chinookWorker.fetch as unknown as typeof fetchWorker;
    const response = await fetchChinook(new Request(`${baseURL}/ovdb/dbs/chinook`, { headers: { Origin: "https://datatug.app" } }),
      { ...env, CHINOOK_RUN_ORIGIN: "https://chinook-ovdb.example.run.app" } as Env, createExecutionContext());
    expect(calls).toBe(1);
    expect(response.status).toBe(302);
    expect(response.headers.get("Location")).toBe("https://evil.example/");
    expect(response.headers.get("Access-Control-Allow-Origin")).toBeNull();
    expect(response.headers.get("Set-Cookie")).toBeNull();
  });

  it("forwards the generic profile and parameterized DTQL without caller credentials", async () => {
    const forwarded: Request[] = [];
    const chinookWorker = createWorker(async (input, init) => {
      const request = new Request(input, init);
      forwarded.push(request);
      return new Response(`{"records":[]}`, { headers: {
        "Content-Type": "application/json",
        "Access-Control-Allow-Origin": "https://chinookdb.com",
        "Cache-Control": request.method === "GET" && request.url.includes("/dtql?") ? "public, max-age=86400, s-maxage=86400" : "no-store",
        Vary: "Origin",
        "Set-Cookie": "should-not-leak=1",
      } });
    });
    const fetchChinook = chinookWorker.fetch as unknown as typeof fetchWorker;
    const chinookEnv = { ...env, CHINOOK_RUN_ORIGIN: "https://chinook-ovdb.example.run.app" } as Env;
    const response = await fetchChinook(new Request(`${baseURL}/v1/databases/chinook/dtql`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Origin: "https://chinookdb.com", Authorization: "Bearer must-not-forward", Cookie: "secret=1" },
      body: JSON.stringify({ query: "from: {name: Album}", parameters: {} }),
    }), chinookEnv, createExecutionContext());
    expect(response.status).toBe(200);
    expect(response.headers.get("Access-Control-Allow-Origin")).toBe("https://chinookdb.com");
    expect(response.headers.get("Set-Cookie")).toBeNull();
    expect(forwarded).toHaveLength(1);
    expect(forwarded[0].url).toBe("https://chinook-ovdb.example.run.app/v1/databases/chinook/dtql");
    expect(forwarded[0].headers.get("Authorization")).toBeNull();
    expect(forwarded[0].headers.get("Cookie")).toBeNull();
    expect(await forwarded[0].text()).toContain("Album");
    const profile = await fetchChinook(new Request(`${baseURL}/ovdb/dbs/chinook`), chinookEnv, createExecutionContext());
    expect(profile.status).toBe(200);
    expect(forwarded[1].url).toBe("https://chinook-ovdb.example.run.app/ovdb/dbs/chinook");
    const dtqlURL = `${baseURL}/v1/databases/chinook/dtql?` + new URLSearchParams({ q: "from: {name: Album}\n" });
    const get = await fetchChinook(new Request(dtqlURL), chinookEnv, createExecutionContext());
    expect(get.headers.get("Cache-Control")).toBe("public, max-age=86400, s-maxage=86400");
    expect(get.headers.get("Vary")).toBe("Origin");
    expect(forwarded[2].url).toBe(dtqlURL.replace(baseURL, "https://chinook-ovdb.example.run.app"));
  });

  it("forwards Retry-After of a capacity refusal and still drops Set-Cookie", async () => {
    const chinookWorker = createWorker(async () =>
      Response.json(
        { error: { code: "query_capacity" } },
        { status: 503, headers: { "Retry-After": "1", "Set-Cookie": "should-not-leak=1" } },
      ),
    );
    const fetchChinook = chinookWorker.fetch as unknown as typeof fetchWorker;
    const chinookEnv = { ...env, CHINOOK_RUN_ORIGIN: "https://chinook-ovdb.example.run.app" } as Env;
    const response = await fetchChinook(new Request(`${baseURL}/v1/dtql`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ query: "from: {database: chinook, name: Album}" }),
    }), chinookEnv, createExecutionContext());
    expect(response.status).toBe(503);
    expect(response.headers.get("Retry-After")).toBe("1");
    expect(response.headers.get("Set-Cookie")).toBeNull();
  });

  it("returns a service error until the Cloud Run origin is configured", async () => {
    const response = await call("/ovdb/dbs/chinook");
    expect(response.status).toBe(503);
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
