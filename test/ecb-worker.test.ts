import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { describe, expect, it, vi } from "vitest";
import { createWorker } from "../src/worker";

const publicURL = "https://cloud.openvaultdb.com/ecb/v1/databases/ecb/dtql";
const syntheticProxySecret = "synthetic-ecb-proxy-secret-32-bytes-long";
const ecbEnv = { ...env, ECB_ENABLED: "true", ECB_RUN_ORIGIN: "https://synthetic-ecb.a.run.app",
  CHINOOK_RUN_ORIGIN: "https://synthetic-ecb.a.run.app",
  ECB_PROXY_SECRET: syntheticProxySecret } as Env;

function invoke(worker: ReturnType<typeof createWorker>, environment: Env, request: Request): Promise<Response> {
  return Promise.resolve((worker.fetch as (request: Request, env: Env, context: ExecutionContext) => Response | Promise<Response>)(
    request, environment, createExecutionContext(),
  ));
}

describe("selected ECB Worker route", () => {
  it("does not admit a candidate from origin or route input alone", async () => {
    const requests: Request[] = [];
    const worker = createWorker(async (input, init) => {
      requests.push(new Request(input, init));
      return Response.json({ records: [] });
    });
    const body = "from: {name: daily}\nlimit: 1\n";
    for (const environment of [env, { ...env, ECB_RUN_ORIGIN: "https://synthetic-ecb.a.run.app" } as Env,
      { ...env, ECB_ENABLED: "true" } as Env,
      { ...env, ECB_ENABLED: "true", ECB_RUN_ORIGIN: "https://synthetic-ecb.a.run.app" } as Env,
      { ...ecbEnv, ECB_PROXY_SECRET: "short" } as Env]) {
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
    for (const origin of ["http://synthetic-ecb.a.run.app", "https://attacker.example", "https://synthetic-ecb.a.run.app/path",
      "https://user:pass@synthetic-ecb.a.run.app", "https://synthetic-ecb.a.run.app:444"]) {
      const response = await invoke(worker, { ...ecbEnv, ECB_RUN_ORIGIN: origin, CHINOOK_RUN_ORIGIN: origin } as Env,
        new Request(publicURL, { method: "POST", body: "from: {name: daily}\nlimit: 1\n" }));
      expect(response.status).toBe(503);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
    }
    const mismatched = await invoke(worker, { ...ecbEnv, ECB_RUN_ORIGIN: "https://other-ecb.a.run.app" } as Env,
      new Request(publicURL, { method: "POST", body: "from: {name: daily}\nlimit: 1\n" }));
    expect(mismatched.status).toBe(503);
    for (const request of [new Request(publicURL), new Request(publicURL + "?q=marker", { method: "POST" }),
      new Request(publicURL.replace("/dtql", "/records/daily/USD"), { method: "POST" })]) {
      const response = await invoke(worker, ecbEnv, request);
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
        return Response.json({ records: [{ currency: "SYN" }] }, {
          headers: { "Cache-Control": "public,max-age=86400", "Set-Cookie": "marker=1", "X-Private": "marker" },
        });
      });
      const body = "from: {name: daily}\nlimit: 1\n";
      const response = await invoke(worker, ecbEnv, new Request(publicURL, {
        method: "POST", body, headers: { "Content-Type": "text/plain", Authorization: "Bearer marker", Cookie: "marker=1",
          "X-OVDB-ECB-Proxy-Secret": "attacker-secret" },
      }));
      expect(requests).toHaveLength(1);
      expect(requests[0].url).toBe("https://synthetic-ecb.a.run.app/v1/databases/ecb/dtql");
      expect(requests[0].headers.get("Authorization")).toBeNull();
      expect(requests[0].headers.get("Cookie")).toBeNull();
      expect(requests[0].headers.get("X-OVDB-ECB-Proxy-Secret")).toBe(syntheticProxySecret);
      expect(await requests[0].text()).toBe(body);
      expect(response.status).toBe(200);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
      expect(response.headers.get("Set-Cookie")).toBeNull();
      expect(response.headers.get("X-Private")).toBeNull();
      await expect(response.json()).resolves.toMatchObject({ records: [{ currency: "SYN" }] });
      expect(cachePut).not.toHaveBeenCalled();
    } finally {
      cachePut.mockRestore();
    }
  });

  it("keeps transport errors and the outer catch marker-safe", async () => {
    const log = vi.spyOn(console, "error").mockImplementation(() => {});
    try {
      const worker = createWorker(async () => { throw new Error("synthetic-private-error-marker"); });
      const response = await invoke(worker, ecbEnv,
        new Request(publicURL, { method: "POST", body: "from: {name: daily}\nlimit: 1\n" }));
      expect(response.status).toBe(503);
      expect(response.headers.get("Cache-Control")).toBe("no-store");
      expect(await response.text()).not.toContain("synthetic-private-error-marker");
      expect(log.mock.calls.flat().join(" ")).not.toContain("synthetic-private-error-marker");

      const throwingEnv = new Proxy(ecbEnv, {
        get(target, key, receiver) {
          if (key === "ECB_ENABLED") throw new Error("synthetic-private-panic-marker");
          return Reflect.get(target, key, receiver);
        },
      });
      const caught = await invoke(worker, throwingEnv,
        new Request(publicURL, { method: "POST", body: "synthetic-private-panic-marker" }));
      expect(caught.status).toBe(500);
      expect(caught.headers.get("Cache-Control")).toBe("no-store");
      expect(await caught.text()).not.toContain("synthetic-private-panic-marker");
      expect(log.mock.calls.flat().join(" ")).not.toContain("synthetic-private-panic-marker");
      expect(log.mock.calls.at(-1)).toEqual(["ecb_worker_internal_error"]);
    } finally {
      log.mockRestore();
    }
  });
});
