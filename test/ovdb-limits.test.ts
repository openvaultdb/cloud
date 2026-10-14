import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { describe, expect, it } from "vitest";

import { DEFAULT_ORIGIN_SECRET_HEADER } from "../src/chinook";
import { isOVDBQueryPath, OVDB_PUBLIC_ALLOWED_ORIGINS } from "../src/ovdb-limits";
import { createWorker } from "../src/worker";

const baseURL = "https://cloud.openvaultdb.com";
const runOrigin = "https://chinook-ovdb.example.run.app";
const secret = "synthetic-origin-secret-0123456789abcdef";

function harness() {
  const upstream: Request[] = [];
  const worker = createWorker(async (input, init) => {
    upstream.push(new Request(input, init));
    return Response.json({ ok: true }, { headers: { "Access-Control-Allow-Origin": "https://datatug.app" } });
  });
  const fetchWorker = worker.fetch as unknown as (request: Request, environment: Env, context: ExecutionContext) => Promise<Response>;
  return { upstream, call: (request: Request, environment: Env) => fetchWorker(request, environment, createExecutionContext()) };
}

function limiter(success: boolean, keys: string[]): RateLimit {
  return { limit: async ({ key }: { key: string }) => { keys.push(key); return { success }; } } as unknown as RateLimit;
}

describe("public OVDB rate limits", () => {
  it("classifies query routes into the tighter bucket", () => {
    for (const path of ["/v1/dtql", "/v1/databases/chinook/dtql", "/v1/databases/chinook-postgresql/dtql", "/v1/databases/x/query", "/v1/databases/x/batch", "/v1/databases/x/read"]) {
      expect(isOVDBQueryPath(path), path).toBe(true);
    }
    for (const path of ["/v1/databases", "/v1/databases/chinook", "/v1/databases/x/records/a/1", "/v1/demodb/postgres-mounts", "/ovdb/dbs/chinook", "/.well-known/openvaultdb"]) {
      expect(isOVDBQueryPath(path), path).toBe(false);
    }
  });

  it("limits queries and reads per IP in separate buckets with a CORS-correct 429", async () => {
    const { upstream, call } = harness();
    const queryKeys: string[] = [];
    const readKeys: string[] = [];
    const limited = { ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_QUERY_LIMITER: limiter(false, queryKeys), OVDB_READ_LIMITER: limiter(false, readKeys) } as Env;
    const query = await call(new Request(`${baseURL}/v1/databases/chinook-postgresql/dtql`, {
      method: "POST",
      headers: { Origin: "https://datatug.app", "CF-Connecting-IP": "203.0.113.7", "Content-Type": "application/json" },
      body: JSON.stringify({ query: "from: {name: Album}" }),
    }), limited);
    expect(query.status).toBe(429);
    expect(query.headers.get("Access-Control-Allow-Origin")).toBe("https://datatug.app");
    expect(query.headers.get("Vary")).toBe("Origin");
    expect(query.headers.get("Retry-After")).toBe("60");
    expect(query.headers.get("Cache-Control")).toBe("no-store");
    expect(await query.json()).toEqual({ error: { code: "rate_limited", message: "Too many requests. Try again in one minute." } });
    expect(queryKeys).toEqual(["ovdb-query:203.0.113.7"]);

    const read = await call(new Request(`${baseURL}/v1/databases/chinook`, { headers: { Origin: "https://evil.example", "CF-Connecting-IP": "203.0.113.7" } }), limited);
    expect(read.status).toBe(429);
    expect(read.headers.get("Access-Control-Allow-Origin")).toBeNull();
    expect(read.headers.get("Vary")).toBe("Origin");
    expect(readKeys).toEqual(["ovdb-read:203.0.113.7"]);

    for (const origin of OVDB_PUBLIC_ALLOWED_ORIGINS) {
      const response = await call(new Request(`${baseURL}/ovdb/dbs/chinook`, { headers: { Origin: origin } }), limited);
      expect(response.headers.get("Access-Control-Allow-Origin")).toBe(origin);
    }
    expect(upstream).toHaveLength(0);
  });

  it("never limits preflight and proceeds when the bucket has room", async () => {
    const { upstream, call } = harness();
    const keys: string[] = [];
    const allowEnv = { ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_QUERY_LIMITER: limiter(true, keys), OVDB_READ_LIMITER: limiter(false, keys) } as Env;
    const preflight = await call(new Request(`${baseURL}/v1/databases/chinook/dtql`, {
      method: "OPTIONS",
      headers: { Origin: "https://datatug.app", "Access-Control-Request-Method": "POST" },
    }), allowEnv);
    expect(preflight.status).toBe(200);
    expect(keys).toHaveLength(0);
    const query = await call(new Request(`${baseURL}/v1/databases/chinook/dtql`, { method: "POST", body: "{}" }), allowEnv);
    expect(query.status).toBe(200);
    expect(keys).toEqual(["ovdb-query:local-or-unknown"]);
    expect(upstream.map((request) => request.method)).toEqual(["OPTIONS", "POST"]);
  });
});

describe("Cloud Run origin secret", () => {
  it("adds nothing when unbound, and never forwards a client-supplied secret header", async () => {
    const { upstream, call } = harness();
    const response = await call(new Request(`${baseURL}/v1/databases/chinook`, { headers: { [DEFAULT_ORIGIN_SECRET_HEADER]: "client-guess" } }), { ...env, CHINOOK_RUN_ORIGIN: runOrigin } as Env);
    expect(response.status).toBe(200);
    expect(upstream[0]?.headers.get(DEFAULT_ORIGIN_SECRET_HEADER)).toBeNull();
  });

  it("forwards the bound secret in the default or configured header", async () => {
    for (const [headerName, expected] of [[undefined, DEFAULT_ORIGIN_SECRET_HEADER], ["X-Demo-Origin", "X-Demo-Origin"]] as const) {
      const { upstream, call } = harness();
      const response = await call(new Request(`${baseURL}/v1/databases/chinook/dtql`, { method: "POST", body: "{}" }), {
        ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_ORIGIN_SECRET: secret, ...(headerName ? { OVDB_ORIGIN_SECRET_HEADER: headerName } : {}),
      } as Env);
      expect(response.status).toBe(200);
      expect(upstream[0]?.headers.get(expected)).toBe(secret);
      expect(response.headers.get(expected)).toBeNull();
    }
  });

  it("refuses to forward with an invalid secret or header name", async () => {
    for (const override of [{ OVDB_ORIGIN_SECRET: "short" }, { OVDB_ORIGIN_SECRET: secret, OVDB_ORIGIN_SECRET_HEADER: "Authorization" }, { OVDB_ORIGIN_SECRET: secret, OVDB_ORIGIN_SECRET_HEADER: "X-OVDB-ECB-Proxy-Secret" }]) {
      const { upstream, call } = harness();
      const response = await call(new Request(`${baseURL}/v1/databases/chinook`), { ...env, CHINOOK_RUN_ORIGIN: runOrigin, ...override } as unknown as Env);
      expect(response.status).toBe(503);
      expect(await response.text()).not.toContain(secret);
      expect(upstream).toHaveLength(0);
    }
  });
});
