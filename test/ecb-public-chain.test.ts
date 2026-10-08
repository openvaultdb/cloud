import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { expect, it, vi } from "vitest";
import { createWorker } from "../src/worker";
import { ECB_PUBLIC_PATH } from "../src/ecb-public";
const bridge = env as Env & {
    ECB_CHAIN_BRIDGE_URL?: string;
    ECB_PUBLIC_CHAIN_ADMISSION?: string;
    ECB_PUBLIC_CHAIN_DIGEST?: string;
};
it.skipIf(!bridge.ECB_PUBLIC_CHAIN_ADMISSION)("released selected Go gate authenticates full native and zero output to Worker", async () => {
    const marker = "synthetic-private-marker";
    const logs = vi.spyOn(console, "error").mockImplementation(() => { });
    const cache = (caches as CacheStorage & {
        default: Cache;
    }).default;
    const put = vi.spyOn(cache, "put").mockImplementation(async () => { });
    try {
        console.error("control");
        expect(logs).toHaveBeenCalledWith("control");
        logs.mockClear();
        await cache.put("https://synthetic.invalid/control", new Response("control"));
        expect(put).toHaveBeenCalledTimes(1);
        put.mockClear();
        const origin = "https://synthetic.directory.invalid";
        const backend = "https://synthetic-ecb.a.run.app";
        let calls = 0;
        let badFooter = false;
        const worker = createWorker(async (input, init) => { calls++; expect(String(input)).toBe(`${backend}/v1/databases/ecb/dtql`); expect(init?.cache).toBe("no-store"); expect(init?.redirect).toBe("manual"); const headers = new Headers(init?.headers); if (badFooter)
            headers.set("X-Synthetic-Bad-Footer", "true"); return fetch(new Request(`${bridge.ECB_CHAIN_BRIDGE_URL}/v1/databases/ecb/dtql`, { ...init, headers })); });
        const e = { ...env, CHINOOK_RUN_ORIGIN: backend, ECB_PUBLIC_ENABLED: "true", ECB_PUBLIC_ADMISSION: bridge.ECB_PUBLIC_CHAIN_ADMISSION, ECB_PUBLIC_ADMISSION_SHA256: bridge.ECB_PUBLIC_CHAIN_DIGEST, ECB_PUBLIC_PROXY_SECRET: "synthetic-public-proxy-key-32-bytes-long", ECB_PUBLIC_LIMITER: { limit: async () => ({ success: true }) } } as Env;
        const run = (query: string, extra: Record<string, string> = {}) => Promise.resolve((worker.fetch as (r: Request, e: Env, c: ExecutionContext) => Promise<Response>)(new Request(`https://cloud.openvaultdb.com${ECB_PUBLIC_PATH}`, { method: "POST", headers: { Origin: origin, "Content-Type": "application/yaml", "OVDB-Execution-ID": "a".repeat(32), ...extra }, body: query }), e, createExecutionContext()));
        const query = "from: {name: daily}\ncolumns: [{field: time}, {field: currency}, {field: rate}]\nlimit: 1\n";
        const good = await run(query);
        expect(good.status).toBe(200);
        expect(good.headers.get("Cache-Control")).toBe("no-store");
        expect(good.headers.get("X-OVDB-ECB-Public-Completion")).toBeNull();
        const result = await good.json() as {
            records: Array<{
                data: {
                    rate: string;
                };
            }>;
            complete: boolean;
            sourceRights: unknown[];
            providerReads: unknown;
        };
        expect(result.complete).toBe(true);
        expect(result.records[0].data.rate).toBe("001.23000");
        expect(result.sourceRights).toHaveLength(1);
        expect(result.providerReads).toBeDefined();
        const empty = await run(query + "where: {op: '==', left: {field: currency}, right: {value: ZZZ}}\n");
        expect(empty.status).toBe(200);
        const zero = await empty.json() as typeof result;
        expect(zero.records).toHaveLength(0);
        expect(zero.sourceRights).toHaveLength(1);
        expect(zero.providerReads).toBeDefined();
        const invalid = await run(query.replace("limit: 1", "limit: 51"));
        expect(invalid.status).toBe(503);
        expect(await invalid.text()).not.toContain(marker);
        expect(calls).toBe(3);
        const refused = await run(query, { Authorization: marker });
        expect(refused.status).not.toBe(200);
        expect(await refused.text()).not.toContain(marker);
        expect(calls).toBe(3);
        badFooter = true;
        const bad = await run(query);
        expect(bad.status).toBe(503);
        expect(await bad.text()).not.toContain(marker);
        expect(bad.headers.get("Cache-Control")).toBe("no-store");
        expect(calls).toBe(4);
        expect(put).not.toHaveBeenCalled();
        expect(JSON.stringify(logs.mock.calls)).not.toContain(marker);
    }
    finally {
        logs.mockRestore();
        put.mockRestore();
    }
});
