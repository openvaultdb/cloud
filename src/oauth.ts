import {
  hashSecret,
  normalizeUserCode,
  randomID,
  randomSecret,
  randomUserCode,
} from "./crypto";
import type { Identity, VerifyIdentity } from "./firebase";
import {
  bearerToken,
  emptyNoStoreResponse,
  jsonResponse,
  oauthError,
  readForm,
} from "./http";

const DEVICE_CODE_TTL_SECONDS = 10 * 60;
const INITIAL_POLL_INTERVAL_SECONDS = 5;
const DEVICE_GRANT_TYPE = "urn:ietf:params:oauth:grant-type:device_code";
const TOKEN_RETENTION_SECONDS = 30 * 24 * 60 * 60;
const DEVICE_AUTHORIZATION_RETENTION_SECONDS = 24 * 60 * 60;

interface OAuthClientRow {
  client_id: string;
  display_name: string;
  allowed_scopes: string;
  default_scopes: string;
  token_ttl_seconds: number;
}

interface DeviceAuthorizationRow {
  id: string;
  client_id: string;
  scopes: string;
  status: "pending" | "approved" | "denied" | "consumed" | "expired";
  expires_at: number;
  poll_interval_seconds: number;
  last_poll_at: number | null;
  user_id: string | null;
  user_email: string | null;
  user_name: string | null;
  token_ttl_seconds: number;
}

interface AccessTokenRow {
  client_id: string;
  user_id: string;
  user_email: string | null;
  user_name: string | null;
  scopes: string;
  expires_at: number;
  revoked_at: number | null;
}

interface DecisionBody {
  user_code: string;
  decision: "approve" | "deny";
}

export async function handleDeviceAuthorization(
  request: Request,
  env: Env,
): Promise<Response> {
  let form: URLSearchParams;
  try {
    form = await readForm(request, ["client_id", "scope"]);
  } catch (error) {
    return oauthError("invalid_request", errorMessage(error));
  }

  const clientID = form.get("client_id") ?? "";
  if (clientID === "") {
    return oauthError("invalid_request", "client_id is required");
  }
  const client = await loadClient(env.DB, clientID);
  if (!client) {
    return oauthError("invalid_client", "unknown OAuth client", 401);
  }

  const requestedScopes = parseScopes(
    form.get("scope") ?? client.default_scopes,
  );
  const allowedScopes = new Set(parseScopes(client.allowed_scopes));
  if (
    requestedScopes.length === 0 ||
    requestedScopes.some((scope) => !allowedScopes.has(scope))
  ) {
    return oauthError(
      "invalid_scope",
      "the requested scope is not registered for this client",
    );
  }

  const now = nowSeconds();
  for (let attempt = 0; attempt < 3; attempt += 1) {
    const id = randomID();
    const deviceCode = randomSecret();
    const userCode = randomUserCode();
    try {
      await env.DB.prepare(
        `INSERT INTO device_authorizations (
          id, device_code_hash, user_code_hash, client_id, scopes, status,
          created_at, expires_at, poll_interval_seconds
        ) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
      )
        .bind(
          id,
          await hashSecret(deviceCode),
          await hashSecret(userCode),
          client.client_id,
          requestedScopes.join(" "),
          now,
          now + DEVICE_CODE_TTL_SECONDS,
          INITIAL_POLL_INTERVAL_SECONDS,
        )
        .run();

      const verificationURI = `${env.PUBLIC_ORIGIN}/device`;
      return jsonResponse({
        device_code: deviceCode,
        user_code: userCode,
        verification_uri: verificationURI,
        verification_uri_complete: `${verificationURI}?user_code=${encodeURIComponent(userCode)}`,
        expires_in: DEVICE_CODE_TTL_SECONDS,
        interval: INITIAL_POLL_INTERVAL_SECONDS,
      });
    } catch (error) {
      if (attempt === 2) {
        throw error;
      }
    }
  }
  throw new Error("unreachable device authorization attempt limit");
}

export async function handleAuthorizationPreview(
  request: Request,
  env: Env,
): Promise<Response> {
  const rawCode = new URL(request.url).searchParams.get("user_code") ?? "";
  const userCode = normalizeUserCode(rawCode);
  if (userCode === "") {
    return jsonResponse({ error: "Enter an eight-character device code." }, 400);
  }
  const now = nowSeconds();
  const authorization = await env.DB.prepare(
    `SELECT d.status, d.scopes, d.expires_at, c.client_id, c.display_name
       FROM device_authorizations d
       JOIN oauth_clients c ON c.client_id = d.client_id
      WHERE d.user_code_hash = ?`,
  )
    .bind(await hashSecret(userCode))
    .first<{
      status: DeviceAuthorizationRow["status"];
      scopes: string;
      expires_at: number;
      client_id: string;
      display_name: string;
    }>();

  if (!authorization) {
    return jsonResponse({ error: "That device code is not valid." }, 404);
  }
  if (authorization.expires_at <= now || authorization.status === "expired") {
    await expireAuthorization(env.DB, await hashSecret(userCode), now);
    return jsonResponse({ error: "That device code has expired." }, 410);
  }
  if (authorization.status !== "pending") {
    return jsonResponse({ error: "That device code has already been used." }, 409);
  }

  return jsonResponse({
    user_code: userCode,
    client: {
      id: authorization.client_id,
      name: authorization.display_name,
    },
    scopes: parseScopes(authorization.scopes).map(scopeDetail),
    expires_at: new Date(authorization.expires_at * 1000).toISOString(),
  });
}

export async function handleAuthorizationDecision(
  request: Request,
  env: Env,
  verifyIdentity: VerifyIdentity,
): Promise<Response> {
  const idToken = bearerToken(request);
  if (idToken === "") {
    return jsonResponse({ error: "Sign in before deciding this request." }, 401);
  }

  let body: unknown;
  try {
    body = await request.json();
  } catch {
    return jsonResponse({ error: "The decision body must be valid JSON." }, 400);
  }
  if (!isDecisionBody(body)) {
    return jsonResponse({ error: "user_code and decision are required." }, 400);
  }
  const userCode = normalizeUserCode(body.user_code);
  if (userCode === "") {
    return jsonResponse({ error: "The device code is invalid." }, 400);
  }

  let identity: Identity;
  try {
    identity = await verifyIdentity(idToken, env.FIREBASE_PROJECT_ID);
  } catch (error) {
    console.error(
      JSON.stringify({
        message: "Firebase ID token verification failed",
        error: errorMessage(error),
      }),
    );
    return jsonResponse({ error: "Your sign-in session is not valid." }, 401);
  }

  const now = nowSeconds();
  const status = body.decision === "approve" ? "approved" : "denied";
  const result = await env.DB.prepare(
    `UPDATE device_authorizations
        SET status = ?, user_id = ?, user_email = ?, user_name = ?, decided_at = ?
      WHERE user_code_hash = ? AND status = 'pending' AND expires_at > ?`,
  )
    .bind(
      status,
      identity.subject,
      identity.email ?? null,
      identity.name ?? null,
      now,
      await hashSecret(userCode),
      now,
    )
    .run();
  if (result.meta.changes !== 1) {
    return jsonResponse(
      { error: "This device authorization is expired or already decided." },
      409,
    );
  }

  return jsonResponse({ decision: body.decision, user_code: userCode });
}

export async function handleTokenExchange(
  request: Request,
  env: Env,
): Promise<Response> {
  let form: URLSearchParams;
  try {
    form = await readForm(request, [
      "grant_type",
      "device_code",
      "client_id",
    ]);
  } catch (error) {
    return oauthError("invalid_request", errorMessage(error));
  }
  if (form.get("grant_type") !== DEVICE_GRANT_TYPE) {
    return oauthError("unsupported_grant_type", "unsupported grant_type");
  }
  const clientID = form.get("client_id") ?? "";
  const deviceCode = form.get("device_code") ?? "";
  if (clientID === "" || deviceCode === "") {
    return oauthError(
      "invalid_request",
      "client_id and device_code are required",
    );
  }

  const deviceHash = await hashSecret(deviceCode);
  const authorization = await env.DB.prepare(
    `SELECT d.id, d.client_id, d.scopes, d.status, d.expires_at,
            d.poll_interval_seconds, d.last_poll_at, d.user_id, d.user_email,
            d.user_name, c.token_ttl_seconds
       FROM device_authorizations d
       JOIN oauth_clients c ON c.client_id = d.client_id
      WHERE d.device_code_hash = ? AND d.client_id = ?`,
  )
    .bind(deviceHash, clientID)
    .first<DeviceAuthorizationRow>();
  if (!authorization) {
    return oauthError("expired_token", "device authorization is not valid");
  }

  const now = nowSeconds();
  if (authorization.expires_at <= now || authorization.status === "expired") {
    await expireAuthorization(env.DB, deviceHash, now, true);
    return oauthError("expired_token", "device authorization has expired");
  }
  if (authorization.status === "pending") {
    return handlePendingPoll(env.DB, authorization, now);
  }
  if (authorization.status === "denied") {
    return oauthError("access_denied", "the user denied this authorization");
  }
  if (authorization.status !== "approved" || !authorization.user_id) {
    return oauthError("invalid_grant", "device authorization was already used");
  }

  const accessToken = randomSecret("ovdb_");
  const tokenHash = await hashSecret(accessToken);
  const tokenID = randomID();
  const expiresAt = now + authorization.token_ttl_seconds;
  try {
    const [consume, issue] = await env.DB.batch([
      env.DB
        .prepare(
          `UPDATE device_authorizations
              SET status = 'consumed', consumed_at = ?
            WHERE id = ? AND status = 'approved'`,
        )
        .bind(now, authorization.id),
      env.DB
        .prepare(
          `INSERT INTO access_tokens (
            id, authorization_id, token_hash, client_id, user_id, user_email,
            user_name, scopes, created_at, expires_at
          ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        )
        .bind(
          tokenID,
          authorization.id,
          tokenHash,
          authorization.client_id,
          authorization.user_id,
          authorization.user_email,
          authorization.user_name,
          authorization.scopes,
          now,
          expiresAt,
        ),
    ]);
    if (consume.meta.changes !== 1 || issue.meta.changes !== 1) {
      return oauthError("invalid_grant", "device authorization was already used");
    }
  } catch (error) {
    console.error(
      JSON.stringify({
        message: "Device token exchange was not issued",
        authorization_id: authorization.id,
        error: errorMessage(error),
      }),
    );
    return oauthError("invalid_grant", "device authorization was already used");
  }

  return jsonResponse({
    access_token: accessToken,
    token_type: "Bearer",
    expires_in: authorization.token_ttl_seconds,
    scope: authorization.scopes,
  });
}

export async function handleUserInfo(
  request: Request,
  env: Env,
  ctx: ExecutionContext,
): Promise<Response> {
  const token = bearerToken(request);
  if (token === "") {
    return oauthError("invalid_token", "a bearer token is required", 401);
  }
  const tokenHash = await hashSecret(token);
  const row = await env.DB.prepare(
    `SELECT client_id, user_id, user_email, user_name, scopes, expires_at, revoked_at
       FROM access_tokens
      WHERE token_hash = ?`,
  )
    .bind(tokenHash)
    .first<AccessTokenRow>();
  const now = nowSeconds();
  if (!row || row.revoked_at !== null || row.expires_at <= now) {
    return oauthError("invalid_token", "the bearer token is expired or revoked", 401);
  }

  ctx.waitUntil(
    env.DB.prepare(
      "UPDATE access_tokens SET last_used_at = ? WHERE token_hash = ?",
    )
      .bind(now, tokenHash)
      .run()
      .then(() => undefined)
      .catch((error: unknown) => {
        console.error(
          JSON.stringify({
            message: "Could not update access token last-used time",
            error: errorMessage(error),
          }),
        );
      }),
  );

  return jsonResponse({
    sub: row.user_id,
    email: row.user_email,
    name: row.user_name,
    client_id: row.client_id,
    scope: row.scopes,
    expires_at: new Date(row.expires_at * 1000).toISOString(),
  });
}

export async function handleTokenRevocation(
  request: Request,
  env: Env,
): Promise<Response> {
  let token = bearerToken(request);
  if (request.headers.get("Content-Type")?.startsWith("application/x-www-form-urlencoded")) {
    try {
      const form = await readForm(request, ["token", "client_id"]);
      token = form.get("token") ?? token;
    } catch (error) {
      return oauthError("invalid_request", errorMessage(error));
    }
  }
  if (token !== "") {
    await env.DB.prepare(
      "UPDATE access_tokens SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL",
    )
      .bind(nowSeconds(), await hashSecret(token))
      .run();
  }
  return emptyNoStoreResponse();
}

export function authorizationServerMetadata(env: Env): Response {
  return jsonResponse({
    issuer: env.PUBLIC_ORIGIN,
    device_authorization_endpoint: `${env.PUBLIC_ORIGIN}/oauth/device/code`,
    token_endpoint: `${env.PUBLIC_ORIGIN}/oauth/token`,
    revocation_endpoint: `${env.PUBLIC_ORIGIN}/oauth/revoke`,
    grant_types_supported: [DEVICE_GRANT_TYPE],
    token_endpoint_auth_methods_supported: ["none"],
    scopes_supported: ["account:read"],
  });
}

export async function cleanupExpiredState(
  db: D1Database,
  now = nowSeconds(),
): Promise<void> {
  const tokenCutoff = now - TOKEN_RETENTION_SECONDS;
  const authorizationCutoff = now - DEVICE_AUTHORIZATION_RETENTION_SECONDS;
  await db.batch([
    db
      .prepare(
        `UPDATE device_authorizations SET status = 'expired'
          WHERE status IN ('pending', 'approved') AND expires_at <= ?`,
      )
      .bind(now),
    db
      .prepare(
        `DELETE FROM access_tokens
          WHERE expires_at <= ? OR (revoked_at IS NOT NULL AND revoked_at <= ?)`,
      )
      .bind(tokenCutoff, tokenCutoff),
    db
      .prepare(
        `DELETE FROM device_authorizations
          WHERE expires_at <= ?
            AND NOT EXISTS (
              SELECT 1 FROM access_tokens
               WHERE access_tokens.authorization_id = device_authorizations.id
            )`,
      )
      .bind(authorizationCutoff),
  ]);
}

async function loadClient(
  db: D1Database,
  clientID: string,
): Promise<OAuthClientRow | null> {
  return db
    .prepare(
      `SELECT client_id, display_name, allowed_scopes, default_scopes,
              token_ttl_seconds
         FROM oauth_clients
        WHERE client_id = ?`,
    )
    .bind(clientID)
    .first<OAuthClientRow>();
}

async function handlePendingPoll(
  db: D1Database,
  authorization: DeviceAuthorizationRow,
  now: number,
): Promise<Response> {
  const result = await db
    .prepare(
      `UPDATE device_authorizations
          SET last_poll_at = ?
        WHERE id = ? AND status = 'pending'
          AND (last_poll_at IS NULL OR last_poll_at <= ? - poll_interval_seconds)`,
    )
    .bind(now, authorization.id, now)
    .run();
  if (result.meta.changes === 1) {
    return oauthError("authorization_pending", "authorization is still pending");
  }

  await db
    .prepare(
      `UPDATE device_authorizations
          SET poll_interval_seconds = MIN(poll_interval_seconds + 5, 60),
              last_poll_at = ?
        WHERE id = ? AND status = 'pending'`,
    )
    .bind(now, authorization.id)
    .run();
  return oauthError("slow_down", "polling is faster than the permitted interval");
}

async function expireAuthorization(
  db: D1Database,
  hash: string,
  now: number,
  byDeviceCode = false,
): Promise<void> {
  const column = byDeviceCode ? "device_code_hash" : "user_code_hash";
  await db
    .prepare(
      `UPDATE device_authorizations SET status = 'expired'
        WHERE ${column} = ? AND status = 'pending' AND expires_at <= ?`,
    )
    .bind(hash, now)
    .run();
}

function parseScopes(value: string): string[] {
  return [...new Set(value.trim().split(/\s+/u).filter(Boolean))];
}

function scopeDetail(scope: string): {
  name: string;
  description: string;
  risk: "low" | "high";
} {
  if (scope === "account:read") {
    return {
      name: scope,
      description: "Read your OpenVaultDB Cloud account identity.",
      risk: "low",
    };
  }
  return {
    name: scope,
    description: "Use this registered OpenVaultDB Cloud capability.",
    risk: "high",
  };
}

function isDecisionBody(value: unknown): value is DecisionBody {
  if (typeof value !== "object" || value === null) {
    return false;
  }
  const candidate = value as Record<string, unknown>;
  return (
    typeof candidate.user_code === "string" &&
    (candidate.decision === "approve" || candidate.decision === "deny")
  );
}

function nowSeconds(): number {
  return Math.floor(Date.now() / 1000);
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : "unexpected error";
}
