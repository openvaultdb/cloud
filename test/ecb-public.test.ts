import { env } from "cloudflare:workers";
import { createExecutionContext } from "cloudflare:test";
import { expect, it, vi } from "vitest";
import { createWorker } from "../src/worker";
import { ECB_PUBLIC_PATH } from "../src/ecb-public";
const secret = "synthetic-public-proxy-key-32-bytes-long";
const origin = "https://synthetic.directory.invalid";
const backend = "https://synthetic-ecb.a.run.app";
const id = "a".repeat(32);
const body = "from: {name: daily}\nlimit: 1\n";
const hex = (raw: ArrayBuffer) => [...new Uint8Array(raw)].map((value) => value.toString(16).padStart(2, "0")).join("");
const encoder = new TextEncoder();
async function environment() {
    const admission = JSON.stringify({ format: "ovdb-ecb-public-free-admission/1", decision: "public-free-transient-read-only", approvedBy: "synthetic-review", approvedAt: new Date(Date.now() - 3600000).toISOString(), expiresAt: new Date(Date.now() + 3600000).toISOString(), costOwner: "synthetic-cost-owner", hostConfigSHA256: "a".repeat(64), publisherManifestSHA256: "399ce77bc4513b1a819f61e26a54fe8e6c46569b2b45ea078582d9ad6758697e", decoderModuleVersion: "v0.4.0", decoderSHA256: "20477d567705fe7cf8115caf696848b9ea1db2e1a165b977a4f54bd610955b49", rightsDigest: "08669fda7a7d255d1c77d2be733e587a23bb2ff1a916e96c727b82d2540cdc93", requestProfile: "ecb-public-free/1", workerPath: ECB_PUBLIC_PATH, goPath: "/v1/databases/ecb/dtql", directoryOrigin: origin, backendOrigin: backend, audience: "public-free", paidAccess: false, maxReads: 1, maxRows: 50, maxConcurrent: 1, executionsPerMinute: 6 });
    const digest = hex(await crypto.subtle.digest("SHA-256", encoder.encode(admission)));
    return { ...env, ECB_PUBLIC_ENABLED: "true", ECB_PUBLIC_ADMISSION: admission, ECB_PUBLIC_ADMISSION_SHA256: digest, ECB_PUBLIC_PROXY_SECRET: secret, CHINOOK_RUN_ORIGIN: backend, ECB_PUBLIC_LIMITER: { limit: vi.fn(async () => ({ success: true })) } } as Env;
}
const request = (headers: object = {}, signal?: AbortSignal) => new Request(`https://cloud.openvaultdb.com${ECB_PUBLIC_PATH}`, { method: "POST", headers: { Origin: origin, "Content-Type": "application/yaml", "OVDB-Execution-ID": id, ...headers }, body, signal });
const invoke = (worker: ReturnType<typeof createWorker>, r: Request, e: Env) => Promise.resolve((worker.fetch as (r: Request, e: Env, c: ExecutionContext) => Promise<Response>)(r, e, createExecutionContext()));
async function signed(text: string, e: Env, execution = id, contentType = "application/json", status = 200) {
    const digest = (e as Env & {
        ECB_PUBLIC_ADMISSION_SHA256: string;
    }).ECB_PUBLIC_ADMISSION_SHA256;
    const raw = encoder.encode(`ovdb-ecb-public-completion/1\n${digest}\n${execution}\n${status}\n${contentType}\n${text}`);
    const key = await crypto.subtle.importKey("raw", encoder.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
    return new Response(text, { status, headers: { "Content-Type": contentType, "X-OVDB-ECB-Public-Completion": hex(await crypto.subtle.sign("HMAC", key, raw)), "Cache-Control": "max-age=300", "Set-Cookie": "MARKER" } });
}
it("requires separate exact admission and refuses caller credentials before upstream", async () => {
    const e = await environment();
    const fetcher = vi.fn(async () => new Response("MARKER"));
    const worker = createWorker(fetcher);
    for (const headers of [{ Authorization: "MARKER" }, { Cookie: "MARKER" }, { "X-Paid-Capability": "MARKER" }, { "X-OVDB-ECB-Public-Secret": secret }, { "OVDB-Execution-ID": `${id},${id}` }, { Origin: "null" }]) {
        const r = await invoke(worker, request(headers), e);
        expect(r.status).not.toBe(200);
        expect(r.headers.get("Cache-Control")).toBe("no-store");
        expect(await r.text()).not.toContain("MARKER");
    }
    for (const override of [{ ECB_PUBLIC_ENABLED: "false" }, { ECB_ENABLED: "true" }, { ECB_PUBLIC_ADMISSION_SHA256: "0".repeat(64) }, { ECB_PUBLIC_LIMITER: undefined }])
        expect((await invoke(worker, request(), { ...e, ...override } as Env)).status).toBe(503);
    expect(fetcher).not.toHaveBeenCalled();
    const preflight = await invoke(worker, new Request(`https://cloud.openvaultdb.com${ECB_PUBLIC_PATH}`, { method: "OPTIONS", headers: { Origin: origin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "Content-Type, OVDB-Execution-ID" } }), e);
    expect(preflight.status).toBe(204);
    expect(preflight.headers.get("Access-Control-Allow-Origin")).toBe(origin);
    expect(fetcher).not.toHaveBeenCalled();
});
it("holds all bytes until exact complete HMAC, strips cookies/proof/cache and sends no caller secrets", async () => {
    const e = await environment();
    const text = '{"records":[],"complete":true,"syntheticCompleteEvidence":true}';
    let finalize: () => void = () => { };
    let entered: () => void = () => { };
    const ready = new Promise<void>(resolve => { entered = resolve; });
    const proof = await signed(text, e);
    const worker = createWorker(async (input, init) => { expect(String(input)).toBe(`${backend}/v1/databases/ecb/dtql`); expect(init?.cache).toBe("no-store"); expect(init?.redirect).toBe("manual"); const headers = new Headers(init?.headers); expect(headers.get("Authorization")).toBeNull(); expect(headers.get("Cookie")).toBeNull(); expect(headers.get("X-OVDB-ECB-Public-Secret")).toBe(secret); return new Response(new ReadableStream({ start(c) { c.enqueue(encoder.encode(text)); entered(); finalize = () => c.close(); } }), { headers: proof.headers }); });
    let resolved = false;
    const pending = invoke(worker, request(), e).then(result => { resolved = true; return result; });
    await ready;
    await Promise.resolve();
    expect(resolved).toBe(false);
    finalize();
    const result = await pending;
    expect(result.status).toBe(200);
    expect(await result.text()).toBe(text);
    expect(result.headers.get("Cache-Control")).toBe("no-store");
    expect(result.headers.get("Set-Cookie")).toBeNull();
    expect(result.headers.get("X-OVDB-ECB-Public-Completion")).toBeNull();
});
it("row then bad/missing/failure/interrupted footer releases zero marker bytes", async () => {
    const e = await environment();
    const approved = await signed('{"records":[],"complete":true}', e);
    for (const suffix of ["", ',"complete":false}', ',"sourceRights":[]}', ',"providerReads":null}', ',"usedSourceIds":[]}', ',"complete":true,"complete":true}', "{}"]) {
        let canceled = false;
        const worker = createWorker(async () => new Response(new ReadableStream({ start(c) { c.enqueue(encoder.encode('{"records":[{"data":{"rate":"MARKER"}}]')); c.enqueue(encoder.encode(suffix)); c.close(); }, cancel() { canceled = true; } }), { headers: approved.headers }));
        const result = await invoke(worker, request(), e);
        expect(result.status).toBe(503);
        expect(await result.text()).not.toContain("MARKER");
        expect(result.headers.get("Cache-Control")).toBe("no-store");
        expect(canceled).toBe(false); // complete read consumes the stream before MAC rejection
    }
    const missing = createWorker(async () => new Response('MARKER'));
    expect(await (await invoke(missing, request(), e)).text()).not.toContain("MARKER");
    const interrupted = createWorker(async () => new Response(new ReadableStream({ start(c) { c.enqueue(encoder.encode('MARKER')); c.error(new Error('MARKER')); } }), { headers: approved.headers }));
    expect(await (await invoke(interrupted, request(), e)).text()).not.toContain("MARKER");
});
it("refuses execution/status/content-type/byte tampering and caps completed bodies", async () => {
    const e = await environment();
    for (const source of [await signed("MARKER", e, "b".repeat(32)), await signed("MARKER", e, id, "text/plain"), await signed("MARKER", e, id, "application/json", 201), await signed("x".repeat(65537), e)]) {
        const result = await invoke(createWorker(async () => source), request(), e);
        expect(result.status).toBe(503);
        expect(await result.text()).not.toContain("MARKER");
    }
});
it("cancellation while a first row is internal cancels reader and releases zero bytes", async () => {
    const e = await environment();
    const proof = await signed('MARKER', e);
    let cancelCount = 0;
    let entered: () => void = () => { };
    const ready = new Promise<void>(resolve => { entered = resolve; });
    const controller = new AbortController();
    const worker = createWorker(async () => new Response(new ReadableStream({ start(c) { c.enqueue(encoder.encode('MARKER')); entered(); }, cancel() { cancelCount++; } }), { headers: proof.headers }));
    const pending = invoke(worker, request({}, controller.signal), e);
    await ready;
    controller.abort();
    const result = await pending;
    expect(result.status).toBe(503);
    expect(await result.text()).not.toContain('MARKER');
    expect(cancelCount).toBe(1);
});
it("total deadline bounds ignored-abort headers/body and prevents late release", async () => {
    const e = await environment();
    vi.useFakeTimers();
    try {
        let signal: AbortSignal | null | undefined;
        const stalled = createWorker(async (_input, init) => { signal = init?.signal; return new Promise<Response>(() => { }); });
        const pending = invoke(stalled, request(), e);
        await vi.advanceTimersByTimeAsync(10000);
        const result = await pending;
        expect(result.status).toBe(503);
        expect(signal?.aborted).toBe(true);
        expect(await result.text()).not.toContain("MARKER");
        const proof = await signed("MARKER", e);
        let canceled = false;
        const streamed = createWorker(async () => new Response(new ReadableStream({ start(c) { c.enqueue(encoder.encode("MARKER")); }, cancel() { canceled = true; } }), { headers: proof.headers }));
        const waiting = invoke(streamed, request(), e);
        await vi.advanceTimersByTimeAsync(10000);
        const failed = await waiting;
        expect(failed.status).toBe(503);
        expect(await failed.text()).not.toContain("MARKER");
        expect(canceled).toBe(true);
    }
    finally {
        vi.useRealTimers();
    }
});

it("budgets invalid native syntax as transport, leaving native refusal to Go", async () => {
    const e = await environment();
    const limiter = (e as Env & {ECB_PUBLIC_LIMITER: {limit: ReturnType<typeof vi.fn>}}).ECB_PUBLIC_LIMITER;
    const backendCall = vi.fn(async () => new Response('{"error":"native request refused"}', {status: 422, headers: {"Content-Type":"application/json", "Cache-Control":"no-store"}}));
    const worker = createWorker(backendCall);
    const invalid = new Request(`https://cloud.openvaultdb.com${ECB_PUBLIC_PATH}`, {method: "POST", headers: {Origin: origin, "Content-Type":"application/yaml", "OVDB-Execution-ID":id}, body:"from: {name: daily}\nlimit: 51\n"});
    const result = await invoke(worker, invalid, e);
    expect(result.status).toBe(503);
    expect(limiter.limit).toHaveBeenCalledTimes(1);
    expect(backendCall).toHaveBeenCalledTimes(1);
    expect(result.headers.get("Cache-Control")).toBe("no-store");
    expect(await result.text()).not.toContain("records");
});
