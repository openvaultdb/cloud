import { createHash } from 'node:crypto';
import { readFile, open, rename, mkdir } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { mergeExports, validateManifest } from './merge.mjs';
import { collectionSchema, domains } from './schema.mjs';

const MAX_EXPORT = 256 * 1024 * 1024;
const MAX_RESPONSE = 8 * 1024 * 1024;
async function boundedBytes(response, limit) {
  if (!response.body) throw new Error('missing response body');
  const reader = response.body.getReader();
  const chunks = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > limit) { await reader.cancel(); throw new Error('response too large'); }
    chunks.push(value);
  }
  return Buffer.concat(chunks);
}

export async function loadExports(manifest, { allowFixtures = false, fetcher = fetch } = {}) {
  validateManifest(manifest, { allowFixtures });
  const exports = [];
  for (const source of manifest.sources) {
    let raw;
    if (source.file) {
      const stat = await import('node:fs/promises').then(fs => fs.stat(source.file));
      if (stat.size > MAX_EXPORT) throw new Error('fixture too large');
      raw = await readFile(source.file);
    } else {
      const response = await fetcher(source.url, { redirect: 'error', signal: AbortSignal.timeout(60000), headers: { accept: 'application/json' } });
      if (!response.ok || response.redirected || response.url !== source.url) throw new Error('source fetch failed');
      raw = await boundedBytes(response, MAX_EXPORT);
    }
    if (createHash('sha256').update(raw).digest('hex') !== source.sha256) throw new Error('source checksum mismatch');
    exports.push(JSON.parse(raw.toString('utf8')));
  }
  return exports;
}

export function typesenseClient(origin, key, fetcher = fetch, { allowLocalHTTP = false } = {}) {
  const url = new URL(origin);
  const localHTTP = allowLocalHTTP && url.protocol === 'http:' && ['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname);
  if (!(url.protocol === 'https:' || localHTTP) || url.username || url.password || url.search || url.hash || url.pathname !== '/' || !key) throw new Error('invalid engine configuration');
  return async (method, path, body, contentType = 'application/json') => {
    const response = await fetcher(new URL(path, url), {
      method, redirect: 'error', signal: AbortSignal.timeout(20000),
      headers: { 'x-typesense-api-key': key, ...(body === undefined ? {} : { 'content-type': contentType }) },
      body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body)
    });
    const raw = await boundedBytes(response, MAX_RESPONSE);
    if (!response.ok && response.status !== 404) throw new Error(`engine request failed (${response.status})`);
    return { status: response.status, body: raw.toString('utf8') };
  };
}

function json(result) { try { return JSON.parse(result.body); } catch { throw new Error('invalid engine JSON'); } }
function checkImports(body, documents) {
  const rows = body.trimEnd().split('\n');
  if (rows.length !== documents.length) throw new Error('import response count mismatch');
  for (let i = 0; i < rows.length; i++) {
    let row;
    try { row = JSON.parse(rows[i]); } catch { throw new Error(`invalid import response row ${i}`); }
    if (row.success !== true || row.id !== documents[i].id) throw new Error(`rejected import row ${i}`);
  }
}

async function saveState(path, state) {
  await mkdir(dirname(path), { recursive: true });
  const temp = `${path}.${process.pid}.tmp`;
  const handle = await open(temp, 'wx', 0o600);
  try { await handle.writeFile(JSON.stringify(state)); await handle.sync(); }
  finally { await handle.close(); }
  await rename(temp, path);
}

async function pruneInactive(api, active, previous) {
  const listed = await api('GET', '/collections');
  if (listed.status !== 200 || !Array.isArray(json(listed))) throw new Error('cannot list search generations');
  const aliases = await api('GET', '/aliases');
  if (aliases.status !== 200 || !Array.isArray(json(aliases).aliases)) throw new Error('cannot list search aliases');
  const referenced = new Set(json(aliases).aliases.map(item => item.collection_name));
  const retain = new Set([active, previous].filter(Boolean));
  for (const { name } of json(listed)) {
    if (/^registry_metadata_[a-f0-9]{24}$/.test(name) && !retain.has(name)) {
      if (referenced.has(name)) throw new Error('inactive generation has another alias');
      const removed = await api('DELETE', `/collections/${name}`);
      if (![200, 404].includes(removed.status)) throw new Error('inactive generation cleanup failed');
    }
  }
  return referenced;
}

export async function publish(manifest, exports, { api, stateDir, smokeQueries, allowFixtures = false }) {
  validateManifest(manifest, { allowFixtures });
  if (!Array.isArray(smokeQueries) || domains.some(domain => !smokeQueries.some(query => query?.domain === domain))) throw new Error('one smoke query per domain required');
  const { documents, hash, generation } = mergeExports(manifest, exports);
  const collection = `registry_metadata_${generation}`;
  const statePath = join(stateDir, 'publication.json');
  await mkdir(stateDir, { recursive: true });
    let state;
    try { state = JSON.parse(await readFile(statePath, 'utf8')); } catch (error) { if (error.code !== 'ENOENT') throw error; }
    if (state && (manifest.sequence < state.sequence || manifest.sequence === state.sequence && state.generation !== generation)) throw new Error('stale or conflicting publication');
    const aliasResult = await api('GET', '/aliases/registry_metadata');
    const active = aliasResult.status === 404 ? null : json(aliasResult).collection_name;
    if (active && !state) throw new Error('publication state missing');
    if (state?.status === 'published' && active !== `registry_metadata_${state.generation}`) throw new Error('alias drift');
    if (active === collection) {
      if (!state || state.sequence !== manifest.sequence || state.status !== 'published') await saveState(statePath, { sequence: manifest.sequence, generation, status: 'published', previous: state?.previous || null });
      await pruneInactive(api, collection, state?.previous);
      return { generation, hash, count: documents.length, unchanged: true };
    }
    // Failed candidates from earlier attempts are safe to remove before retrying.
    const referenced = await pruneInactive(api, active, state?.previous);
    const existing = await api('GET', `/collections/${collection}`);
    if (existing.status === 200) {
      if (referenced.has(collection)) throw new Error('candidate generation has another alias');
      const removed = await api('DELETE', `/collections/${collection}`);
      if (![200, 404].includes(removed.status)) throw new Error('candidate generation removal failed');
    }
    await api('POST', '/collections', collectionSchema(collection));
    for (let start = 0; start < documents.length; start += 500) {
      const batch = documents.slice(start, start + 500);
      const body = batch.map(doc => JSON.stringify(doc)).join('\n') + '\n';
      const result = await api('POST', `/collections/${collection}/documents/import?return_id=true&action=create`, body, 'text/plain');
      checkImports(result.body, batch);
    }
    const imported = await api('GET', `/collections/${collection}`);
    if (imported.status !== 200 || json(imported).num_documents !== documents.length) throw new Error('collection count mismatch');
    for (const query of smokeQueries) {
      if (!documents.some(doc => doc.id === query.id && doc.domain === query.domain) || typeof query.q !== 'string' || !query.q) throw new Error('invalid smoke query');
      const args = new URLSearchParams({ q: query.q, query_by: 'identifier,qualified_name,title,aliases,description', filter_by: `domain:=${query.domain} && visibility:=public`, per_page: '20' });
      const result = json(await api('GET', `/collections/${collection}/documents/search?${args}`));
      if (!result.hits?.some(hit => hit.document?.id === query.id)) throw new Error('smoke query failed');
    }
    await saveState(statePath, { sequence: manifest.sequence, generation, status: 'switching', previous: active });
    await api('PUT', '/aliases/registry_metadata', { collection_name: collection });
    const switched = await api('GET', '/aliases/registry_metadata');
    if (switched.status !== 200 || json(switched).collection_name !== collection) throw new Error('alias verification failed');
    await saveState(statePath, { sequence: manifest.sequence, generation, status: 'published', previous: active });
    try { await pruneInactive(api, collection, active); }
    catch (error) { throw new Error(`search alias switched; ${error.message}`); }
    return { generation, hash, count: documents.length, previous: active };
}
