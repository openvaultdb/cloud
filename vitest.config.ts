import path from "node:path";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

const root = path.dirname(fileURLToPath(import.meta.url));
const northwindSeedPath = process.env.NORTHWIND_D1_SEED_SQL;
const northwindSeedSql = northwindSeedPath ? readFileSync(northwindSeedPath, "utf8") : "";
if (northwindSeedSql) {
  const sha256 = createHash("sha256").update(northwindSeedSql).digest("hex");
  if (sha256 !== "fea7d05421f0297adc18c41196f22b498c669abf78c2392b50343091df8810d7") {
    throw new Error("Northwind D1 test import does not match the pinned hosting artifact.");
  }
}
let northwindNeonTestURL = "";
const northwindNeonConnectionFile = process.env.NORTHWIND_NEON_CONNECTION_FILE;
if (northwindNeonConnectionFile) {
  try {
    northwindNeonTestURL = readFileSync(northwindNeonConnectionFile, "utf8").trim();
  } catch {
    throw new Error("Northwind Neon test connection file could not be read.");
  }
  if (!northwindNeonTestURL) throw new Error("Northwind Neon test connection file is empty.");
}

export default defineConfig({
  define: { __NORTHWIND_D1_SEED_SQL__: JSON.stringify(northwindSeedSql) },
  test: { include: ["test/**/*.test.{js,mjs,cjs,ts,mts,cts,jsx,tsx}"] },
  plugins: [
    cloudflareTest(() => ({
      wrangler: { configPath: path.join(root, "wrangler.jsonc") },
      miniflare: {
        bindings: {
          OVDB_DEVICE_AUTH_PROXY_SECRET: "test-proxy-secret",
          NORTHWIND_NEON_TEST_URL: northwindNeonTestURL,
          NORTHWIND_NEON_TEST_ENABLED: northwindNeonTestURL.length > 0 ? "true" : "false",
          ...(process.env.ECB_PUBLIC_CHAIN_ADMISSION ? { ECB_PUBLIC_CHAIN_ADMISSION: process.env.ECB_PUBLIC_CHAIN_ADMISSION, ECB_PUBLIC_CHAIN_DIGEST: process.env.ECB_PUBLIC_CHAIN_DIGEST } : {}),
          ...(process.env.ECB_CHAIN_BRIDGE_URL ? { ECB_CHAIN_BRIDGE_URL: process.env.ECB_CHAIN_BRIDGE_URL } : {}),
          ...(process.env.IANA_CHAIN_BRIDGE_URL ? { IANA_CHAIN_BRIDGE_URL: process.env.IANA_CHAIN_BRIDGE_URL } : {}),
        },
        d1Databases: {
          NORTHWIND_D1: "local-test-northwind",
        },
      },
    })),
  ],
});
