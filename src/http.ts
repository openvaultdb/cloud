const BASE_HEADERS: HeadersInit = {
  "Cache-Control": "no-store",
  "Content-Type": "application/json; charset=utf-8",
  Pragma: "no-cache",
  "Referrer-Policy": "no-referrer",
  "X-Content-Type-Options": "nosniff",
};

export function jsonResponse(value: unknown, status = 200): Response {
  return Response.json(value, { status, headers: BASE_HEADERS });
}

export function oauthError(
  error: string,
  description: string,
  status = 400,
): Response {
  return jsonResponse({ error, error_description: description }, status);
}

export function emptyNoStoreResponse(status = 200): Response {
  return new Response(null, { status, headers: BASE_HEADERS });
}

export function methodNotAllowed(allowed: string): Response {
  return new Response("Method not allowed", {
    status: 405,
    headers: {
      Allow: allowed,
      "Cache-Control": "no-store",
      "Content-Type": "text/plain; charset=utf-8",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

export async function readForm(
  request: Request,
  uniqueFields: readonly string[],
): Promise<URLSearchParams> {
  const contentType = request.headers.get("Content-Type")?.split(";", 1)[0];
  if (contentType !== "application/x-www-form-urlencoded") {
    throw new Error("content type must be application/x-www-form-urlencoded");
  }
  const contentLength = Number(request.headers.get("Content-Length") ?? "0");
  if (Number.isFinite(contentLength) && contentLength > 8192) {
    throw new Error("request body is too large");
  }
  const body = new TextDecoder().decode(await request.arrayBuffer());
  if (body.length > 8192) {
    throw new Error("request body is too large");
  }
  const form = new URLSearchParams(body);
  for (const field of uniqueFields) {
    if (form.getAll(field).length > 1) {
      throw new Error(`${field} must not be repeated`);
    }
  }
  return form;
}

export function bearerToken(request: Request): string {
  const authorization = request.headers.get("Authorization") ?? "";
  const match = /^Bearer ([^\s]+)$/iu.exec(authorization);
  return match?.[1] ?? "";
}

export function withAssetSecurityHeaders(response: Response): Response {
  const headers = new Headers(response.headers);
  headers.set("Referrer-Policy", "no-referrer");
  headers.set("X-Content-Type-Options", "nosniff");
  headers.set("X-Frame-Options", "DENY");
  headers.set(
    "Content-Security-Policy",
    "default-src 'self'; script-src 'self' https://www.gstatic.com https://apis.google.com; " +
      "connect-src 'self' https://www.gstatic.com https://*.googleapis.com https://securetoken.googleapis.com " +
      "https://identitytoolkit.googleapis.com https://auth.sneat.co; " +
      "frame-src https://auth.sneat.co; img-src 'self' data:; " +
      "style-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
  );
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}
