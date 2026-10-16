// Static-first routing: only the paths that need code run the Worker; the rest
// are free static assets with their redirects and headers in public/.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { parseJSONC } from "./jsonc.mjs";

const read = (path) => readFile(new URL(`../${path}`, import.meta.url), "utf8");
const config = parseJSONC(await read("wrangler.jsonc"));
const patterns = config.assets.run_worker_first;

/** Cloudflare run_worker_first glob: `*` matches any characters, including `/`. */
function runsWorkerFirst(pathname) {
  return patterns.some((pattern) => {
    const source = pattern.split("*").map((part) => part.replace(/[.+?^${}()|[\]\\]/gu, "\\$&")).join(".*");
    return new RegExp(`^${source}$`, "u").test(pathname);
  });
}

test("run_worker_first is an array of the dynamic paths only", () => {
  assert.ok(Array.isArray(patterns));
  assert.equal(config.assets.not_found_handling, "404-page");
  for (const dynamic of [
    "/v1", "/v1/", "/v1/dtql", "/v1/databases/chinook/dtql", "/ovdb", "/ovdb/dbs/chinook", "/.well-known/openvaultdb",
    "/oauth/device/code", "/oauth/token", "/oauth/userinfo", "/oauth/revoke",
    "/api/device-authorization", "/api/device-authorization/decision", "/api/devices", "/api/devices/revoke",
    "/api/databases", "/api/databases/db_1",
    "/iana/v1/databases/iana-http-status/dtql", "/ecb/v1/databases/ecb/dtql",
    "/ecb-public", "/ecb-public/v1/databases/ecb/dtql",
  ]) assert.ok(runsWorkerFirst(dynamic), dynamic);
  for (const asset of [
    "/", "/device", "/device/", "/devices", "/devices/", "/device.js", "/devices.js", "/styles.css",
    "/firebase-auth.js", "/404.html", "/.well-known/oauth-authorization-server", "/missing",
    // The retired Northwind D1/Neon mirrors are static 404s now.
    "/northwind/d1/v1/metadata", "/northwind/neon/v1/query",
  ]) assert.ok(!runsWorkerFirst(asset), asset);
});

test("_redirects sends the bare origin to the device page", async () => {
  const rules = (await read("public/_redirects")).split("\n").filter((line) => line.trim() && !line.startsWith("#"));
  assert.deepEqual(rules, ["/ /device/ 302"]);
});

test("_headers carries the page security headers and the discovery metadata type", async () => {
  const headers = await read("public/_headers");
  const all = headers.slice(headers.indexOf("/*\n"), headers.indexOf("\n\n", headers.indexOf("/*\n")));
  for (const want of [
    "Referrer-Policy: no-referrer", "X-Content-Type-Options: nosniff", "X-Frame-Options: DENY",
    "Content-Security-Policy: default-src 'self'; script-src 'self' https://www.gstatic.com https://apis.google.com;",
    "frame-src https://auth.sneat.co;", "frame-ancestors 'none'",
  ]) assert.ok(all.includes(want), want);
  assert.match(headers, /\/\.well-known\/oauth-authorization-server\n {2}Content-Type: application\/json; charset=utf-8\n {2}Cache-Control: no-store/u);
  assert.match(headers, /https:\/\/ovdb\.demodb\.dev\/\*\n {2}X-Robots-Tag: noindex/u);
});

test("OAuth discovery metadata names the public origin", async () => {
  const origin = "https://cloud.openvaultdb.com";
  assert.deepEqual(JSON.parse(await read("public/.well-known/oauth-authorization-server")), {
    issuer: origin,
    device_authorization_endpoint: `${origin}/oauth/device/code`,
    token_endpoint: `${origin}/oauth/token`,
    revocation_endpoint: `${origin}/oauth/revoke`,
    grant_types_supported: ["urn:ietf:params:oauth:grant-type:device_code"],
    token_endpoint_auth_methods_supported: ["none"],
    scopes_supported: ["account:read", "databases:read"],
  });
});

test("device-auth calls go straight to Cloud Run, not through the api.sneat.cloud Worker", () => {
  assert.match(config.vars.SNEAT_API_ORIGIN, /^https:\/\/[a-z0-9-]+\.a\.run\.app$/u);
});
