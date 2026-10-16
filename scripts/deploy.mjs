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

/** A failure whose message is fixed text (never derived from env, URLs, or child output), so it is safe to print. */
export class DeploymentError extends Error {
  constructor(step, message) {
    super(message);
    this.name = "DeploymentError";
    this.step = step;
  }
}

/**
 * Parses JSONC (wrangler.jsonc): strips // and /* *\/ comments outside strings and trailing commas.
 * JSON.parse alone rejects the comments the Worker config carries.
 */
export function parseJSONC(text) {
  let out = "";
  let inString = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inString) {
      out += c;
      if (c === "\\") out += text[++i] ?? "";
      else if (c === '"') inString = false;
    } else if (c === '"') {
      inString = true;
      out += c;
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++;
      out += "\n";
    } else if (c === "/" && text[i + 1] === "*") {
      const end = text.indexOf("*/", i + 2);
      if (end < 0) throw new Error("unterminated comment");
      i = end + 1;
    } else {
      out += c;
    }
  }
  return JSON.parse(out.replace(/,(\s*[}\]])/gu, "$1"));
}

export function describeFailure(error) {
  if (error instanceof DeploymentError) return `Cloud deployment failed at step "${error.step}": ${error.message}`;
  return "Cloud deployment failed at step \"unexpected\": an unexpected error occurred; its details are withheld because they might contain runtime credentials.";
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
  let baseConfig;
  try {
    baseConfig = parseJSONC(await readFile(configFile, "utf8"));
  } catch {
    throw new DeploymentError("read-config", "wrangler.jsonc could not be read or parsed as JSONC.");
  }
  let prepared;
  try {
    prepared = prepareDeploymentConfig(baseConfig, environment);
  } catch (error) {
    throw new DeploymentError("validate-inputs", error instanceof Error ? error.message : "Deployment inputs are invalid.");
  }
  const { config, neonURL } = prepared;
  if (neonURL) {
    try {
      await transferNeonSecret(neonURL, { spawnCommand, wrangler, cwd: root, environment });
    } catch {
      throw new DeploymentError("neon-secret", "wrangler secret put NORTHWIND_NEON_DATABASE_URL failed (check the Cloudflare token's Workers Scripts edit permission).");
    }
  }

  const generatedConfig = path.join(root, configName(randomUUID()));
  let created = false;
  try {
    await writeFile(generatedConfig, JSON.stringify(config), { flag: "wx", mode: 0o600 });
    created = true;
    const deployEnvironment = { ...environment };
    delete deployEnvironment.NORTHWIND_NEON_DATABASE_URL;
    try {
      await runCommand(wrangler, ["deploy", "--config", generatedConfig, ...(options.deployArgs ?? process.argv.slice(2))], {
        cwd: root,
        env: deployEnvironment,
        stdio: "inherit",
      });
    } catch {
      throw new DeploymentError("wrangler-deploy", "wrangler deploy exited with an error; see the wrangler output above (it is printed by wrangler itself).");
    }
  } finally {
    if (created) await rm(generatedConfig, { force: true });
  }
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    try {
      await prepareDalgoCore();
    } catch {
      throw new DeploymentError("prepare-dalgo-core", "the pinned @dalgo/core package could not be prepared (run npm ci).");
    }
    await runDeployment(process.env);
  } catch (error) {
    process.stderr.write(`${describeFailure(error)}\n`);
    process.exitCode = 1;
  }
}
