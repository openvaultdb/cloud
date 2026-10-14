import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { expect, it, vi } from "vitest";
import { createWorker } from "../src/worker";

const publicURL = "https://cloud.openvaultdb.com/ecb/v1/databases/ecb/dtql";
const origin = "https://synthetic-ecb.a.run.app";
const secret = "synthetic-ecb-proxy-secret-32-bytes-long";
const operatorToken = "synthetic-operator-token-32-bytes-long";
const marker = "synthetic-private-error-marker";
const query = "from: {name: daily}\ncolumns: [{field: time}, {field: currency}, {field: rate}]\nlimit: 1\n";

const bridgeURL = (env as Env & { ECB_CHAIN_BRIDGE_URL?: string }).ECB_CHAIN_BRIDGE_URL;

it.skipIf(!bridgeURL)("drives the selected Worker request through configuredHandler over local HTTP", async () => {
  const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
  const defaultCache = (caches as CacheStorage & { default: Cache }).default;
  const cachePut = vi.spyOn(defaultCache, "put").mockImplementation(async () => {});
  try {
    console.error("capture-control");
    expect(consoleError).toHaveBeenCalledWith("capture-control");
    consoleError.mockClear();
    await defaultCache.put("https://synthetic.invalid/control", new Response("control"));
    expect(cachePut).toHaveBeenCalledTimes(1);
    cachePut.mockClear();

    let upstreamCalls = 0;
    const worker = createWorker(async (input, init) => {
      upstreamCalls++;
      expect(init?.cache).toBe("no-store");
      expect(init?.redirect).toBe("manual");
      const request = new Request(input, init);
      expect(request.url).toBe(`${origin}/v1/databases/ecb/dtql`);
      expect(request.headers.get("X-OVDB-ECB-Proxy-Secret")).toBe(secret);
      expect(request.headers.get("X-OVDB-ECB-Operator-Token")).toBeNull();
      expect(request.headers.get("Authorization")).toBeNull();
      expect(request.headers.get("Cookie")).toBeNull();
      const upstream = await fetch(new Request(`${bridgeURL}/v1/databases/ecb/dtql`, init));
      expect(upstream.headers.get("Cache-Control")).toBe("no-store");
      return upstream;
    });
    const environment = { ...env, ECB_ENABLED: "true", CHINOOK_RUN_ORIGIN: origin,
      ECB_RUN_ORIGIN: origin, ECB_PROXY_SECRET: secret, ECB_OPERATOR_TOKEN: operatorToken } as Env;
    const invoke = (request: Request, selectedEnv = environment): Promise<Response> => Promise.resolve(
      (worker.fetch as (request: Request, env: Env, context: ExecutionContext) => Response | Promise<Response>)(
        request, selectedEnv, createExecutionContext(),
      ),
    );
    const body = () => new Request(publicURL, { method: "POST", body: query,
      headers: { Authorization: `Bearer ${marker}`, Cookie: marker, "X-OVDB-ECB-Proxy-Secret": marker,
        "X-OVDB-ECB-Operator-Token": operatorToken } });

    const success = await invoke(body());
    expect(success.status).toBe(200);
    expect(success.headers.get("Cache-Control")).toBe("no-store");
    expect(success.headers.get("Set-Cookie")).toBeNull();
    expect(JSON.stringify([...success.headers])).not.toContain(marker);
    const successText = await success.text();
    expect(successText).not.toContain(marker);
    const result = JSON.parse(successText) as { records: Array<{ data: { currency: string; rate: string } }> };
    expect(result.records).toHaveLength(1);
    expect(result.records[0]).toMatchObject({ data: { currency: "AAA", rate: "001.23000" } });
    expect(upstreamCalls).toBe(1);

    const refused = await invoke(body(), { ...environment, ECB_ENABLED: "false" } as Env);
    expect(refused.status).toBe(503);
    expect(refused.headers.get("Cache-Control")).toBe("no-store");
    expect(JSON.stringify([...refused.headers])).not.toContain(marker);
    expect(await refused.text()).not.toContain(marker);
    expect(upstreamCalls).toBe(1);

    const failed = await invoke(body());
    expect(failed.status).not.toBe(200);
    expect(failed.headers.get("Cache-Control")).toBe("no-store");
    expect(JSON.stringify([...failed.headers])).not.toContain(marker);
    expect(await failed.text()).not.toContain(marker);
    expect(upstreamCalls).toBe(2);
    expect(consoleError).toHaveBeenCalledWith("ECB OVDB upstream failed");
    expect(consoleError.mock.calls.flat().join(" ")).not.toContain(marker);
    expect(cachePut).not.toHaveBeenCalled();
  } finally {
    consoleError.mockRestore();
    cachePut.mockRestore();
  }
});
