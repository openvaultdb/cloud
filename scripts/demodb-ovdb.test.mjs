import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

import { COMMITTED_PATH, deriveDemoDBOVDB, serialise, validateDemoDBOVDB } from "./demodb-ovdb.mjs";

const sample = {
  server: {
    format: "ovdb-server/draft-1",
    id: "https://demodb.dev/ovdb",
    apiUrl: "https://demodb.dev/ovdb/v1",
    databases: [
      { id: "https://demodb.dev/chinook/", localId: "chinook", apiUrl: "https://demodb.dev/ovdb/v1/databases/chinook" },
      { id: "https://demodb.dev/pubs/", localId: "pubs", apiUrl: "https://demodb.dev/ovdb/v1/databases/pubs" },
    ],
  },
  databases: [
    { localId: "chinook", capabilities: { read: true, query: true, write: false }, recordsets: [] },
    { localId: "pubs", capabilities: { read: true, query: false, write: false }, recordsets: [] },
  ],
};

test("derives the server index and per-database query capability", () => {
  assert.deepEqual(deriveDemoDBOVDB(sample), { server: sample.server, query: { chinook: true, pubs: false } });
});

test("refuses inputs the gateway could not serve correctly", () => {
  assert.throws(() => deriveDemoDBOVDB({}), /missing server.databases/u);
  const mismatched = structuredClone(sample);
  mismatched.databases.pop();
  assert.throws(() => deriveDemoDBOVDB(mismatched), /same localIds/u);
  const badApi = structuredClone(sample);
  badApi.server.databases[0].apiUrl = "https://cloud.openvaultdb.com/v1/databases/chinook";
  assert.throws(() => deriveDemoDBOVDB(badApi), /apiUrl must be/u);
  const badId = structuredClone(sample);
  badId.databases[0].localId = "Chinook";
  assert.throws(() => deriveDemoDBOVDB(badId), /invalid localId/u);
});

test("the committed allowlist is valid and canonically formatted", async () => {
  const text = await readFile(COMMITTED_PATH, "utf8");
  const committed = JSON.parse(text);
  validateDemoDBOVDB(committed);
  assert.equal(serialise(committed), text);
});
