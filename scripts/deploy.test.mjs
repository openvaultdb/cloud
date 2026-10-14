import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { mkdir, mkdtemp, readFile, readdir, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { prepareDeploymentConfig, runDeployment, transferNeonSecret } from "./deploy.mjs";
import { prepareDalgoCore } from "./prepare-dalgo-core.mjs";

const baseConfig = { name: "worker", vars: { KEEP: "yes" } };
const readerURL = "postgresql://northwind_reader:private-value@ep-test.us-west-2.aws.neon.tech/northwind?sslmode=require";

test("deployment configuration keeps optional bindings absent unless explicitly configured", () => {
  const { config, neonURL } = prepareDeploymentConfig(baseConfig, {});
  assert.equal(neonURL, undefined);
  assert.equal(Object.hasOwn(config, "d1_databases"), false);
  assert.deepEqual(config.vars, baseConfig.vars);

  const configured = prepareDeploymentConfig(baseConfig, {
    NORTHWIND_D1_DATABASE_ID: "123e4567-e89b-42d3-a456-426614174000",
  });
  assert.deepEqual(configured.config.d1_databases, [{
    binding: "NORTHWIND_D1",
    database_name: "demodb-northwind",
    database_id: "123e4567-e89b-42d3-a456-426614174000",
  }]);
});

test("pinned DALgo core preparation builds the immutable source package before imports are used", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "northwind-core-test-"));
  try {
    const core = path.join(root, "node_modules", "@dalgo", "core");
    await mkdir(core, { recursive: true });
    await writeFile(path.join(core, "package.json"), JSON.stringify({ name: "@dalgo/core", version: "0.2.0" }));
    await writeFile(path.join(core, "tsconfig.build.json"), JSON.stringify({ compilerOptions: {} }));
    let invocation;
    await prepareDalgoCore(root, async (command, args, options) => { invocation = { command, args, options }; });
    assert.equal(invocation.command, path.join(root, "node_modules", ".bin", "tsc"));
    assert.deepEqual(invocation.args, ["-p", path.join(core, "tsconfig.build.json")]);
    assert.equal(invocation.options.cwd, root);
    await writeFile(path.join(core, "package.json"), JSON.stringify({ name: "@dalgo/core", version: "0.3.0" }));
    await assert.rejects(prepareDalgoCore(root, async () => {}), /expected pinned DALgo core/u);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});

test("deployment rejects invalid D1 IDs and non-reader or non-Neon connection targets", () => {
  assert.throws(() => prepareDeploymentConfig(baseConfig, { NORTHWIND_D1_DATABASE_ID: "fake-id" }), /database ID is invalid/u);
  assert.throws(() => prepareDeploymentConfig(baseConfig, { NORTHWIND_NEON_DATABASE_URL: "postgresql://admin:secret@db.example.test/northwind" }), /unexpected target/u);
});

test("Neon secret transfer sends the value only through stdin", async () => {
  let observed;
  await transferNeonSecret(readerURL, {
    wrangler: "/fake/wrangler",
    cwd: "/private/tmp",
    environment: { CLOUDFLARE_API_TOKEN: "token", NORTHWIND_NEON_DATABASE_URL: readerURL },
    spawnCommand(command, args, options) {
      const child = new EventEmitter();
      child.stdin = { end(value) { observed.stdin = value; } };
      observed = { command, args, options };
      queueMicrotask(() => child.emit("close", 0));
      return child;
    },
  });
  assert.equal(observed.command, "/fake/wrangler");
  assert.deepEqual(observed.args, ["secret", "put", "NORTHWIND_NEON_DATABASE_URL", "--name", "openvaultdb-cloud"]);
  assert.equal(observed.stdin, readerURL);
  assert.equal(observed.options.stdio[0], "pipe");
  assert.equal(observed.options.stdio[1], "ignore");
  assert.equal(observed.options.stdio[2], "ignore");
  assert.equal(Object.hasOwn(observed.options.env, "NORTHWIND_NEON_DATABASE_URL"), false);
  assert.equal(JSON.stringify(observed.args).includes(readerURL), false);
});

test("deployment strips secret from config and process environment and cleans its mode-0600 config on failure", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "northwind-deploy-test-"));
  try {
    await writeFile(path.join(root, "wrangler.jsonc"), JSON.stringify(baseConfig));
    let secretTransfer;
    let deployment;
    await assert.rejects(runDeployment({
      NORTHWIND_D1_DATABASE_ID: "123e4567-e89b-42d3-a456-426614174000",
      NORTHWIND_NEON_DATABASE_URL: readerURL,
      CLOUDFLARE_API_TOKEN: "token",
    }, {
      root,
      wrangler: "/fake/wrangler",
      spawnCommand(command, args, options) {
        const child = new EventEmitter();
        child.stdin = { end(value) { secretTransfer = { command, args, options, value }; } };
        queueMicrotask(() => child.emit("close", 0));
        return child;
      },
      async runCommand(command, args, options) {
        const generated = args[args.indexOf("--config") + 1];
        const contents = await readFile(generated, "utf8");
        const details = await stat(generated);
        deployment = { command, args, options, generated, contents, mode: details.mode & 0o777 };
        throw new Error("intentional simulated deploy failure");
      },
    }), /intentional simulated deploy failure/u);

    assert.equal(secretTransfer.value, readerURL);
    assert.equal(secretTransfer.args.includes(readerURL), false);
    assert.equal(Object.hasOwn(secretTransfer.options.env, "NORTHWIND_NEON_DATABASE_URL"), false);
    assert.equal(deployment.mode, 0o600);
    assert.equal(deployment.contents.includes(readerURL), false);
    assert.equal(deployment.options.env.NORTHWIND_D1_DATABASE_ID, "123e4567-e89b-42d3-a456-426614174000");
    assert.equal(Object.hasOwn(deployment.options.env, "NORTHWIND_NEON_DATABASE_URL"), false);
    assert.equal(deployment.contents.includes('"database_name":"demodb-northwind"'), true);
    assert.equal((await readdir(root)).some((name) => name.startsWith(".wrangler-northwind-deploy-")), false);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
