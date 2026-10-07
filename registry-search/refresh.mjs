#!/usr/bin/env node
import { createHash } from 'node:crypto';
import { readFile, mkdir } from 'node:fs/promises';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { canonicalRoutes, mergeExports } from './merge.mjs';
import { domains } from './schema.mjs';
import { publish, typesenseClient } from './publish.mjs';
import { productionEngineOrigin } from './engine-policy.mjs';
import { runUnderPublicationLock, assertPublicationLock } from './lock.mjs';

const repositories = Object.freeze({ meaninggraph: 'meaninggraph/registry', modelspec: 'modelspec-org/registry', ovdb: 'openvaultdb/directory' });
const maxExportBytes = 32 * 1024 * 1024;
const maxPilotDocuments = 10_000;
const sha = /^[a-f0-9]{40}$/;

async function boundedExport(response) {
  if (!response.body || Number(response.headers.get('content-length')) > maxExportBytes) throw new Error('export too large');
  const reader = response.body.getReader();
  const chunks = [];
  let bytes = 0;
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    bytes += value.byteLength;
    if (bytes > maxExportBytes) { await reader.cancel(); throw new Error('export too large'); }
    chunks.push(value);
  }
  return Buffer.concat(chunks);
}

function smokeFor(domain, envelope) {
  const eligible = envelope.documents.filter(doc => doc.domain === domain && typeof doc.identifier === 'string' && doc.identifier && typeof doc.qualified_name === 'string' && doc.qualified_name);
  const counts = new Map();
  for (const doc of eligible) counts.set(doc.identifier, (counts.get(doc.identifier) || 0) + 1);
  let doc = eligible.find(item => counts.get(item.identifier) === 1);
  if (doc) return { domain, q: doc.identifier, id: doc.id };
  counts.clear();
  for (const item of eligible) counts.set(item.qualified_name, (counts.get(item.qualified_name) || 0) + 1);
  doc = eligible.find(item => counts.get(item.qualified_name) === 1);
  if (!doc) throw new Error(`no unique smoke query for ${domain}`);
  return { domain, q: doc.qualified_name, id: doc.id };
}

export async function fetchCurrentCorpus(fetcher = fetch) {
  const sources = [];
  const exports = [];
  const revisions = new Map();
  for (const domain of domains) {
    const url = `https://${canonicalRoutes[domain].hosts[0]}/registry-search.json`;
    let raw;
    try {
      const response = await fetcher(url, { redirect: 'error', signal: AbortSignal.timeout(30_000), headers: { accept: 'application/json' } });
      if (!response.ok || response.redirected || response.url !== url) throw new Error('invalid fetch');
      raw = await boundedExport(response);
    } catch { throw new Error(`public export fetch failed for ${domain}`); }
    let envelope;
    try { envelope = JSON.parse(raw.toString('utf8')); } catch { throw new Error(`invalid public export for ${domain}`); }
    const repository = repositories[domain];
    const ownRevision = envelope?.source_revisions?.[repository];
    if (envelope?.domain !== domain || envelope?.format !== 'registry-search-export/v1' || envelope.fixture !== false || !sha.test(ownRevision) || !Array.isArray(envelope.documents)) throw new Error(`invalid public export for ${domain}`);
    for (const [repo, revision] of Object.entries(envelope.source_revisions)) {
      if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo) || !sha.test(revision) || revisions.has(repo) && revisions.get(repo) !== revision) throw new Error('conflicting export source pins');
      revisions.set(repo, revision);
    }
    sources.push({ domain, repository, revision: ownRevision, sha256: createHash('sha256').update(raw).digest('hex'), url });
    exports.push(envelope);
  }
  const approved_revisions = Object.fromEntries([...revisions].filter(([repo]) => !Object.values(repositories).includes(repo)).sort());
  if (exports.reduce((total, envelope) => total + envelope.documents.length, 0) > maxPilotDocuments) throw new Error('pilot document capacity exceeded');
  const manifest = { sequence: 1, sources, approved_revisions, smoke_queries: exports.map(envelope => smokeFor(envelope.domain, envelope)) };
  return { manifest, exports };
}

async function readJSON(path) {
  try { return JSON.parse(await readFile(path, 'utf8')); }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
}

export async function refresh({ stateDir, api, fetcher = fetch }) {
  const state = await readJSON(join(stateDir, 'publication.json'));
  const { manifest, exports } = await fetchCurrentCorpus(fetcher);
  const merged = mergeExports(manifest, exports);
  manifest.sequence = state?.generation === merged.generation ? state.sequence : (state?.sequence || 0) + 1;
  const result = await publish(manifest, exports, { api, stateDir, smokeQueries: manifest.smoke_queries });
  // The manifest is diagnostic state. Publication itself is fenced by publication.json.
  const { writeFile, rename } = await import('node:fs/promises');
  const path = join(stateDir, 'live-manifest.json');
  const temp = `${path}.${process.pid}.tmp`;
  await writeFile(temp, JSON.stringify(manifest), { mode: 0o600, flag: 'wx' });
  await rename(temp, path);
  return result;
}

async function main() {
  const stateDir = process.env.REGISTRY_SEARCH_STATE_DIR;
  if (process.env.REGISTRY_SEARCH_MODE !== 'production' || !stateDir) throw new Error('production mode and durable state directory required');
  productionEngineOrigin(process.env);
  await mkdir(stateDir, { recursive: true, mode: 0o700 });
  const lockPath = join(stateDir, 'publication.lock');
  if (!process.env.REGISTRY_SEARCH_LOCK_FD && !process.env.REGISTRY_SEARCH_LOCK_PID) {
    process.exitCode = await runUnderPublicationLock(lockPath, process.execPath, [fileURLToPath(import.meta.url)]);
    return;
  }
  assertPublicationLock(lockPath);
  const adminKey = process.env.TYPESENSE_API_KEY;
  if (!adminKey) throw new Error('missing engine key');
  // Publication stays on the VM loopback API. The public HTTPS origin is only for search.
  if (process.env.REGISTRY_TYPESENSE_DEPLOYMENT !== 'vm-pilot') throw new Error('scheduled refresh requires VM pilot');
  const result = await refresh({ stateDir, api: typesenseClient('http://127.0.0.1:8108/', adminKey, fetch, { allowLocalHTTP: true }) });
  process.stdout.write(JSON.stringify({ generation: result.generation, count: result.count, unchanged: !!result.unchanged }) + '\n');
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main().catch(error => {
  process.stderr.write(`registry search refresh: ${error.message}\n`);
  process.exitCode = 1;
});
