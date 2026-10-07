#!/usr/bin/env node
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { loadExports, publish, typesenseClient } from './publish.mjs';
import { mergeExports } from './merge.mjs';
import { runUnderPublicationLock, assertPublicationLock } from './lock.mjs';
import { productionEngineOrigin } from './engine-policy.mjs';

async function main() {
  const [command, manifestPath, output] = process.argv.slice(2);
  if (!['prepare', 'publish'].includes(command) || !manifestPath) throw new Error('usage: cli.mjs prepare|publish manifest.json [output.jsonl]');
  if (command === 'publish') {
    const stateDir = process.env.REGISTRY_SEARCH_STATE_DIR;
    if (!stateDir) throw new Error('state directory required');
    await mkdir(stateDir, { recursive: true, mode: 0o700 });
    const lockPath = join(stateDir, 'publication.lock');
    if (process.env.REGISTRY_SEARCH_LOCK_FD || process.env.REGISTRY_SEARCH_LOCK_PID) assertPublicationLock(lockPath);
    else {
      process.exitCode = await runUnderPublicationLock(lockPath, process.execPath, [fileURLToPath(import.meta.url), 'publish', resolve(manifestPath)]);
      return;
    }
  }
  const manifest = JSON.parse(await readFile(resolve(manifestPath), 'utf8'));
  const mode = process.env.REGISTRY_SEARCH_MODE;
  const allowFixtures = (command === 'prepare' || mode === 'local') && process.env.REGISTRY_SEARCH_ALLOW_FIXTURES === '1';
  const exports = await loadExports(manifest, { allowFixtures });
  if (command === 'prepare') {
    if (!output) throw new Error('output path required');
    const merged = mergeExports(manifest, exports);
    await writeFile(resolve(output), merged.documents.map(doc => JSON.stringify(doc)).join('\n') + '\n');
    process.stdout.write(JSON.stringify({ generation: merged.generation, hash: merged.hash, count: merged.documents.length }) + '\n');
    return;
  }
  if ((manifest.sources.some(source => source.file) || exports.some(source => source.fixture)) && !(mode === 'local' && allowFixtures)) throw new Error('fixtures require explicit local proof');
  if (!['local', 'staging', 'production'].includes(mode)) throw new Error('publish requires local, staging or production mode');
  if (mode === 'production') productionEngineOrigin(process.env);
  const stateDir = process.env.REGISTRY_SEARCH_STATE_DIR;
  if (!stateDir) throw new Error('state directory required');
  await mkdir(stateDir, { recursive: true });
  const result = await publish(manifest, exports, {
    api: typesenseClient(mode === 'production' && process.env.REGISTRY_TYPESENSE_DEPLOYMENT === 'vm-pilot' ? 'http://127.0.0.1:8108/' : process.env.REGISTRY_TYPESENSE_ORIGIN, process.env.REGISTRY_TYPESENSE_ADMIN_KEY, fetch, { allowLocalHTTP: mode === 'local' || mode === 'production' && process.env.REGISTRY_TYPESENSE_DEPLOYMENT === 'vm-pilot' }),
    stateDir,
    smokeQueries: manifest.smoke_queries,
    allowFixtures: mode === 'local' && allowFixtures
  });
  process.stdout.write(JSON.stringify(result) + '\n');
}
main().catch(error => { process.stderr.write(`registry search: ${error.message}\n`); process.exitCode = 1; });
