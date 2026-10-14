import { jsonResponse } from "./http";
import type { UpstreamFetch } from "./proxy";

type ChinookEnv = Env & { CHINOOK_RUN_ORIGIN?: string };
type ECBEnv = Env & { CHINOOK_RUN_ORIGIN?: string; ECB_RUN_ORIGIN?: string; ECB_PROXY_SECRET?: string; ECB_OPERATOR_TOKEN?: string };
type IANAEnv = Env & { CHINOOK_RUN_ORIGIN?: string; IANA_RUN_ORIGIN?: string; IANA_PROXY_SECRET?: string; IANA_OPERATOR_TOKEN?: string };
const ecbProxySecretHeader = "X-OVDB-ECB-Proxy-Secret";
const ecbOperatorTokenHeader = "X-OVDB-ECB-Operator-Token";
const ianaProxySecretHeader = "X-OVDB-IANA-Proxy-Secret";
const ianaOperatorTokenHeader = "X-OVDB-IANA-Operator-Token";

const pinHeaders = [
  "OVDB-Provider-Revision",
  "OVDB-Source-SHA256",
  "OVDB-Serving-SHA256",
  "OVDB-Manifest-SHA256",
] as const;

// Selected operator routes use this only after their separate admission gates.
export const NO_RETENTION_OVDB_TIMEOUT_MS = 15_000;
type ProxyPolicy = Readonly<{ noRetention: boolean; timeoutMs?: number }>;
const samplePolicy: ProxyPolicy = Object.freeze({ noRetention: false });
const noRetentionPolicy: ProxyPolicy = Object.freeze({ noRetention: true, timeoutMs: NO_RETENTION_OVDB_TIMEOUT_MS });

// Callers cannot select this policy through request headers, URL or body.
// This factory is for trusted server-side composition, not Worker route input.
export function createTrustedNoRetentionOVDBProxy(): typeof proxyChinook {
  return (request, env, upstreamFetch) => proxyWithPolicy(request, env, upstreamFetch, noRetentionPolicy, env.CHINOOK_RUN_ORIGIN);
}

export async function proxyChinook(
  request: Request,
  env: ChinookEnv,
  upstreamFetch: UpstreamFetch,
): Promise<Response> {
  return proxyWithPolicy(request, env, upstreamFetch, samplePolicy, env.CHINOOK_RUN_ORIGIN);
}

export async function proxyECB(
  request: Request,
  env: ECBEnv,
  upstreamFetch: UpstreamFetch,
): Promise<Response> {
  if (!env.ECB_PROXY_SECRET || !/^[A-Za-z0-9_-]{32,128}$/u.test(env.ECB_PROXY_SECRET) ||
    !env.ECB_OPERATOR_TOKEN || !/^[A-Za-z0-9_-]{32,128}$/u.test(env.ECB_OPERATOR_TOKEN) ||
    !env.ECB_RUN_ORIGIN || env.ECB_RUN_ORIGIN !== env.CHINOOK_RUN_ORIGIN) {
    return jsonResponse({ error: "ECB OVDB is not configured." }, 503);
  }
  const presented = request.headers.get(ecbOperatorTokenHeader);
  if (!presented || !/^[A-Za-z0-9_-]{32,128}$/u.test(presented)) {
    return jsonResponse({ error: "Not found." }, 404);
  }
  const encoder = new TextEncoder();
  const [expected, actual] = await Promise.all([
    crypto.subtle.digest("SHA-256", encoder.encode(env.ECB_OPERATOR_TOKEN)),
    crypto.subtle.digest("SHA-256", encoder.encode(presented)),
  ]);
  let difference = 0;
  const expectedBytes = new Uint8Array(expected);
  const actualBytes = new Uint8Array(actual);
  for (let index = 0; index < expectedBytes.length; index++) difference |= expectedBytes[index] ^ actualBytes[index];
  if (difference !== 0) return jsonResponse({ error: "Not found." }, 404);
  return proxyWithPolicy(request, env, upstreamFetch, noRetentionPolicy, env.ECB_RUN_ORIGIN,
    "/v1/databases/ecb/dtql", "ECB", env.ECB_PROXY_SECRET);
}

export async function proxyIANA(request: Request, env: IANAEnv, upstreamFetch: UpstreamFetch): Promise<Response> {
  if (!env.IANA_PROXY_SECRET || !/^[A-Za-z0-9_-]{32,128}$/u.test(env.IANA_PROXY_SECRET) ||
    !env.IANA_OPERATOR_TOKEN || !/^[A-Za-z0-9_-]{32,128}$/u.test(env.IANA_OPERATOR_TOKEN) ||
    !env.IANA_RUN_ORIGIN || env.IANA_RUN_ORIGIN !== env.CHINOOK_RUN_ORIGIN) {
    return jsonResponse({ error: "IANA OVDB is not configured." }, 503);
  }
  const presented = request.headers.get(ianaOperatorTokenHeader);
  if (!presented || !/^[A-Za-z0-9_-]{32,128}$/u.test(presented)) return jsonResponse({ error: "Not found." }, 404);
  const encoder = new TextEncoder();
  const [expected, actual] = await Promise.all([
    crypto.subtle.digest("SHA-256", encoder.encode(env.IANA_OPERATOR_TOKEN)),
    crypto.subtle.digest("SHA-256", encoder.encode(presented)),
  ]);
  const expectedBytes = new Uint8Array(expected);
  const actualBytes = new Uint8Array(actual);
  let difference = 0;
  for (let index = 0; index < expectedBytes.length; index++) difference |= expectedBytes[index] ^ actualBytes[index];
  if (difference !== 0) return jsonResponse({ error: "Not found." }, 404);
  return proxyWithPolicy(request, env, upstreamFetch, noRetentionPolicy, env.IANA_RUN_ORIGIN,
    "/v1/databases/iana-http-status/dtql", "IANA", env.IANA_PROXY_SECRET, ianaProxySecretHeader);
}

async function proxyWithPolicy(
  request: Request,
  env: Env,
  upstreamFetch: UpstreamFetch,
  policy: ProxyPolicy,
  originValue: string | undefined,
  selectedPath?: string,
  service = "Chinook",
  trustedSecret?: string,
  trustedSecretHeader = ecbProxySecretHeader,
): Promise<Response> {
  if (!originValue) {
    return jsonResponse({ error: `${service} OVDB is not configured.` }, 503);
  }
  let origin: URL;
  try { origin = new URL(originValue); }
  catch { return jsonResponse({ error: `${service} OVDB origin is invalid.` }, 503); }
  if (origin.protocol !== "https:" || origin.username || origin.password || origin.pathname !== "/" || origin.search || origin.hash ||
    ((service === "ECB" || service === "IANA") && (origin.port !== "" || !origin.hostname.endsWith(".run.app") ||
      (originValue !== origin.origin && originValue !== `${origin.origin}/`)))) {
    return jsonResponse({ error: `${service} OVDB origin is invalid.` }, 503);
  }
  const publicURL = new URL(request.url);
  const upstreamURL = new URL((selectedPath ?? publicURL.pathname) + publicURL.search, origin);
  const headers = new Headers();
  for (const name of ["Accept", "Content-Type", "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers", "OVDB-Execution-ID", "OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close", ...pinHeaders] as const) {
    const value = request.headers.get(name);
    if (value !== null) headers.set(name, value);
  }
  if (trustedSecret) headers.set(trustedSecretHeader, trustedSecret);
  if (request.signal.aborted) return jsonResponse({ error: `${service} OVDB request cancelled.` }, 503);
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
      return jsonResponse({ error: `${service} OVDB request cancelled.` }, 503);
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
    console.error(`${service} OVDB upstream failed`);
    return jsonResponse({ error: `${service} OVDB is temporarily unavailable.` }, 503);
  }
}
