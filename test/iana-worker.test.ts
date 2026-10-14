import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { describe, expect, it, vi } from "vitest";
import { createWorker } from "../src/worker";

const publicURL = "https://cloud.openvaultdb.com/iana/v1/databases/iana-http-status/dtql";
const syntheticProxySecret = "synthetic-iana-proxy-secret-32-bytes-long";
const operatorToken = "synthetic-operator-token-32-bytes-long";
const ianaEnv = { ...env, IANA_ENABLED: "true", IANA_RUN_ORIGIN: "https://synthetic-iana.a.run.app",
  CHINOOK_RUN_ORIGIN: "https://synthetic-iana.a.run.app",
  IANA_PROXY_SECRET: syntheticProxySecret, IANA_OPERATOR_TOKEN: operatorToken } as Env;

function invoke(worker: ReturnType<typeof createWorker>, environment: Env, request: Request): Promise<Response> {
  return Promise.resolve((worker.fetch as (request: Request, env: Env, context: ExecutionContext) => Response | Promise<Response>)(
    request, environment, createExecutionContext(),
  ));
}

describe("selected IANA Worker route", () => {
  it("does not admit a candidate from origin or route input alone", async () => {
    const requests: Request[] = [];
    const worker = createWorker(async (input, init) => {
      requests.push(new Request(input, init));
      return Response.json({ records: [] });
    });
    const body = "from: {name: rows}\nlimit: 1\n";
    for (const environment of [env, { ...env, IANA_RUN_ORIGIN: "https://synthetic-iana.a.run.app" } as Env,
      { ...env, IANA_ENABLED: "true" } as Env,
      { ...env, IANA_ENABLED: "true", IANA_RUN_ORIGIN: "https://synthetic-iana.a.run.app" } as Env,
      { ...ianaEnv, IANA_PROXY_SECRET: "short" } as Env]) {
      const response = await invoke(worker, environment, new Request(publicURL, { method: "POST", body }));
      expect(response.status).toBe(503);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
    }
    expect(requests).toHaveLength(0);
  });

  it("refuses malformed origin, passive reads, alternate paths, and query strings before upstream I/O", async () => {
    const requests: Request[] = [];
    const worker = createWorker(async (input, init) => {
      requests.push(new Request(input, init));
      return Response.json({ records: [] });
    });
    for (const origin of ["http://synthetic-iana.a.run.app", "https://attacker.example", "https://synthetic-iana.a.run.app/path",
      "https://user:pass@synthetic-iana.a.run.app", "https://synthetic-iana.a.run.app:444",
      "https://synthetic-iana.a.run.app:443", "https://synthetic-iana.a.run.app/%2e%2e/"]) {
      const response = await invoke(worker, { ...ianaEnv, IANA_RUN_ORIGIN: origin, CHINOOK_RUN_ORIGIN: origin } as Env,
        new Request(publicURL, { method: "POST", body: "from: {name: rows}\nlimit: 1\n",
          headers: { "X-OVDB-IANA-Operator-Token": operatorToken } }));
      expect(response.status, origin).toBe(503);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
    }
    const mismatched = await invoke(worker, { ...ianaEnv, IANA_RUN_ORIGIN: "https://other-iana.a.run.app" } as Env,
      new Request(publicURL, { method: "POST", body: "from: {name: rows}\nlimit: 1\n",
        headers: { "X-OVDB-IANA-Operator-Token": operatorToken } }));
    expect(mismatched.status).toBe(503);
    for (const request of [new Request(publicURL), new Request(publicURL + "?q=marker", { method: "POST" }), new Request(publicURL + "?", { method: "POST" }),
      new Request(publicURL.replace("/dtql", "/records/rows/799"), { method: "POST" })]) {
      const response = await invoke(worker, ianaEnv, request);
      expect(response.status).not.toBe(200);
    }
    expect(requests).toHaveLength(0);
  });

  it("forwards only the explicit query to the fixed Go route with no-store and no cache writes", async () => {
    const cachePut = vi.spyOn((caches as CacheStorage & { default: Cache }).default, "put");
    try {
      const requests: Request[] = [];
      const worker = createWorker(async (input, init) => {
        expect(init?.cache).toBe("no-store");
        expect(init?.redirect).toBe("manual");
        const request = new Request(input, init);
        requests.push(request);
        return Response.json({ records: [{ Value: "799" }] }, {
          headers: { "Cache-Control": "public,max-age=86400", "Set-Cookie": "marker=1", "X-Private": "marker" },
        });
      });
      const body = "from: {name: rows}\nlimit: 1\n";
      const response = await invoke(worker, ianaEnv, new Request(publicURL, {
        method: "POST", body, headers: { "Content-Type": "text/plain", Authorization: "Bearer marker", Cookie: "marker=1",
          "X-OVDB-IANA-Proxy-Secret": "attacker-secret", "X-OVDB-IANA-Operator-Token": operatorToken },
      }));
      expect(requests).toHaveLength(1);
      expect(requests[0].url).toBe("https://synthetic-iana.a.run.app/v1/databases/iana-http-status/dtql");
      expect(requests[0].headers.get("Authorization")).toBeNull();
      expect(requests[0].headers.get("Cookie")).toBeNull();
      expect(requests[0].headers.get("X-OVDB-IANA-Proxy-Secret")).toBe(syntheticProxySecret);
      expect(requests[0].headers.get("X-OVDB-IANA-Operator-Token")).toBeNull();
      expect(await requests[0].text()).toBe(body);
      expect(response.status).toBe(200);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
      expect(response.headers.get("Set-Cookie")).toBeNull();
      expect(response.headers.get("X-Private")).toBeNull();
      await expect(response.json()).resolves.toMatchObject({ records: [{ Value: "799" }] });
      expect(cachePut).not.toHaveBeenCalled();
    } finally {
      cachePut.mockRestore();
    }
  });

  it("keeps transport errors and the outer catch marker-safe", async () => {
    const log = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const worker = createWorker(async () => { throw new Error("synthetic-private-error-marker"); });
      const response = await invoke(worker, ianaEnv,
        new Request(publicURL, { method: "POST", body: "from: {name: rows}\nlimit: 1\n",
          headers: { "X-OVDB-IANA-Operator-Token": operatorToken } }));
      expect(response.status).toBe(503);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
      expect(await response.text()).not.toContain("synthetic-private-error-marker");
      expect(log.mock.calls.flat().join(" ")).not.toContain("synthetic-private-error-marker");

      const throwingEnv = new Proxy(ianaEnv, {
        get(target, key, receiver) {
          if (key === "IANA_ENABLED") throw new Error("synthetic-private-panic-marker");
          return Reflect.get(target, key, receiver);
        },
      });
      const caught = await invoke(worker, throwingEnv,
        new Request(publicURL, { method: "POST", body: "synthetic-private-panic-marker" }));
      expect(caught.status).toBe(503);
      expect(caught.headers.get("Cache-Control")).toBe("no-store");
      expect(await caught.text()).not.toContain("synthetic-private-panic-marker");
      expect(log.mock.calls.flat().join(" ")).not.toContain("synthetic-private-panic-marker");
      expect(log.mock.calls.at(-1)).toEqual(["iana_worker_internal_error"]);
    } finally {
      log.mockRestore();
    }
  });
});

describe("IANA operator-only admission", () => {
  it("refuses absent, wrong, duplicated and unconfigured operator tokens before upstream I/O", async () => {
    let reads = 0;
    const worker = createWorker(async () => { reads++; return Response.json({ records: [] }); });
    for (const token of [undefined, "wrong", "valid-length-but-wrong-operator-token", `${operatorToken},${operatorToken}`]) {
      const headers = token ? { "X-OVDB-IANA-Operator-Token": token } : undefined;
      const response = await invoke(worker, ianaEnv, new Request(publicURL, {
        method: "POST", body: "from: {name: rows}\nlimit: 1\n", headers,
      }));
      expect(response.status).toBe(404);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
    }
    const absentBinding = await invoke(worker, { ...ianaEnv, IANA_OPERATOR_TOKEN: undefined } as Env,
      new Request(publicURL, { method: "POST", headers: { "X-OVDB-IANA-Operator-Token": operatorToken } }));
    expect(absentBinding.status).toBe(503);
    expect(reads).toBe(0);
  });
});
