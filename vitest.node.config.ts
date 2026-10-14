import { defineConfig } from "vitest/config";

// Plain Node project: PGlite (real PostgreSQL semantics) cannot run inside the Workers test pool.
export default defineConfig({
  test: { environment: "node", include: ["test-node/**/*.test.ts"], testTimeout: 60_000, hookTimeout: 120_000 },
});
