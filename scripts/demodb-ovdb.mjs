// Keeps server/gatewayassets/demodb-ovdb.json, the DemoDB OVDB allowlist the Go gateway serves at
// https://ovdb.demodb.dev/v1, in step with its source of truth: the
// src/data/generated/ovdb.json that demo-db/websites `npm run prepare:databases`
// writes. The Go gateway has no runtime dependency on the demo-db repository.
//
//   node scripts/demodb-ovdb.mjs check <path/to/demo-db/websites/src/data/generated/ovdb.json>
//   node scripts/demodb-ovdb.mjs write <same path>
//
// `check` exits 1 when the committed copy has drifted; `write` refreshes it.
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repositoryRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
export const COMMITTED_PATH = path.join(repositoryRoot, "server", "gatewayassets", "demodb-ovdb.json");
const localIdPattern = /^[a-z][a-z0-9-]{0,39}$/u;

/** The part of demo-db's generated ovdb.json the gateway needs: the server index and per-database query capability. */
export function deriveDemoDBOVDB(generated) {
  const server = generated?.server;
  const databases = generated?.databases;
  if (!server || !Array.isArray(server.databases) || !Array.isArray(databases)) {
    throw new Error("not a demo-db generated ovdb.json: missing server.databases or databases");
  }
  const query = {};
  for (const database of databases) {
    if (!localIdPattern.test(database?.localId ?? "")) throw new Error(`invalid localId ${JSON.stringify(database?.localId)}`);
    if (typeof database.capabilities?.query !== "boolean") throw new Error(`${database.localId}: capabilities.query must be boolean`);
    query[database.localId] = database.capabilities.query;
  }
  const result = { server, query };
  validateDemoDBOVDB(result);
  return result;
}

// demo-db advertises the API at its former home or at the dedicated host; the
// Worker rewrites both to https://ovdb.demodb.dev/v1 when it serves the index.
const apiBases = ["https://demodb.dev/ovdb/v1", "https://ovdb.demodb.dev/v1"];

/** Invariants the Go gateway relies on; scripts/demodb-ovdb.test.mjs checks the committed copy with the same rules. */
export function validateDemoDBOVDB({ server, query }) {
  if (!apiBases.includes(server.apiUrl)) throw new Error(`server.apiUrl must be one of ${apiBases.join(", ")}`);
  const ids = server.databases.map((database) => database.localId);
  if (ids.length === 0 || new Set(ids).size !== ids.length) throw new Error("server.databases must list each database once");
  if (JSON.stringify([...ids].sort()) !== JSON.stringify(Object.keys(query).sort())) {
    throw new Error("server.databases and databases must name the same localIds");
  }
  for (const database of server.databases) {
    if (!localIdPattern.test(database.localId)) throw new Error(`invalid localId ${JSON.stringify(database.localId)}`);
    if (database.apiUrl !== `${server.apiUrl}/databases/${database.localId}`) {
      throw new Error(`${database.localId}: apiUrl must be ${server.apiUrl}/databases/${database.localId}`);
    }
  }
}

export function serialise(value) {
  return `${JSON.stringify(value, null, 2)}\n`;
}

async function main([command, source]) {
  if (!["check", "write"].includes(command) || !source) {
    console.error("usage: node scripts/demodb-ovdb.mjs check|write <demo-db/websites/src/data/generated/ovdb.json>");
    return 2;
  }
  const expected = serialise(deriveDemoDBOVDB(JSON.parse(await readFile(source, "utf8"))));
  if (command === "write") {
    await writeFile(COMMITTED_PATH, expected);
    console.log(`wrote ${path.relative(repositoryRoot, COMMITTED_PATH)}`);
    return 0;
  }
  const actual = await readFile(COMMITTED_PATH, "utf8");
  if (actual !== expected) {
    console.error(`${path.relative(repositoryRoot, COMMITTED_PATH)} has drifted from ${source}; run: node scripts/demodb-ovdb.mjs write ${source}`);
    return 1;
  }
  console.log(`${path.relative(repositoryRoot, COMMITTED_PATH)} matches ${source}`);
  return 0;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = await main(process.argv.slice(2));
}
