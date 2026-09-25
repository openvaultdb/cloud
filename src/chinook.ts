import { jsonResponse } from "./http";
import type { UpstreamFetch } from "./proxy";

type ChinookEnv = Env & { CHINOOK_RUN_ORIGIN?: string };

export async function proxyChinook(
  request: Request,
  env: ChinookEnv,
  upstreamFetch: UpstreamFetch,
): Promise<Response> {
  if (!env.CHINOOK_RUN_ORIGIN) {
    return jsonResponse({ error: "Chinook OVDB is not configured." }, 503);
  }
  const origin = new URL(env.CHINOOK_RUN_ORIGIN);
  if (origin.protocol !== "https:" || origin.username || origin.password || origin.pathname !== "/" || origin.search || origin.hash) {
    return jsonResponse({ error: "Chinook OVDB origin is invalid." }, 503);
  }
  const publicURL = new URL(request.url);
  const upstreamURL = new URL(publicURL.pathname + publicURL.search, origin);
  const headers = new Headers();
  for (const name of ["Accept", "Content-Type", "Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers", "OVDB-Page-Size", "OVDB-Page-Token", "OVDB-Page-Close"] as const) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }
  try {
    const upstream = await upstreamFetch(upstreamURL, {
      method: request.method,
      headers,
      body: request.method === "GET" || request.method === "HEAD" ? undefined : request.body,
      redirect: "manual",
    });
    const responseHeaders = new Headers();
    for (const name of ["Content-Type", "Cache-Control", "Location", "Link", "Vary", "Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Max-Age", "X-Content-Type-Options", "Content-Security-Policy"] as const) {
      const value = upstream.headers.get(name);
      if (value) responseHeaders.set(name, value);
    }
    responseHeaders.set("X-Content-Type-Options", "nosniff");
    return new Response(upstream.body, {
      status: upstream.status,
      statusText: upstream.statusText,
      headers: responseHeaders,
    });
  } catch (error) {
    console.error(JSON.stringify({ message: "Chinook OVDB upstream failed", path: publicURL.pathname, error: error instanceof Error ? error.message : "unexpected error" }));
    return jsonResponse({ error: "Chinook OVDB is temporarily unavailable." }, 503);
  }
}
