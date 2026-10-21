// Immutable embedded asset policy inputs. Actual HTTP behavior is exercised
// by the native gateway httptest suite.
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";


const read = (path) => readFile(new URL(`../${path}`, import.meta.url), "utf8");
test("_redirects sends the bare origin to the device page", async () => {
  const rules = (await read("server/gatewayassets/public/_redirects")).split("\n").filter((line) => line.trim() && !line.startsWith("#"));
  assert.deepEqual(rules, ["/ /device/ 302"]);
});

test("_headers carries the page security headers and the discovery metadata type", async () => {
  const headers = await read("server/gatewayassets/public/_headers");
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
  assert.deepEqual(JSON.parse(await read("server/gatewayassets/public/.well-known/oauth-authorization-server")), {
    issuer: origin,
    device_authorization_endpoint: `${origin}/oauth/device/code`,
    token_endpoint: `${origin}/oauth/token`,
    revocation_endpoint: `${origin}/oauth/revoke`,
    grant_types_supported: ["urn:ietf:params:oauth:grant-type:device_code"],
    token_endpoint_auth_methods_supported: ["none"],
    scopes_supported: ["account:read", "databases:read"],
  });
});
