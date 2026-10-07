import { jsonResponse } from "./http";
import type { UpstreamFetch } from "./proxy";

type ChinookEnv = Env & { CHINOOK_RUN_ORIGIN?: string };

const pinHeaders = [
  "OVDB-Provider-Revision",
  "OVDB-Source-SHA256",
  "OVDB-Serving-SHA256",
  "OVDB-Manifest-SHA256",
] as const;

// This source-only future policy is never selected by current Worker routing.
// A trusted composition must separately admit its source before using it.
export const NO_RETENTION_OVDB_TIMEOUT_MS = 15_000;
type ProxyPolicy = Readonly<{ noRetention: boolean; timeoutMs?: number }>;
const samplePolicy: ProxyPolicy = Object.freeze({ noRetention: false });
const noRetentionPolicy: ProxyPolicy = Object.freeze({ noRetention: true, timeoutMs: NO_RETENTION_OVDB_TIMEOUT_MS });

// Neither request headers/URL/body nor deployment bindings select policy.
// Only a future reviewed server-side composition can import this factory.
export function createTrustedNoRetentionOVDBProxy(): typeof proxyChinook {
  return (request, env, upstreamFetch) => proxyWithPolicy(request, env, upstreamFetch, noRetentionPolicy);
}

export async function proxyChinook(
  request: Request,
  env: ChinookEnv,
  upstreamFetch: UpstreamFetch,
): Promise<Response> {
  return proxyWithPolicy(request, env, upstreamFetch, samplePolicy);
}

async function proxyWithPolicy(
  request: Request,
  env: ChinookEnv,
  upstreamFetch: UpstreamFetch,
  policy: ProxyPolicy,
): Promise<Response> {
  if (!env.CHINOOK_RUN_ORIGIN) {
    return jsonResponse({ error: "Chinook OVDB is not configured." }, 503);
  }
  let origin: URL;
  try { origin = new URL(env.CHINOOK_RUN_ORIGIN); }
  catch { return jsonResponse({ error: "Chinook OVDB origin is invalid." }, 503); }
  if (origin.protocol !== "https:" || origin.username || origin.password || origin.pathname !== "/" || origin.search || origin.hash) {
    return jsonResponse({ error: "Chinook OVDB origin is invalid." }, 503);
  }
  const publicURL = new URL(request.url);
  const upstreamURL = new URL(publicURL.pathname + publicURL.search, origin);
  const headers = new Headers();
  for (const name of ["Accept", "Content-Type", "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers", "OVDB-Execution-ID", "OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close", ...pinHeaders] as const) {
    const value = request.headers.get(name);
    if (value !== null) headers.set(name, value);
  }
  if (request.signal.aborted) return jsonResponse({ error: "Chinook OVDB request cancelled." }, 503);
  const abort = new AbortController();
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  let streamController: ReadableStreamDefaultController<Uint8Array> | undefined;
  let finished = false;
  const dispose = () => {
    finished = true;
    if (deadline !== undefined) clearTimeout(deadline);
    request.signal.removeEventListener("abort", cancel);
  };
  const cancel = () => {
    if (finished) return;
    dispose();
    abort.abort();
    streamController?.error(new Error("OVDB upstream cancelled"));
    void reader?.cancel().catch(() => {});
  };
  const deadline = policy.timeoutMs === undefined ? undefined : setTimeout(cancel, policy.timeoutMs);
  request.signal.addEventListener("abort", cancel, { once: true });
  try {
    const upstream = await upstreamFetch(upstreamURL, {
      method: request.method,
      headers,
      body: request.method === "GET" || request.method === "HEAD" ? undefined : request.body,
      redirect: "manual",
      ...(policy.noRetention ? { cache: "no-store" as const } : {}),
      signal: abort.signal,
    });
    if (abort.signal.aborted) {
      void upstream.body?.cancel().catch(() => {});
      return jsonResponse({ error: "Chinook OVDB request cancelled." }, 503);
    }
    const responseHeaders = new Headers();
    for (const name of ["Content-Type", "Cache-Control", "Location", "Link", "Vary", "Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Expose-Headers", "Access-Control-Max-Age", "Retry-After", "X-Content-Type-Options", "Content-Security-Policy", ...pinHeaders] as const) {
      const value = upstream.headers.get(name);
      if (value !== null) responseHeaders.set(name, value);
    }
    responseHeaders.set("X-Content-Type-Options", "nosniff");
    if (policy.noRetention) {
      responseHeaders.set("Cache-Control", "no-store");
      responseHeaders.set("Pragma", "no-cache");
      responseHeaders.set("Referrer-Policy", "no-referrer");
    }
    let body: ReadableStream<Uint8Array> | null = null;
    if (upstream.body) {
      reader = upstream.body.getReader();
      body = new ReadableStream<Uint8Array>({
        start(controller) { streamController = controller; },
        async pull(controller) {
          try {
            const chunk = await reader!.read();
            if (finished) return;
            if (chunk.done) { dispose(); controller.close(); }
            else controller.enqueue(chunk.value);
          } catch {
            if (finished) return;
            dispose();
            abort.abort();
            void reader!.cancel().catch(() => {});
            controller.error(new Error("OVDB upstream stream failed"));
          }
        },
        async cancel() { dispose(); abort.abort(); await reader!.cancel().catch(() => {}); },
      });
    } else dispose();
    return new Response(body, {
      status: upstream.status,
      statusText: upstream.statusText,
      headers: responseHeaders,
    });
  } catch {
    dispose();
    abort.abort();
    void reader?.cancel().catch(() => {});
    console.error("Chinook OVDB upstream failed");
    return jsonResponse({ error: "Chinook OVDB is temporarily unavailable." }, 503);
  }
}
