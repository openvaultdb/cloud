import { jsonResponse } from "./http";

export const PROXY_SECRET_HEADER = "X-OVDB-Proxy-Secret";

export type DeviceAuthEnv = Env & {
  OVDB_DEVICE_AUTH_PROXY_SECRET: string;
};

export type UpstreamFetch = (
  input: RequestInfo | URL,
  init?: RequestInit,
) => Promise<Response>;

export async function proxyDeviceAuthorization(
  request: Request,
  env: DeviceAuthEnv,
  upstreamPath: string,
  upstreamFetch: UpstreamFetch,
): Promise<Response> {
  const upstreamURL = new URL(env.SNEAT_API_ORIGIN);
  upstreamURL.pathname = upstreamPath;
  upstreamURL.search = new URL(request.url).search;

  const headers = new Headers();
  headers.set("Accept", request.headers.get("Accept") ?? "application/json");
  headers.set(PROXY_SECRET_HEADER, env.OVDB_DEVICE_AUTH_PROXY_SECRET);
  for (const name of ["Authorization", "Content-Type"] as const) {
    const value = request.headers.get(name);
    if (value) headers.set(name, value);
  }

  const body =
    request.method === "GET" || request.method === "HEAD"
      ? undefined
      : request.body ?? undefined;

  try {
    const upstream = await upstreamFetch(upstreamURL, {
      method: request.method,
      headers,
      body,
      redirect: "manual",
    });
    const responseHeaders = new Headers({
      "Cache-Control": "no-store",
      Pragma: "no-cache",
      "Referrer-Policy": "no-referrer",
      "X-Content-Type-Options": "nosniff",
    });
    for (const name of ["Content-Type", "Retry-After", "WWW-Authenticate"] as const) {
      const value = upstream.headers.get(name);
      if (value) responseHeaders.set(name, value);
    }
    return new Response(upstream.body, {
      status: upstream.status,
      statusText: upstream.statusText,
      headers: responseHeaders,
    });
  } catch (error) {
    console.error(
      JSON.stringify({
        message: "OpenVaultDB device authorization backend request failed",
        method: request.method,
        path: upstreamPath,
        error: error instanceof Error ? error.message : "unexpected error",
      }),
    );
    return jsonResponse(
      {
        error: "temporarily_unavailable",
        error_description: "OpenVaultDB Cloud could not reach its authorization service.",
      },
      503,
    );
  }
}
