import type { UpstreamFetch } from "./proxy";
export const ECB_PUBLIC_PATH = "/ecb-public/v1/databases/ecb/dtql";
const goPath = "/v1/databases/ecb/dtql";
const secretHeader = "X-OVDB-ECB-Public-Secret";
const admissionHeader = "X-OVDB-ECB-Public-Admission";
const proofHeader = "X-OVDB-ECB-Public-Completion";
const requestCap = 8192;
const responseCap = 65536;
const timeoutMs = 10000;
const encoder = new TextEncoder();
type PublicEnv = Env & {
    ECB_PUBLIC_ENABLED?: string;
    ECB_PUBLIC_ADMISSION?: string;
    ECB_PUBLIC_ADMISSION_SHA256?: string;
    ECB_PUBLIC_PROXY_SECRET?: string;
    ECB_PUBLIC_LIMITER?: RateLimit;
    ECB_ENABLED?: string;
    ECB_PROXY_SECRET?: string;
    CHINOOK_RUN_ORIGIN?: string;
};
const admissionKeys = ["format", "decision", "approvedBy", "approvedAt", "expiresAt", "costOwner", "hostConfigSHA256", "publisherManifestSHA256", "decoderModuleVersion", "decoderSHA256", "rightsDigest", "requestProfile", "workerPath", "goPath", "directoryOrigin", "backendOrigin", "audience", "paidAccess", "maxReads", "maxRows", "maxConcurrent", "executionsPerMinute"];
const hex = (raw: ArrayBuffer): string => [...new Uint8Array(raw)].map((x) => x.toString(16).padStart(2, "0")).join("");
function response(status: number, origin?: string, body?: Uint8Array<ArrayBuffer>): Response {
    const headers = new Headers({ "Content-Type": "application/json", "Cache-Control": "no-store", Pragma: "no-cache", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", Vary: "Origin" });
    if (origin)
        headers.set("Access-Control-Allow-Origin", origin);
    return new Response(status === 204 ? null : body ?? '{"error":"ECB public request refused."}', { status, headers });
}
// Trusted backend validates the exact released protocol before issuing a MAC.
// No raw upstream status/body/headers escape until complete bytes authenticate.
export function createECBPublicProxy(upstreamFetch: UpstreamFetch) {
    let active = false;
    return async (request: Request, environment: Env): Promise<Response> => {
        const env = environment as PublicEnv;
        const abort = new AbortController();
        const started = Date.now();
        let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
        let rejectCutoff: (reason: Error) => void = () => { };
        const cutoff = new Promise<never>((_, reject) => { rejectCutoff = reject; });
        const cancel = () => { abort.abort(); rejectCutoff(new Error("public ECB cancelled")); void reader?.cancel().catch(() => { }); };
        const deadline = setTimeout(cancel, timeoutMs);
        request.signal.addEventListener("abort", cancel, { once: true });
        let occupied = false;
        const bounded = async (body: ReadableStream<Uint8Array> | null, cap: number): Promise<Uint8Array<ArrayBuffer>> => {
            if (!body)
                throw new Error("missing body");
            reader = body.getReader();
            const buffer = new Uint8Array(cap);
            let size = 0;
            while (true) {
                const chunk = await Promise.race([reader.read(), cutoff]);
                if (abort.signal.aborted || Date.now() - started >= timeoutMs)
                    throw new Error("deadline");
                if (chunk.done) {
                    reader.releaseLock();
                    reader = undefined;
                    return buffer.slice(0, size);
                }
                if (chunk.value.byteLength > cap - size)
                    throw new Error("body cap");
                buffer.set(chunk.value, size);
                size += chunk.value.byteLength;
            }
        };
        try {
            return await Promise.race([cutoff, (async () => {
                    if (request.signal.aborted)
                        throw new Error("cancelled");
                    if (env.ECB_PUBLIC_ENABLED !== "true" || env.ECB_ENABLED === "true" || !env.ECB_PUBLIC_LIMITER || !env.ECB_PUBLIC_PROXY_SECRET || !/^[A-Za-z0-9_-]{32,128}$/u.test(env.ECB_PUBLIC_PROXY_SECRET) || env.ECB_PUBLIC_PROXY_SECRET === env.ECB_PROXY_SECRET || !env.ECB_PUBLIC_ADMISSION || encoder.encode(env.ECB_PUBLIC_ADMISSION).length > 4096 || !/^[0-9a-f]{64}$/u.test(env.ECB_PUBLIC_ADMISSION_SHA256 ?? ""))
                        return response(503);
                    const digest = hex(await crypto.subtle.digest("SHA-256", encoder.encode(env.ECB_PUBLIC_ADMISSION)));
                    if (digest !== env.ECB_PUBLIC_ADMISSION_SHA256)
                        return response(503);
                    const admission = JSON.parse(env.ECB_PUBLIC_ADMISSION) as Record<string, unknown>;
                    // Exact canonical representation rejects duplicates, null and extra keys.
                    if (JSON.stringify(admission) !== env.ECB_PUBLIC_ADMISSION || Object.keys(admission).length !== admissionKeys.length || admissionKeys.some((key) => !(key in admission)))
                        return response(503);
                    for (const key of admissionKeys.filter((key) => !["paidAccess", "maxReads", "maxRows", "maxConcurrent", "executionsPerMinute"].includes(key)))
                        if (typeof admission[key] !== "string" || !(admission[key] as string).trim())
                            return response(503);
                    const backend = new URL(admission.backendOrigin as string);
                    const directory = new URL(admission.directoryOrigin as string);
                    const now = Date.now();
                    const approved = Date.parse(admission.approvedAt as string);
                    const expires = Date.parse(admission.expiresAt as string);
                    if (!Number.isFinite(approved) || !Number.isFinite(expires) || approved > now || expires <= now || expires <= approved || expires - approved > 30 * 86400000 || backend.protocol !== "https:" || !backend.hostname.endsWith(".run.app") || backend.port || backend.origin !== admission.backendOrigin || env.CHINOOK_RUN_ORIGIN !== backend.origin || directory.protocol !== "https:" || directory.origin !== admission.directoryOrigin || admission.format !== "ovdb-ecb-public-free-admission/1" || admission.decision !== "public-free-transient-read-only" || admission.audience !== "public-free" || admission.paidAccess !== false || admission.decoderModuleVersion !== "v0.4.0" || admission.publisherManifestSHA256 !== "399ce77bc4513b1a819f61e26a54fe8e6c46569b2b45ea078582d9ad6758697e" || admission.decoderSHA256 !== "20477d567705fe7cf8115caf696848b9ea1db2e1a165b977a4f54bd610955b49" || admission.rightsDigest !== "08669fda7a7d255d1c77d2be733e587a23bb2ff1a916e96c727b82d2540cdc93" || !/^[0-9a-f]{64}$/u.test(admission.hostConfigSHA256 as string) || admission.requestProfile !== "ecb-public-free/1" || admission.workerPath !== ECB_PUBLIC_PATH || admission.goPath !== goPath || admission.maxReads !== 1 || admission.maxRows !== 50 || admission.maxConcurrent !== 1 || admission.executionsPerMinute !== 6)
                        return response(503);
                    const origin = request.headers.get("Origin");
                    const url = new URL(request.url);
                    if (url.pathname !== ECB_PUBLIC_PATH || request.url.includes("?") || origin !== directory.origin)
                        return response(404);
                    for (const [name] of request.headers)
                        if (/^(authorization|cookie)$/u.test(name) || /paid|entitlement|billing/u.test(name) || name.startsWith("x-ovdb-") || name.startsWith("ovdb-page"))
                            return response(404, origin);
                    if (request.method === "OPTIONS") {
                        if (request.headers.get("Access-Control-Request-Method") !== "POST")
                            return response(405, origin);
                        const requested = (request.headers.get("Access-Control-Request-Headers") ?? "").toLowerCase().split(",").map((key) => key.trim());
                        if (requested.some((key) => !["content-type", "ovdb-execution-id"].includes(key)) || new Set(requested).size !== requested.length)
                            return response(405, origin);
                        const result = response(204, origin);
                        result.headers.set("Access-Control-Allow-Methods", "POST");
                        result.headers.set("Access-Control-Allow-Headers", "Content-Type, OVDB-Execution-ID");
                        return result;
                    }
                    const id = request.headers.get("OVDB-Execution-ID") ?? "";
                    const contentType = request.headers.get("Content-Type") ?? "";
                    if (request.method !== "POST" || !/^[0-9a-f]{32}$/u.test(id) || !["application/yaml", "text/plain"].includes(contentType))
                        return response(405, origin);
                    const body = await bounded(request.body, requestCap);
                    if (active)
                        return response(429, origin);
                    // This budget counts all admitted transport attempts, including invalid
                // native syntax. Go alone validates native grammar before its execution
                // slot/provider I/O; Worker never duplicates that YAML parser.
                const limit = await Promise.race([env.ECB_PUBLIC_LIMITER!.limit({ key: "ecb-public-service" }), cutoff]);
                    if (abort.signal.aborted)
                        throw new Error("cancelled");
                    if (!limit.success || active)
                        return response(429, origin);
                    active = true;
                    occupied = true;
                    const headers = new Headers({ "Content-Type": contentType, Accept: "application/json", Origin: origin, "OVDB-Execution-ID": id, [secretHeader]: env.ECB_PUBLIC_PROXY_SECRET, [admissionHeader]: digest });
                    const pending = upstreamFetch(new URL(goPath, backend), { method: "POST", headers, body, redirect: "manual", cache: "no-store", signal: abort.signal });
                    // Dispose a late response even when a fake/upstream ignores AbortSignal.
                    void pending.then((upstream) => { if (abort.signal.aborted)
                        void upstream.body?.cancel().catch(() => { }); }, () => { });
                    const upstream = await Promise.race([pending, cutoff]);
                    if (upstream.status !== 200 || upstream.headers.get("Content-Type") !== "application/json" || !/^[0-9a-f]{64}$/u.test(upstream.headers.get(proofHeader) ?? "")) {
                        void upstream.body?.cancel().catch(() => { });
                        return response(503, origin);
                    }
                    const bytes = await bounded(upstream.body, responseCap);
                    const prefix = encoder.encode(`ovdb-ecb-public-completion/1\n${digest}\n${id}\n${upstream.status}\n${upstream.headers.get("Content-Type")}\n`);
                    const authenticated = new Uint8Array(prefix.length + bytes.length);
                    authenticated.set(prefix);
                    authenticated.set(bytes, prefix.length);
                    const key = await crypto.subtle.importKey("raw", encoder.encode(env.ECB_PUBLIC_PROXY_SECRET), { name: "HMAC", hash: "SHA-256" }, false, ["verify"]);
                    const signature = Uint8Array.from(upstream.headers.get(proofHeader)!.match(/../gu)!, (pair) => Number.parseInt(pair, 16));
                    const valid = await crypto.subtle.verify("HMAC", key, signature, authenticated);
                    if (!valid || abort.signal.aborted || Date.now() - started >= timeoutMs)
                        return response(503, origin);
                    return response(200, origin, bytes);
                })()]);
        }
        catch {
            return response(503);
        }
        finally {
            clearTimeout(deadline);
            request.signal.removeEventListener("abort", cancel);
            abort.abort();
            void reader?.cancel().catch(() => { });
            reader = undefined;
            if (occupied)
                active = false;
        }
    };
}
