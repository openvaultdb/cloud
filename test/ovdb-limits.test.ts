import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { describe, expect, it, vi } from "vitest";

import { DEFAULT_ORIGIN_SECRET_HEADER } from "../src/chinook";
import { canonicalOVDBPath, isOVDBQueryPath, OVDB_PUBLIC_ALLOWED_ORIGINS, rateLimitActor, resetMissingLimiterWarnings } from "../src/ovdb-limits";
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

  it("limits preflight in its own bucket and proceeds when a bucket has room", async () => {
    const { upstream, call } = harness();
    const keys: string[] = [];
    const allowEnv = { ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_QUERY_LIMITER: limiter(true, keys), OVDB_READ_LIMITER: limiter(false, keys), OVDB_PREFLIGHT_LIMITER: limiter(true, keys) } as Env;
    const preflight = await call(new Request(`${baseURL}/v1/databases/chinook/dtql`, {
      method: "OPTIONS",
      headers: { Origin: "https://datatug.app", "Access-Control-Request-Method": "POST" },
    }), allowEnv);
    expect(preflight.status).toBe(200);
    const query = await call(new Request(`${baseURL}/v1/databases/chinook/dtql`, { method: "POST", body: "{}" }), allowEnv);
    expect(query.status).toBe(200);
    expect(keys).toEqual(["ovdb-preflight:local-or-unknown", "ovdb-query:local-or-unknown"]);
    expect(upstream.map((request) => request.method)).toEqual(["OPTIONS", "POST"]);

    const preflightKeys: string[] = [];
    const exhausted = { ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_PREFLIGHT_LIMITER: limiter(false, preflightKeys) } as Env;
    const limited = await call(new Request(`${baseURL}/v1/databases/chinook/dtql`, {
      method: "OPTIONS",
      headers: { Origin: "https://datatug.app", "Access-Control-Request-Method": "POST", "CF-Connecting-IP": "198.51.100.9" },
    }), exhausted);
    expect(limited.status).toBe(429);
    expect(limited.headers.get("Access-Control-Allow-Origin")).toBe("https://datatug.app");
    expect(preflightKeys).toEqual(["ovdb-preflight:198.51.100.9"]);
    expect(upstream).toHaveLength(2);
  });

  it("keys IPv6 clients by their /64 prefix", async () => {
    expect(rateLimitActor("2001:db8:1:2:aaaa:bbbb:cccc:dddd")).toBe("2001:db8:1:2::/64");
    expect(rateLimitActor("2001:db8:1:2::1")).toBe("2001:db8:1:2::/64");
    expect(rateLimitActor("2001:0DB8:0001:0002:ffff::9")).toBe("2001:db8:1:2::/64");
    expect(rateLimitActor("2001:db8::1")).toBe("2001:db8:0:0::/64");
    expect(rateLimitActor("::1")).toBe("0:0:0:0::/64");
    expect(rateLimitActor("::ffff:192.0.2.1")).toBe("192.0.2.1");
    expect(rateLimitActor("192.0.2.1")).toBe("192.0.2.1");
    expect(rateLimitActor(null)).toBe("local-or-unknown");
    const { call } = harness();
    const keys: string[] = [];
    const limited = { ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_READ_LIMITER: limiter(false, keys) } as Env;
    for (const ip of ["2001:db8:1:2::1", "2001:db8:1:2:ffff::2"]) {
      await call(new Request(`${baseURL}/v1/databases/chinook`, { headers: { "CF-Connecting-IP": ip } }), limited);
    }
    expect(keys).toEqual(["ovdb-read:2001:db8:1:2::/64", "ovdb-read:2001:db8:1:2::/64"]);
  });

  it("fails open on a missing binding but warns once per isolate", async () => {
    resetMissingLimiterWarnings();
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    try {
      const { upstream, call } = harness();
      const unbound = { ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_QUERY_LIMITER: undefined } as unknown as Env;
      for (let i = 0; i < 3; i++) {
        const response = await call(new Request(`${baseURL}/v1/dtql`, { method: "POST", body: "{}" }), unbound);
        expect(response.status).toBe(200);
      }
      expect(upstream).toHaveLength(3);
      expect(warn).toHaveBeenCalledTimes(1);
      expect(String(warn.mock.calls[0]?.[0])).toBe("OVDB_QUERY_LIMITER binding is missing: public OVDB requests in this bucket are not rate limited");
    } finally {
      warn.mockRestore();
    }
  });
});

describe("canonical OVDB paths", () => {
  const queryCases: [string, string][] = [
    ["/v1/databases/chinook/d%74ql", "/v1/databases/chinook/dtql"],
    ["/v1/%64tql", "/v1/dtql"],
    ["/v1/databases/chinook/d%74%71l", "/v1/databases/chinook/dtql"],
    ["/v1/databases/chinook/dtql/", "/v1/databases/chinook/dtql/"],
    ["/v1/dtql", "/v1/dtql"],
    ["/v1/dtql/", "/v1/dtql/"],
    ["/%76%31/dtql", "/v1/dtql"],
    ["/v1/databases/chinook/%71uery", "/v1/databases/chinook/query"],
    ["/v1/databases/chinook/b%61tch", "/v1/databases/chinook/batch"],
    ["/v1/databases/chinook/r%65ad", "/v1/databases/chinook/read"],
    ["/v1/databases/c%68inook-postgresql/dtql", "/v1/databases/chinook-postgresql/dtql"],
  ];
  const readCases: [string, string][] = [
    ["/v1/databases/chinook", "/v1/databases/chinook"],
    ["/v1/databases/chinook/d%2574ql", "/v1/databases/chinook/d%2574ql"], // double encoding stays one escape
    ["/v1/databases/chinook/records/Album/%7a", "/v1/databases/chinook/records/Album/z"], // lower-case hex
    ["/ovdb/dbs/northwind/collections/Order%20Details", "/ovdb/dbs/northwind/collections/Order%20Details"],
    ["/ovdb/dbs/northwind/collections/Order%2cDetails", "/ovdb/dbs/northwind/collections/Order%2CDetails"],
  ];
  const refused = [
    "/v1/databases/chinook%2fdtql", "/v1/databases/chinook/d%2Ftql", "/v1%2Fdtql", "/v1/databases/chinook/%2e%2e/dtql",
    "/v1/databases/chinook/%2E/dtql", "/v1/databases/chinook%5cdtql", "/v1//dtql", "/v1/databases//dtql",
    "/v1/databases/chinook/dtql;x=1", "/v1/dtql;", "/v1/databases/chinook/d%7", "/v1/databases/chinook/d%zzql",
  ];

  it("canonicalises, classifies, and forwards one and the same path", async () => {
    for (const [raw, canonical] of [...queryCases, ...readCases]) {
      expect(canonicalOVDBPath(raw), raw).toBe(canonical);
    }
    for (const raw of refused) expect(canonicalOVDBPath(raw), raw).toBeNull();

    for (const method of ["GET", "HEAD", "POST"]) {
      for (const [raw, canonical] of [...queryCases, ...readCases]) {
        const { upstream, call } = harness();
        const keys: string[] = [];
        const counting = { ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_QUERY_LIMITER: limiter(true, keys), OVDB_READ_LIMITER: limiter(true, keys) } as Env;
        const response = await call(new Request(`${baseURL}${raw}?q=1`, { method, ...(method === "POST" ? { body: "{}" } : {}) }), counting);
        expect(response.status, `${method} ${raw}`).toBe(200);
        const bucket = queryCases.some(([path]) => path === raw) ? "ovdb-query" : "ovdb-read";
        expect(keys, `${method} ${raw}`).toEqual([`${bucket}:local-or-unknown`]);
        expect(isOVDBQueryPath(canonical), canonical).toBe(bucket === "ovdb-query");
        const forwarded = new URL(upstream[0]!.url);
        expect(forwarded.pathname, `${method} ${raw}`).toBe(canonical);
        expect(forwarded.search).toBe("?q=1");
        expect(upstream[0]!.method).toBe(method);
      }
    }
  });

  it("refuses non-canonical OVDB paths with 400 before limiting or forwarding", async () => {
    // WHATWG URL parsing resolves %2e/%2e%2e dot segments before the Worker sees
    // them; the rest are refused by canonicalOVDBPath.
    const resolvedByURL = new Map([
      ["/v1/databases/chinook/%2e%2e/dtql", ["/v1/databases/dtql", "ovdb-read"]],
      ["/v1/databases/chinook/%2E/dtql", ["/v1/databases/chinook/dtql", "ovdb-query"]],
    ]);
    for (const raw of refused) {
      const { upstream, call } = harness();
      const keys: string[] = [];
      const counting = { ...env, CHINOOK_RUN_ORIGIN: runOrigin, OVDB_QUERY_LIMITER: limiter(true, keys), OVDB_READ_LIMITER: limiter(true, keys) } as Env;
      const response = await call(new Request(`${baseURL}${raw}`, { method: "POST", body: "{}" }), counting);
      const resolved = resolvedByURL.get(raw);
      if (resolved) {
        expect(response.status, raw).toBe(200);
        expect(new URL(upstream[0]!.url).pathname, raw).toBe(resolved[0]);
        expect(keys, raw).toEqual([`${resolved[1]}:local-or-unknown`]);
        continue;
      }
      expect(response.status, raw).toBe(400);
      expect(await response.json()).toEqual({ error: { code: "invalid_path", message: "Request path is not in canonical form." } });
      expect(upstream, raw).toHaveLength(0);
      expect(keys, raw).toHaveLength(0);
    }
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
