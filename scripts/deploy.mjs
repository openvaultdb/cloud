import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { prepareDalgoCore } from "./prepare-dalgo-core.mjs";

const repositoryRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const configName = (id) => `.wrangler-northwind-deploy-${id}.json`;

function run(command, args, options = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, options);
    child.on("error", () => reject(new Error("Cloud deployment command failed.")));
    child.on("close", (code) => code === 0 ? resolve() : reject(new Error("Cloud deployment command failed.")));
  });
}

function validateNeonURL(value) {
  try {
    const url = new URL(value);
    return ["postgres:", "postgresql:"].includes(url.protocol) &&
      url.hostname.endsWith(".neon.tech") &&
      decodeURIComponent(url.username) === "northwind_reader" &&
      decodeURIComponent(url.pathname.replace(/^\//u, "")) === "northwind";
  } catch {
    return false;
  }
}

export function prepareDeploymentConfig(baseConfig, environment) {
  const config = structuredClone(baseConfig);
  const databaseID = environment.NORTHWIND_D1_DATABASE_ID?.trim();
  if (databaseID) {
    if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/iu.test(databaseID)) {
      throw new Error("Northwind D1 database ID is invalid.");
    }
    config.d1_databases = [{ binding: "NORTHWIND_D1", database_name: "demodb-northwind", database_id: databaseID }];
  } else {
    delete config.d1_databases;
  }
  const neonURL = environment.NORTHWIND_NEON_DATABASE_URL;
  if (neonURL && !validateNeonURL(neonURL)) throw new Error("Northwind reader secret has an unexpected target.");
  return { config, neonURL };
}

export async function transferNeonSecret(value, { spawnCommand = spawn, wrangler, cwd = process.cwd(), environment = process.env }) {
  const childEnvironment = { ...environment };
  delete childEnvironment.NORTHWIND_NEON_DATABASE_URL;
  const child = spawnCommand(wrangler, ["secret", "put", "NORTHWIND_NEON_DATABASE_URL", "--name", "openvaultdb-cloud"], {
    cwd,
    env: childEnvironment,
    stdio: ["pipe", "ignore", "ignore"],
  });
  child.stdin.end(value);
  await new Promise((resolve, reject) => {
    child.on("error", () => reject(new Error("Northwind reader secret transfer failed.")));
    child.on("close", (code) => code === 0 ? resolve() : reject(new Error("Northwind reader secret transfer failed.")));
  });
}

export async function runDeployment(environment, options = {}) {
  const root = options.root ?? repositoryRoot;
  const configFile = options.sourceConfig ?? path.join(root, "wrangler.jsonc");
  const wrangler = options.wrangler ?? path.join(root, "node_modules", ".bin", "wrangler");
  const spawnCommand = options.spawnCommand ?? spawn;
  const runCommand = options.runCommand ?? run;
  const { config, neonURL } = prepareDeploymentConfig(JSON.parse(await readFile(configFile, "utf8")), environment);
  if (neonURL) await transferNeonSecret(neonURL, { spawnCommand, wrangler, cwd: root, environment });

  const generatedConfig = path.join(root, configName(randomUUID()));
  let created = false;
  try {
    await writeFile(generatedConfig, JSON.stringify(config), { flag: "wx", mode: 0o600 });
    created = true;
    const deployEnvironment = { ...environment };
    delete deployEnvironment.NORTHWIND_NEON_DATABASE_URL;
    await runCommand(wrangler, ["deploy", "--config", generatedConfig, ...process.argv.slice(2)], {
      cwd: root,
      env: deployEnvironment,
      stdio: "inherit",
    });
  } finally {
    if (created) await rm(generatedConfig, { force: true });
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    await prepareDalgoCore();
    await runDeployment(process.env);
  } catch {
    process.stderr.write("Cloud deployment failed; inspect the deployment workflow without printing runtime credentials.\n");
    process.exitCode = 1;
  }
}
