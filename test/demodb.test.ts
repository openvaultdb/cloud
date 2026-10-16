import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { describe, expect, it } from "vitest";

import { DEFAULT_ORIGIN_SECRET_HEADER } from "../src/chinook";
import demodbOVDB from "../src/demodb-ovdb.json";
import { createWorker } from "../src/worker";
import { referenceDemoDB } from "./fixtures/demodb-websites-reference";

const demodb = "https://demodb.dev";
const apiHost = "https://ovdb.demodb.dev";
const cloud = "https://cloud.openvaultdb.com";
const runOrigin = "https://chinook-ovdb.example.run.app";
const secret = "synthetic-origin-secret-0123456789abcdef";
const MiB = 1024 * 1024;

type Mode = "ok" | "redirect" | "error" | "throw" | "bad-metadata" | "wrong-endpoint";
type Recorded = { method: string; url: string; headers: [string, string][]; body: string };

/** A fake Cloud Run service, a cloud Worker in front of it, and both ways a client reaches it. */
function harness(options: { mode?: Mode; limited?: boolean; configured?: boolean } = {}) {
  const mode = options.mode ?? "ok";
  const cloudRun: Recorded[] = [];
  const limiterKeys: string[] = [];
  const worker = createWorker(async (input, init) => {
    const request = new Request(input, init);
    cloudRun.push({
      method: request.method,
      url: request.url,
      headers: [...request.headers].sort(([a], [b]) => a.localeCompare(b)),
      body: request.method === "GET" || request.method === "HEAD" ? "" : await request.text(),
    });
    if (mode === "throw") throw new Error("connect failed token=abc");
    if (mode === "redirect") return new Response(null, { status: 302, headers: { Location: "https://elsewhere.example/" } });
    if (mode === "error") return Response.json({ error: "boom" }, { status: 500, headers: { "Cache-Control": "no-store" } });
    const path = new URL(request.url).pathname;
    const database = /^\/v1\/databases\/([^/]+)$/u.exec(path)?.[1];
    const upstreamHeaders = {
      "Cache-Control": "public, max-age=60",
      ETag: '"v1"',
      "Last-Modified": "Fri, 16 Oct 2026 00:00:00 GMT",
      Link: "<https://example.com/next>; rel=next",
      Vary: "Accept",
      "OVDB-Page-Token": "next-page",
      "Access-Control-Allow-Origin": "https://datatug.app",
      "Set-Cookie": "upstream=1",
    };
    if (database) {
      const dtql = mode === "wrong-endpoint" ? "https://evil.example/dtql" : `${cloud}/v1/databases/${database}/dtql`;
      return Response.json(mode === "bad-metadata" ? { id: "other" } : { id: database, title: database, endpoints: { dtql } }, { headers: upstreamHeaders });
    }
    return Response.json({ path, method: request.method }, { headers: upstreamHeaders });
  });
  const fetchWorker = worker.fetch as unknown as (request: Request, environment: Env, context: ExecutionContext) => Promise<Response>;
  const limiter = { limit: async ({ key }: { key: string }) => { limiterKeys.push(key); return { success: !options.limited }; } } as unknown as RateLimit;
  const environment = {
    ...env,
    ...(options.configured === false ? {} : { CHINOOK_RUN_ORIGIN: runOrigin }),
    OVDB_ORIGIN_SECRET: secret,
    OVDB_QUERY_LIMITER: limiter,
    OVDB_READ_LIMITER: limiter,
    OVDB_PREFLIGHT_LIMITER: limiter,
  } as unknown as Env;
  const call = (request: Request) => fetchWorker(request, environment, createExecutionContext());
  return {
    cloudRun,
    limiterKeys,
    /** After: ovdb.demodb.dev/v1/... served by this Worker directly. */
    oneHop: (request: Request) => call(request),
    /** Before: demodb.dev/ovdb/v1/... handled by demodb-websites, which re-fetched cloud.openvaultdb.com. */
    twoHop: (request: Request) => {
      const url = new URL(request.url);
      return referenceDemoDB(new Request(`${demodb}/ovdb${url.pathname}${url.search}`, request), (input, init) => call(new Request(input, init)));
    },
  };
}

/** `former` maps the URLs demodb.dev/ovdb/v1 advertised to the ones ovdb.demodb.dev/v1 advertises (by design). */
async function snapshot(response: Response, former = false) {
  const body = await response.text();
  return {
    status: response.status,
    headers: [...response.headers].sort(([a], [b]) => a.localeCompare(b)),
    body: former ? body.replaceAll(`${demodb}/ovdb/v1`, `${apiHost}/v1`) : body,
  };
}

const paths = [
  "/", "/ovdb/v1", "/ovdb/v1/databases/chinook/dtql",
  "/v1", "/v1/", "/v1x", "/v1.json", "/v1/databases", "/v1/databases/",
  "/v1/databases/chinook", "/v1/databases/chinook/", "/v1/databases/employees",
  "/v1/databases/unknown", "/v1/databases/unknown/dtql", "/v1/databases/Chinook/dtql",
  "/v1/databases/chinook/dtql", "/v1/databases/chinook/dtql?limit=5&x=%20y", "/v1/databases/northwind/query",
  "/v1/databases/chinook/read", "/v1/databases/chinook/inferred-schema", "/v1/databases/chinook/batch",
  "/v1/databases/chinook/records/Album/1", "/v1/databases/chinook/records/Album", "/v1/databases/chinook/records/A%20B/1/2",
  "/v1/databases/chin%6Fok/dtql", "/v1/databases/chinook/records/Album/%2F", "/v1/databases/chinook/records/Album/%zz",
  "/v1/databases/chinook/records/Album/%2e%2e", "/v1/databases/chinook/records/Album/%00", "/v1/databases/chinook/records/A%2541/1",
  "/v1/databases/chinook/records/A;b/1", "/v1/databases/chinook//dtql",
];
const methods = ["GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "PURGE"];
const clientHeaders = {
  Accept: "application/x-ndjson",
  "Content-Type": "application/json",
  "OVDB-Page-Size": "10",
  "OVDB-Page-Token": "page-2",
  "OVDB-Page-Close": "1",
  Origin: "https://datatug.app",
  Authorization: "Bearer client-secret",
  Cookie: "a=b",
  [DEFAULT_ORIGIN_SECRET_HEADER]: "client-guess",
};

function request(path: string, method: string, init: RequestInit = {}): Request {
  const hasBody = method !== "GET" && method !== "HEAD";
  return new Request(`${apiHost}${path}`, { method, headers: clientHeaders, ...(hasBody ? { body: '{"dtql":"FROM Album"}' } : {}), ...init });
}

describe("ovdb.demodb.dev/v1 matches the former demodb.dev/ovdb/v1 two-Worker chain", () => {
  it("returns the same response and sends Cloud Run the same request for every path, method and backend outcome", async () => {
    let compared = 0;
    for (const mode of ["ok", "redirect", "error", "throw", "bad-metadata", "wrong-endpoint"] as const) {
      for (const path of paths) {
        for (const method of methods) {
          const before = harness({ mode });
          const after = harness({ mode });
          const expected = await snapshot(await before.twoHop(request(path, method)), true);
          const actual = await snapshot(await after.oneHop(request(path, method)));
          expect(actual, `${mode} ${method} ${path}`).toEqual(expected);
          expect(after.cloudRun, `${mode} ${method} ${path}`).toEqual(before.cloudRun);
          expect(after.limiterKeys, `${mode} ${method} ${path}`).toEqual(before.limiterKeys);
          expect(after.cloudRun.length, `${mode} ${method} ${path}`).toBeLessThanOrEqual(1);
          compared++;
        }
      }
    }
    expect(compared).toBe(6 * paths.length * methods.length);
  });

  it("matches when rate limited and when the Cloud Run origin is not configured", async () => {
    for (const options of [{ limited: true }, { configured: false }]) {
      for (const path of ["/v1/databases/chinook", "/v1/databases/chinook/dtql", "/v1/databases/chinook/records/Album/1"]) {
        for (const method of ["GET", "POST", "OPTIONS"]) {
          const expected = await snapshot(await harness(options).twoHop(request(path, method)), true);
          const actual = await snapshot(await harness(options).oneHop(request(path, method)));
          expect(actual, `${JSON.stringify(options)} ${method} ${path}`).toEqual(expected);
        }
      }
    }
  });

  it("matches for oversized query bodies, declared or streamed", async () => {
    const big = "x".repeat(MiB + 1);
    const streamed = () => new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new Uint8Array(MiB));
        controller.enqueue(new Uint8Array(1));
        controller.close();
      },
    });
    for (const make of [
      () => request("/v1/databases/chinook/dtql", "POST", { body: big }),
      () => request("/v1/databases/chinook/dtql", "POST", { body: streamed() }),
      () => request("/v1/databases/chinook/dtql", "POST", { body: "x".repeat(MiB) }),
    ]) {
      const before = harness();
      const after = harness();
      const expected = await snapshot(await before.twoHop(make()), true);
      const actual = await snapshot(await after.oneHop(make()));
      expect(actual).toEqual(expected);
      expect(after.cloudRun.map((item) => item.body.length)).toEqual(before.cloudRun.map((item) => item.body.length));
    }
  });
});

describe("ovdb.demodb.dev/v1 behaviour", () => {
  it("serves the server index from the bundled allowlist", async () => {
    const { oneHop, cloudRun } = harness();
    for (const path of ["/v1", "/v1/"]) {
      const response = await oneHop(request(path, "GET"));
      expect(response.status).toBe(200);
      expect(response.headers.get("Content-Type")).toBe("application/json; charset=utf-8");
      expect(response.headers.get("Cache-Control")).toBe("public, max-age=300");
      expect(response.headers.get("Access-Control-Allow-Origin")).toBe("*");
      expect(response.headers.get("Access-Control-Allow-Methods")).toBe("GET, HEAD, OPTIONS");
      const index = await response.json() as typeof demodbOVDB.server;
      expect(index).toEqual({
        ...demodbOVDB.server,
        apiUrl: `${apiHost}/v1`,
        databases: demodbOVDB.server.databases.map((database) => ({ ...database, apiUrl: `${apiHost}/v1/databases/${database.localId}` })),
      });
      expect(index.databases.map((database) => database.localId)).toEqual(["chinook", "northwind", "pubs", "sakila", "adventureworks", "employees"]);
    }
    const head = await oneHop(request("/v1", "HEAD"));
    expect(await head.text()).toBe("");
    const post = await oneHop(request("/v1", "POST"));
    expect(post.status).toBe(405);
    expect(post.headers.get("Allow")).toBe("GET, HEAD, OPTIONS");
    expect(cloudRun).toHaveLength(0);
  });

  it("answers CORS preflights itself with the OVDB paging headers allowed", async () => {
    const { oneHop, cloudRun, limiterKeys } = harness();
    const query = await oneHop(request("/v1/databases/chinook/dtql", "OPTIONS", { body: null }));
    expect(query.status).toBe(204);
    expect(query.headers.get("Access-Control-Allow-Origin")).toBe("*");
    expect(query.headers.get("Access-Control-Allow-Methods")).toBe("GET, HEAD, POST, OPTIONS");
    expect(query.headers.get("Access-Control-Allow-Headers")).toBe("Content-Type, Accept, OVDB-Page-Size, OVDB-Page-Token, OVDB-Page-Close");
    expect(query.headers.get("Access-Control-Max-Age")).toBe("86400");
    const read = await oneHop(request("/v1/databases/chinook/records/Album/1", "OPTIONS", { body: null }));
    expect(read.status).toBe(204);
    expect(read.headers.get("Access-Control-Allow-Methods")).toBe("GET, HEAD, OPTIONS");
    const index = await oneHop(request("/v1/databases/chinook", "OPTIONS", { body: null }));
    expect(index.status).toBe(204);
    expect(index.headers.get("Access-Control-Allow-Headers")).toBe("Content-Type");
    expect(cloudRun).toHaveLength(0);
    expect(limiterKeys).toHaveLength(0);
  });

  it("is read-only: writes get 403 with Allow, other methods 405", async () => {
    const { oneHop, cloudRun } = harness();
    for (const [path, method, status, allow] of [
      ["/v1/databases/chinook/records/Album/1", "POST", 403, "GET, HEAD, OPTIONS"],
      ["/v1/databases/chinook/dtql", "PUT", 403, "GET, HEAD, POST, OPTIONS"],
      ["/v1/databases/chinook/dtql", "DELETE", 403, "GET, HEAD, POST, OPTIONS"],
      ["/v1/databases/chinook", "PATCH", 403, "GET, HEAD, OPTIONS"],
      ["/v1/databases/chinook/dtql", "PURGE", 405, "GET, HEAD, POST, OPTIONS"],
      ["/v1/databases/chinook", "PURGE", 405, "GET, HEAD, OPTIONS"],
    ] as const) {
      const response = await oneHop(request(path, method));
      expect(response.status, `${method} ${path}`).toBe(status);
      expect(response.headers.get("Allow"), `${method} ${path}`).toBe(allow);
      expect(response.headers.get("Access-Control-Allow-Origin")).toBe("*");
    }
    expect(cloudRun).toHaveLength(0);
  });

  it("refuses query bodies over 1 MiB with 413 and forwards one of exactly 1 MiB", async () => {
    const { oneHop, cloudRun } = harness();
    const declared = await oneHop(request("/v1/databases/chinook/dtql", "POST", { body: "x".repeat(MiB + 1) }));
    expect(declared.status).toBe(413);
    expect(declared.headers.get("Access-Control-Allow-Origin")).toBe("*");
    expect(cloudRun).toHaveLength(0);
    const exact = await oneHop(request("/v1/databases/chinook/dtql", "POST", { body: "x".repeat(MiB) }));
    expect(exact.status).toBe(200);
    expect(cloudRun[0]?.body.length).toBe(MiB);
  });

  it("refuses databases outside the allowlist and unknown operations without a backend call", async () => {
    const { oneHop, cloudRun } = harness();
    for (const path of ["/v1/databases/unknown", "/v1/databases/iana-http-status/dtql", "/v1/databases/ecb/dtql",
      "/v1/databases/chinook/batch", "/v1/databases/chinook/records/Album", "/v1/databases"]) {
      const response = await oneHop(request(path, "GET"));
      expect(response.status, path).toBe(404);
      expect(await response.text()).toBe("Not Found");
    }
    expect(cloudRun).toHaveLength(0);
  });

  it("forwards one request to Cloud Run on the stripped path with the origin secret and only demo-db's headers", async () => {
    const { oneHop, cloudRun, limiterKeys } = harness();
    const response = await oneHop(request("/v1/databases/chinook/dtql?limit=5", "POST", {
      headers: { ...clientHeaders, "CF-Connecting-IP": "203.0.113.7" },
    }));
    expect(response.status).toBe(200);
    expect(await response.json()).toEqual({ path: "/v1/databases/chinook/dtql", method: "POST" });
    expect(cloudRun).toHaveLength(1);
    expect(cloudRun[0]!.url).toBe(`${runOrigin}/v1/databases/chinook/dtql?limit=5`);
    expect(Object.fromEntries(cloudRun[0]!.headers)).toEqual({
      accept: "application/x-ndjson",
      "content-type": "application/json",
      "ovdb-page-close": "1",
      "ovdb-page-size": "10",
      "ovdb-page-token": "page-2",
      [DEFAULT_ORIGIN_SECRET_HEADER.toLowerCase()]: secret,
    });
    expect(cloudRun[0]!.body).toBe('{"dtql":"FROM Album"}');
    // The client's own address keys the limiter (the old cross-Worker hop hid it).
    expect(limiterKeys).toEqual(["ovdb-query:203.0.113.7"]);
    expect(response.headers.get("Access-Control-Allow-Origin")).toBe("*");
    expect(response.headers.get("Access-Control-Expose-Headers")).toBe("Cache-Control, ETag, Last-Modified, Link, OVDB-Page-Size, OVDB-Page-Token, Vary");
    expect(response.headers.get("Set-Cookie")).toBeNull();
  });

  it("rewrites the per-database index to the ovdb.demodb.dev query endpoint", async () => {
    const { oneHop } = harness();
    const response = await oneHop(request("/v1/databases/chinook", "GET"));
    expect(response.status).toBe(200);
    expect(response.headers.get("ETag")).toBeNull();
    expect(response.headers.get("Access-Control-Expose-Headers")).toBe("Cache-Control, ETag, Last-Modified, Link, Vary");
    expect(await response.json()).toEqual({ id: "chinook", title: "chinook", endpoints: { dtql: `${apiHost}/v1/databases/chinook/dtql` } });
    const wrong = await harness({ mode: "wrong-endpoint" }).oneHop(request("/v1/databases/chinook", "GET"));
    expect(wrong.status).toBe(502);
  });

  it("never reaches cloud.openvaultdb.com's own routes under the ovdb.demodb.dev host", async () => {
    const { oneHop, cloudRun } = harness();
    for (const path of ["/", "/oauth/token", "/ovdb/v1/databases/chinook/dtql", "/ovdb/dbs/chinook", "/device/", "/devices/", "/styles.css",
      "/northwind/d1/x", "/.well-known/openvaultdb", "/.well-known/oauth-authorization-server", "/api/devices", "/ecb-public", "/v1x"]) {
      const get = await oneHop(request(path, "GET"));
      expect(get.status, path).toBe(404);
      expect(await get.text()).toBe("Not Found");
      const post = await oneHop(request(path, "POST"));
      expect(post.status, path).toBe(405);
    }
    expect(cloudRun).toHaveLength(0);
  });
});

describe("cloud.openvaultdb.com is unchanged by the demodb.dev gateway", () => {
  it("keeps the /ovdb prefix and its own CORS pass-through on the cloud host", async () => {
    const { oneHop, cloudRun } = harness();
    const response = await oneHop(new Request(`${cloud}/ovdb/v1/databases/chinook/dtql`, { method: "POST", headers: clientHeaders, body: "{}" }));
    expect(response.status).toBe(200);
    expect(cloudRun[0]!.url).toBe(`${runOrigin}/ovdb/v1/databases/chinook/dtql`);
    expect(Object.fromEntries(cloudRun[0]!.headers).origin).toBe("https://datatug.app");
    expect(response.headers.get("Access-Control-Allow-Origin")).toBe("https://datatug.app");
    const v1 = await oneHop(new Request(`${cloud}/v1/databases/chinook`));
    expect(v1.status).toBe(200);
    expect(cloudRun[1]!.url).toBe(`${runOrigin}/v1/databases/chinook`);
    expect(await v1.json()).toEqual({ id: "chinook", title: "chinook", endpoints: { dtql: `${cloud}/v1/databases/chinook/dtql` } });
  });

  it("does not treat lookalike hosts as ovdb.demodb.dev", async () => {
    for (const host of ["demodb.dev", "www.demodb.dev", "chinook.demodb.dev", "ovdb.demodb.dev.example.com"]) {
      const { oneHop, cloudRun } = harness();
      const response = await oneHop(new Request(`https://${host}/ovdb/v1/databases/chinook/dtql`, { method: "POST", body: "{}" }));
      expect(response.status, host).toBe(200);
      expect(cloudRun[0]!.url, host).toBe(`${runOrigin}/ovdb/v1/databases/chinook/dtql`);
    }
  });
});
