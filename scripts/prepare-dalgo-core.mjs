import { spawn } from "node:child_process";
import { access, readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repositoryRoot = path.dirname(path.dirname(fileURLToPath(import.meta.url)));

function run(command, args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { ...options, stdio: "inherit" });
    child.on("error", () => reject(new Error("Pinned DALgo core preparation failed.")));
    child.on("close", (code) => code === 0 ? resolve() : reject(new Error("Pinned DALgo core preparation failed.")));
  });
}

export async function prepareDalgoCore(root = repositoryRoot, runCommand = run) {
  const core = path.join(root, "node_modules", "@dalgo", "core");
  const packageJSON = JSON.parse(await readFile(path.join(core, "package.json"), "utf8"));
  if (packageJSON.name !== "@dalgo/core" || packageJSON.version !== "0.2.0") {
    throw new Error("The expected pinned DALgo core package is missing.");
  }
  const config = path.join(core, "tsconfig.build.json");
  await access(config);
  await runCommand(path.join(root, "node_modules", ".bin", "tsc"), ["-p", config], { cwd: root });
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    await prepareDalgoCore();
  } catch {
    process.stderr.write("Could not prepare the pinned DALgo core dependency.\n");
    process.exitCode = 1;
  }
}
