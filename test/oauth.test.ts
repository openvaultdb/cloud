import { env } from "cloudflare:workers";
import { createExecutionContext, waitOnExecutionContext } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";

import { cleanupExpiredState } from "../src/oauth";
import { createWorker } from "../src/worker";

const baseURL = "https://cloud.openvaultdb.com";
const worker = createWorker(async (token, projectID) => {
  if (token !== "valid-firebase-token" || projectID !== "sneat-eur3-1") {
    throw new Error("invalid test identity");
  }
  return {
    subject: "sneat-user-1",
    email: "alex@example.com",
    name: "Alex",
  };
});
const fetchWorker = worker.fetch as unknown as (
  request: Request,
  environment: Env,
  context: ExecutionContext,
) => Response | Promise<Response>;

beforeEach(async () => {
  await env.DB.batch([
    env.DB.prepare("DELETE FROM access_tokens"),
    env.DB.prepare("DELETE FROM device_authorizations"),
  ]);
});

describe("device authorization", () => {
  it("serves the approval page with browser security headers", async () => {
    const redirect = await call("/device");
    expect(redirect.status).toBe(302);
    expect(redirect.headers.get("Location")).toBe(`${baseURL}/device/`);

    const page = await call("/device/");
    expect(page.status).toBe(200);
    expect(page.headers.get("Content-Security-Policy")).toContain(
      "frame-src https://auth.sneat.co",
    );
    expect(page.headers.get("Content-Security-Policy")).toContain("frame-ancestors 'none'");
    const pageHTML = await page.text();
    expect(pageHTML).toContain("Connect your command line");
    expect(pageHTML).toContain("OpenVaultDB is a Sneat Co. product");
  });

  it("approves, exchanges once, authenticates, and revokes", async () => {
    const authorization = await startAuthorization();

    expect(authorization.verification_uri).toBe(`${baseURL}/device`);
    expect(authorization.verification_uri_complete).toContain(authorization.user_code);
    expect(authorization.expires_in).toBe(600);
    expect(authorization.interval).toBe(5);

    const persisted = await env.DB.prepare(
      "SELECT device_code_hash, user_code_hash FROM device_authorizations",
    ).first<{ device_code_hash: string; user_code_hash: string }>();
    expect(persisted?.device_code_hash).not.toContain(authorization.device_code);
    expect(persisted?.user_code_hash).not.toContain(authorization.user_code);

    const preview = await call(
      `/api/device-authorization?user_code=${encodeURIComponent(authorization.user_code)}`,
    );
    expect(preview.status).toBe(200);
    expect(await preview.json()).toMatchObject({
      client: { id: "ovdb-cli", name: "OpenVaultDB CLI" },
      scopes: [{ name: "account:read", risk: "low" }],
    });

    const decision = await call("/api/device-authorization/decision", {
      method: "POST",
      headers: {
        Authorization: "Bearer valid-firebase-token",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        user_code: authorization.user_code,
        decision: "approve",
      }),
    });
    expect(decision.status).toBe(200);

    const tokenResponse = await exchange(authorization.device_code);
    expect(tokenResponse.status).toBe(200);
    const token = await tokenResponse.json<{
      access_token: string;
      expires_in: number;
      scope: string;
      token_type: string;
    }>();
    expect(token.access_token).toMatch(/^ovdb_[A-Za-z0-9_-]+$/u);
    expect(token.expires_in).toBe(31_536_000);
    expect(token.scope).toBe("account:read");
    expect(token.token_type).toBe("Bearer");

    const replay = await exchange(authorization.device_code);
    expect(replay.status).toBe(400);
    expect(await replay.json()).toMatchObject({ error: "invalid_grant" });

    const ctx = createExecutionContext();
    const userInfo = await fetchWorker(
      new Request(`${baseURL}/oauth/userinfo`, {
        headers: { Authorization: `Bearer ${token.access_token}` },
      }),
      env,
      ctx,
    );
    expect(userInfo.status).toBe(200);
    expect(await userInfo.json()).toMatchObject({
      sub: "sneat-user-1",
      email: "alex@example.com",
      name: "Alex",
      client_id: "ovdb-cli",
      scope: "account:read",
    });
    await waitOnExecutionContext(ctx);

    const revoke = await call("/oauth/revoke", {
      method: "POST",
      headers: {
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body: new URLSearchParams({ token: token.access_token, client_id: "ovdb-cli" }),
    });
    expect(revoke.status).toBe(200);

    const revoked = await call("/oauth/userinfo", {
      headers: { Authorization: `Bearer ${token.access_token}` },
    });
    expect(revoked.status).toBe(401);
    expect(await revoked.json()).toMatchObject({ error: "invalid_token" });
  });

  it("enforces polling intervals and reports denial", async () => {
    const pending = await startAuthorization();
    const firstPoll = await exchange(pending.device_code);
    expect(await firstPoll.json()).toMatchObject({ error: "authorization_pending" });

    const fastPoll = await exchange(pending.device_code);
    expect(await fastPoll.json()).toMatchObject({ error: "slow_down" });

    const denied = await startAuthorization();
    const decision = await call("/api/device-authorization/decision", {
      method: "POST",
      headers: {
        Authorization: "Bearer valid-firebase-token",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ user_code: denied.user_code, decision: "deny" }),
    });
    expect(decision.status).toBe(200);
    const exchangeResponse = await exchange(denied.device_code);
    expect(await exchangeResponse.json()).toMatchObject({ error: "access_denied" });
  });

  it("rejects unknown clients, scopes, and unauthenticated decisions", async () => {
    const unknownClient = await formPost("/oauth/device/code", {
      client_id: "unknown",
      scope: "account:read",
    });
    expect(unknownClient.status).toBe(401);
    expect(await unknownClient.json()).toMatchObject({ error: "invalid_client" });

    const invalidScope = await formPost("/oauth/device/code", {
      client_id: "ovdb-cli",
      scope: "account:write",
    });
    expect(invalidScope.status).toBe(400);
    expect(await invalidScope.json()).toMatchObject({ error: "invalid_scope" });

    const authorization = await startAuthorization();
    const decision = await call("/api/device-authorization/decision", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        user_code: authorization.user_code,
        decision: "approve",
      }),
    });
    expect(decision.status).toBe(401);
  });

  it("issues exactly one token under concurrent exchanges", async () => {
    const authorization = await startAuthorization();
    await call("/api/device-authorization/decision", {
      method: "POST",
      headers: {
        Authorization: "Bearer valid-firebase-token",
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        user_code: authorization.user_code,
        decision: "approve",
      }),
    });

    const responses = await Promise.all([
      exchange(authorization.device_code),
      exchange(authorization.device_code),
    ]);
    expect(responses.map((response) => response.status).sort()).toEqual([200, 400]);
    const count = await env.DB.prepare("SELECT COUNT(*) AS count FROM access_tokens")
      .first<{ count: number }>();
    expect(count?.count).toBe(1);
  });

  it("expires active grants and deletes retained stale state", async () => {
    const now = Math.floor(Date.now() / 1000);
    await env.DB.batch([
      env.DB
        .prepare(
          `INSERT INTO device_authorizations (
            id, device_code_hash, user_code_hash, client_id, scopes, status,
            created_at, expires_at, poll_interval_seconds
          ) VALUES (?, ?, ?, 'ovdb-cli', 'account:read', 'pending', ?, ?, 5)`,
        )
        .bind("recent", "recent-device", "recent-user", now - 120, now - 60),
      env.DB
        .prepare(
          `INSERT INTO device_authorizations (
            id, device_code_hash, user_code_hash, client_id, scopes, status,
            created_at, expires_at, poll_interval_seconds
          ) VALUES (?, ?, ?, 'ovdb-cli', 'account:read', 'pending', ?, ?, 5)`,
        )
        .bind("stale", "stale-device", "stale-user", now - 200_000, now - 100_000),
    ]);

    await cleanupExpiredState(env.DB, now);

    const recent = await env.DB.prepare(
      "SELECT status FROM device_authorizations WHERE id = 'recent'",
    ).first<{ status: string }>();
    expect(recent?.status).toBe("expired");
    const stale = await env.DB.prepare(
      "SELECT status FROM device_authorizations WHERE id = 'stale'",
    ).first<{ status: string }>();
    expect(stale).toBeNull();
  });
});

interface DeviceAuthorization {
  device_code: string;
  user_code: string;
  verification_uri: string;
  verification_uri_complete: string;
  expires_in: number;
  interval: number;
}

async function startAuthorization(): Promise<DeviceAuthorization> {
  const response = await formPost("/oauth/device/code", {
    client_id: "ovdb-cli",
    scope: "account:read",
  });
  expect(response.status).toBe(200);
  return response.json<DeviceAuthorization>();
}

function exchange(deviceCode: string): Promise<Response> {
  return formPost("/oauth/token", {
    grant_type: "urn:ietf:params:oauth:grant-type:device_code",
    device_code: deviceCode,
    client_id: "ovdb-cli",
  });
}

function formPost(pathname: string, values: Record<string, string>): Promise<Response> {
  return call(pathname, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams(values),
  });
}

function call(pathname: string, init?: RequestInit): Promise<Response> {
  return fetchWorker(
    new Request(`${baseURL}${pathname}`, init),
    env,
    createExecutionContext(),
  ) as Promise<Response>;
}
