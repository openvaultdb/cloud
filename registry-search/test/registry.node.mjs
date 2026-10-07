import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, readFile } from 'node:fs/promises';
import { writeFile, access } from 'node:fs/promises';
import { spawn, spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { stableId, collectionSchema } from '../schema.mjs';
import { originForDocument, corePriority, repositoryTerms, searchFields } from '../provenance.mjs';
import { mergeExports, validateManifest } from '../merge.mjs';
import { publish, typesenseClient } from '../publish.mjs';
import { createGateway } from '../gateway.mjs';
import { productionEngineOrigin } from '../engine-policy.mjs';
import { fetchCurrentCorpus, refresh } from '../refresh.mjs';
import { serviceUnit, sourceFiles } from '../vm-refresh-service.mjs';
import { insertRoute, removeRoute } from '../vm-caddy-route.mjs';
import { runUnderPublicationLock, assertPublicationLock } from '../lock.mjs';

const sha = char => char.repeat(40);
const specs = [
  ['meaninggraph', 'meaninggraph/registry', 'meaning_entity', 'https://meaninggraph.io/graphs/demo/concepts/Customer/'],
  ['modelspec', 'modelspec-org/registry', 'model', 'https://modelspec.org/registry/models/commerce/'],
  ['ovdb', 'openvaultdb/directory', 'ovdb_database', 'https://directory.openvaultdb.com/databases/chinook/']
];
function fixture() {
  const sources = specs.map(([domain, repository], i) => ({ domain, repository, revision: sha(String(i + 1)), sha256: 'a'.repeat(64), file: `/tmp/${domain}.json` }));
  const exports = specs.map(([domain, repository, kind, canonical_url], i) => {
    const native_id = `${domain}/Customer`;
    return { format: 'registry-search-export/v1', domain, fixture: true, source_revisions: { [repository]: sha(String(i + 1)), 'shared/pin': sha('f') }, documents: [{
      id: stableId(domain, kind, native_id), domain, kind, native_id, title: 'Customer', identifier: 'Customer', qualified_name: `${domain}.Customer`, canonical_url,
      visibility: 'public', source_repository: repository, source_commit: sha(String(i + 1)), source_path: 'records/customer.json'
    }] };
  });
  return { manifest: { sequence: 1, sources, approved_revisions: { 'shared/pin': sha('f') } }, exports };
}
test('merges a reproducible public corpus and rejects conflicting pins', () => {
  const { manifest, exports } = fixture();
  const first = mergeExports(manifest, exports);
  assert.equal(first.documents.length, 3);
  assert.equal(first.hash, mergeExports(manifest, exports).hash);
  exports[0].documents[0] = Object.fromEntries(Object.entries(exports[0].documents[0]).reverse());
  assert.equal(first.hash, mergeExports(manifest, exports).hash);
  exports[2].source_revisions['shared/pin'] = sha('e');
  assert.throws(() => mergeExports(manifest, exports), /conflicting shared pin/);
});
test('rejects private data, duplicate IDs, unsafe routes, and unpinned sources', () => {
  const { manifest, exports } = fixture();
  exports[0].documents[0].visibility = 'private';
  assert.throws(() => mergeExports(manifest, exports), /public scope/);
  exports[0].documents[0].visibility = 'public';
  exports[0].documents.push(exports[0].documents[0]);
  assert.throws(() => mergeExports(manifest, exports), /duplicate document/);
  exports[0].documents.pop();
  exports[0].documents[0].canonical_url = 'https://evil.example/graphs/demo/concepts/Customer/';
  assert.throws(() => mergeExports(manifest, exports), /unsafe canonical/);
  manifest.sources[0].url = 'https://meaninggraph.io/registry-search.json';
  assert.throws(() => validateManifest(manifest, { allowFixtures: true }), /fixture forbidden/);
});
test('accepts approved object-repository provenance and rejects missing envelope pin', () => {
  const { manifest, exports } = fixture();
  manifest.approved_revisions['datatug/chinookdb'] = sha('d');
  exports[2].documents[0].source_repository = 'datatug/chinookdb';
  exports[2].documents[0].source_commit = sha('d');
  exports[2].source_revisions['datatug/chinookdb'] = sha('d');
  assert.equal(mergeExports(manifest, exports).documents.length, 3);
  delete exports[2].source_revisions['datatug/chinookdb'];
  assert.throws(() => mergeExports(manifest, exports), /document missing source pin/);
});
test('derives searchable repository terms only from pinned public provenance', () => {
  const { manifest, exports } = fixture();
  const model = exports[1].documents[0];
  manifest.approved_revisions['demo-db/chinook'] = sha('d');
  exports[1].source_revisions['demo-db/chinook'] = sha('d');
  model.source_repository = 'demo-db/chinook';
  model.source_commit = sha('d');
  const indexed = mergeExports(manifest, exports).documents.find(doc => doc.id === model.id);
  assert.deepEqual(repositoryTerms(indexed), { repository_owner: 'demo-db', repository_name: 'chinook', repository_full_name: 'demo-db/chinook' });
  assert.equal(indexed.repository_owner, 'demo-db');
  assert.equal(indexed.repository_name, 'chinook');
  assert.equal(indexed.repository_full_name, 'demo-db/chinook');
  assert.notEqual(mergeExports(manifest, exports).generation, mergeExports(fixture().manifest, fixture().exports).generation);
  for (const field of ['repository_owner', 'repository_name', 'repository_full_name']) {
    model[field] = 'forged';
    assert.throws(() => mergeExports(manifest, exports), /unknown document field/);
    delete model[field];
    assert.equal(collectionSchema('candidate').fields.find(item => item.name === field).index, undefined);
  }
  assert.throws(() => repositoryTerms({ source_repository: `owner/${'x'.repeat(160)}` }), /invalid source repository/);
});
test('derives core priority and entity field context only from pinned source records', () => {
  const { manifest, exports } = fixture();
  const core = exports[0].documents[0];
  manifest.approved_revisions['meaninggraph/core'] = sha('c');
  exports[0].source_revisions['meaninggraph/core'] = sha('c');
  core.source_repository = 'meaninggraph/core';
  core.source_commit = sha('c');
  core.description = 'A customer in the core vocabulary.';
  const field = {
    ...core, id: stableId('meaninggraph', 'meaning_field', 'core/Customer/country'), kind: 'meaning_field',
    native_id: 'core/Customer/country', title: 'Country', identifier: 'country', qualified_name: 'core.Customer.country',
    parent_id: core.id, parent_label: 'Customer', description: 'Customer country.'
  };
  const publicField = {
    ...field, id: stableId('meaninggraph', 'meaning_field', 'northwind/Customer/credit'),
    native_id: 'northwind/Customer/credit', title: 'Credit', identifier: 'credit', qualified_name: 'northwind.Customer.credit',
    source_repository: 'meaninggraph/registry', source_commit: manifest.sources[0].revision
  };
  exports[0].documents.push(field, publicField);
  const merged = mergeExports(manifest, exports);
  const indexedCore = merged.documents.find(doc => doc.id === core.id);
  assert.equal(indexedCore.core_priority, 1);
  assert.deepEqual(indexedCore.field_preview, ['Country']);
  assert.equal(indexedCore.field_count, 1);
  assert.equal(merged.documents.find(doc => doc.id === field.id).core_priority, 1);
  assert.equal(merged.documents.find(doc => doc.id === publicField.id).core_priority, 0);
  assert.notEqual(merged.generation, mergeExports(fixture().manifest, fixture().exports).generation);
  exports[0].documents[0].core_priority = 1;
  assert.throws(() => mergeExports(manifest, exports), /unknown document field/);
});
test('a core-like name, parent or unrelated domain cannot claim the core origin', () => {
  const core = { domain: 'meaninggraph', kind: 'meaning_entity', source_repository: 'meaninggraph/core' };
  assert.equal(originForDocument(core), 'core');
  for (const doc of [
    { ...core, source_repository: 'meaninggraph/core-fork' },
    { ...core, source_repository: 'meaninggraph/registry', qualified_name: 'core.customer' },
    { ...core, kind: 'meaning_field', source_repository: 'meaninggraph/registry', parent_label: 'Core' },
    { ...core, domain: 'modelspec' }
  ]) {
    assert.equal(originForDocument(doc), 'public_registry');
    assert.equal(corePriority(doc), 0);
  }
});
test('merges a ModelSpec component-use effective field with its declaring anchor', () => {
  const { manifest, exports } = fixture();
  const model = exports[1].documents[0];
  const source = { visibility: 'public', source_repository: model.source_repository, source_commit: model.source_commit, source_path: model.source_path };
  const entityNative = 'modelspec/commerce/Customer';
  const entity = {
    ...source, id: stableId('modelspec', 'model_entity', entityNative), domain: 'modelspec', kind: 'model_entity', native_id: entityNative,
    title: 'Customer', identifier: 'Customer', qualified_name: 'commerce.Customer', canonical_url: 'https://modelspec.org/registry/models/commerce/#entity-Customer',
    parent_id: model.id, parent_label: model.title
  };
  const fieldNative = 'modelspec/commerce/Customer/Auditable.createdAt';
  const field = {
    ...source, id: stableId('modelspec', 'model_field', fieldNative), domain: 'modelspec', kind: 'model_field', native_id: fieldNative,
    title: 'createdAt', identifier: 'createdAt', qualified_name: 'commerce.Customer.createdAt',
    canonical_url: 'https://modelspec.org/registry/models/commerce/#field-Auditable-createdAt',
    parent_id: entity.id, parent_label: entity.title, declaring_component: 'Auditable'
  };
  exports[1].documents.push(entity, field);
  const merged = mergeExports(manifest, exports);
  assert.equal(merged.documents.length, 5);
  assert.equal(merged.documents.find(doc => doc.id === field.id).declaring_component, 'Auditable');
  assert.deepEqual(merged.documents.find(doc => doc.id === entity.id).field_preview, ['createdAt']);
  assert.equal(merged.documents.find(doc => doc.id === entity.id).field_count, 1);
  assert.equal(collectionSchema('candidate').fields.find(item => item.name === 'declaring_component').index, false);
});
function fakeEngine(failImport = false) {
  const collections = new Map();
  let alias = null;
  let failDelete = false;
  let otherAlias = null;
  const engine = {
    get failImport() { return failImport; },
    set failImport(value) { failImport = value; },
    set failDelete(value) { failDelete = value; },
    set otherAlias(value) { otherAlias = value; },
    get alias() { return alias; },
    get collections() { return [...collections.keys()]; },
    async api(method, path, body) {
      if (method === 'GET' && path === '/collections') return { status: 200, body: JSON.stringify([...collections.keys()].map(name => ({ name }))) };
      if (method === 'GET' && path === '/aliases') return { status: 200, body: JSON.stringify({ aliases: [...(alias ? [{ name: 'registry_metadata', collection_name: alias }] : []), ...(otherAlias ? [{ name: 'other', collection_name: otherAlias }] : [])] }) };
      if (method === 'GET' && path === '/aliases/registry_metadata') return alias ? { status: 200, body: JSON.stringify({ collection_name: alias }) } : { status: 404, body: '' };
      if (method === 'PUT' && path === '/aliases/registry_metadata') { alias = body.collection_name; return { status: 200, body: '{}' }; }
      if (method === 'POST' && path === '/collections') { collections.set(body.name, []); return { status: 201, body: '{}' }; }
      const collection = path.match(/^\/collections\/([^/]+)/)?.[1];
      if (method === 'DELETE' && collection) { if (failDelete) return { status: 500, body: '{}' }; collections.delete(collection); return { status: 200, body: '{}' }; }
      if (method === 'GET' && path.endsWith('/documents/search?'+path.split('/documents/search?')[1])) {
        const params = new URLSearchParams(path.split('?')[1]);
        const q = params.get('q');
        assert.equal(params.get('query_by'), searchFields.query_by);
        assert.equal(params.get('query_by_weights'), searchFields.query_by_weights);
        assert.equal(params.get('sort_by'), '_text_match:desc,core_priority:desc');
        assert.equal(params.get('prioritize_num_matching_fields'), params.get('filter_by').includes('domain:=meaninggraph') ? 'false' : null);
        const docs = collections.get(collection) || [];
        return { status: 200, body: JSON.stringify({ hits: docs.filter(doc => doc.identifier === q).map(document => ({ document })) }) };
      }
      if (method === 'POST' && path.includes('/documents/import?')) {
        const rows = body.trim().split('\n').map(JSON.parse);
        collections.get(collection).push(...(failImport ? rows.slice(1) : rows));
        return { status: 200, body: rows.map((row, i) => JSON.stringify({ success: !failImport || i > 0, id: row.id })).join('\n') + '\n' };
      }
      if (method === 'GET' && collection) return collections.has(collection) ? { status: 200, body: JSON.stringify({ num_documents: collections.get(collection).length }) } : { status: 404, body: '' };
      throw new Error(`unexpected ${method} ${path}`);
    }
  };
  return engine;
}
test('failed per-row import preserves alias; valid generation is idempotent and fences older sequence', async () => {
  const { manifest, exports } = fixture();
  for (const source of manifest.sources) { delete source.file; source.url = `https://${source.domain === 'meaninggraph' ? 'meaninggraph.io' : source.domain === 'modelspec' ? 'modelspec.org' : 'directory.openvaultdb.com'}/registry-search.json`; }
  for (const envelope of exports) envelope.fixture = false;
  const dir = await mkdtemp(join(tmpdir(), 'registry-test-'));
  const smokeQueries = exports.map(envelope => ({ domain: envelope.domain, id: envelope.documents[0].id, q: 'Customer' }));
  const bad = fakeEngine(true);
  await assert.rejects(publish(manifest, exports, { api: bad.api, stateDir: dir, smokeQueries }), /rejected import/);
  assert.equal(bad.alias, null);
  bad.failImport = false;
  const good = bad;
  const first = await publish(manifest, exports, { api: good.api, stateDir: dir, smokeQueries });
  assert.equal(first.count, 3);
  assert.equal(good.alias, `registry_metadata_${first.generation}`);
  assert.equal((await publish(manifest, exports, { api: good.api, stateDir: dir, smokeQueries })).unchanged, true);
  manifest.sequence = 5;
  assert.equal((await publish(manifest, exports, { api: good.api, stateDir: dir, smokeQueries })).unchanged, true);
  assert.equal(JSON.parse(await readFile(join(dir, 'publication.json'))).sequence, 5);
  const newer = structuredClone(exports);
  newer[0].documents[0].title = 'Customer New';
  manifest.sequence = 6;
  await publish(manifest, newer, { api: good.api, stateDir: dir, smokeQueries });
  good.otherAlias = `registry_metadata_${first.generation}`;
  manifest.sequence = 7;
  await assert.rejects(publish(manifest, exports, { api: good.api, stateDir: dir, smokeQueries }), /candidate generation has another alias/);
  assert.equal(good.collections.includes(`registry_metadata_${first.generation}`), true);
  good.otherAlias = null;
  manifest.sequence = 4;
  await assert.rejects(publish(manifest, exports, { api: good.api, stateDir: dir, smokeQueries }), /stale/);
  assert.equal(JSON.parse(await readFile(join(dir, 'publication.json'))).sequence, 6);
});
test('publication recovers a switching journal before and after alias update', async () => {
  const { manifest, exports } = fixture();
  for (const source of manifest.sources) { delete source.file; source.url = `https://${source.domain === 'meaninggraph' ? 'meaninggraph.io' : source.domain === 'modelspec' ? 'modelspec.org' : 'directory.openvaultdb.com'}/registry-search.json`; }
  for (const envelope of exports) envelope.fixture = false;
  const dir = await mkdtemp(join(tmpdir(), 'registry-switch-'));
  const engine = fakeEngine();
  const smokeQueries = exports.map(envelope => ({ domain: envelope.domain, id: envelope.documents[0].id, q: 'Customer' }));
  const first = await publish(manifest, exports, { api: engine.api, stateDir: dir, smokeQueries });
  const next = structuredClone(exports);
  next[0].documents[0].title = 'Renamed Customer';
  manifest.sequence = 2;
  const nextGeneration = mergeExports(manifest, next).generation;
  await writeFile(join(dir, 'publication.json'), JSON.stringify({ sequence: 2, generation: nextGeneration, status: 'switching', previous: engine.alias }));
  await publish(manifest, next, { api: engine.api, stateDir: dir, smokeQueries });
  assert.equal(engine.alias, `registry_metadata_${nextGeneration}`);
  await writeFile(join(dir, 'publication.json'), JSON.stringify({ sequence: 2, generation: nextGeneration, status: 'switching', previous: `registry_metadata_${first.generation}` }));
  assert.equal((await publish(manifest, next, { api: engine.api, stateDir: dir, smokeQueries })).unchanged, true);
  assert.equal(JSON.parse(await readFile(join(dir, 'publication.json'))).status, 'published');
  await assert.rejects(publish(manifest, next, { api: engine.api, stateDir: dir, smokeQueries: [smokeQueries[0], smokeQueries[0], smokeQueries[0]] }), /per domain/);
});
const env = { REGISTRY_SEARCH_MODE: 'staging', REGISTRY_TYPESENSE_ORIGIN: 'https://engine.example/', REGISTRY_TYPESENSE_SEARCH_KEY: 'secret', REGISTRY_RATE_LIMITER: { limit: async () => ({ success: true }) } };
const request = (body, headers = {}) => new Request('https://search.example/v1/registry-search', { method: 'POST', headers: { 'content-type': 'application/json', origin: 'https://meaninggraph.io', ...headers }, body: JSON.stringify(body) });
test('gateway fixes query policy, checks domain/scope, and hides engine response', async () => {
  let target;
  const worker = createGateway(async (url, options) => {
    target = url;
    assert.equal(options.redirect, 'manual');
    assert.equal(options.headers['x-typesense-api-key'], 'secret');
    const search = JSON.parse(options.body).searches[0];
    assert.deepEqual({ query_by: search.query_by, query_by_weights: search.query_by_weights }, searchFields);
    assert.equal(search.sort_by, '_text_match:desc,core_priority:desc');
    assert.equal(search.prioritize_num_matching_fields, false);
    assert.match(search.filter_by, /visibility:=public/);
    assert.match(search.include_fields, /domain,visibility/);
    return Response.json({ results: [{ found: 1, hits: [{ document: { id: 'a', domain: 'meaninggraph', visibility: 'public', title: 'Customer', kind: 'meaning_entity', canonical_url: 'https://meaninggraph.io/graphs/g/concepts/Customer/', generation_id: 'gen', source_repository: 'meaninggraph/core', repository_owner: 'meaninggraph', repository_name: 'core', repository_full_name: 'meaninggraph/core', core_priority: 1, description: 'A customer.', field_preview: ['Country', 'City'], field_count: 2, source_path: 'private' } }] }] });
  });
  const response = await worker.fetch(request({ q: 'Customer', domain: 'meaninggraph' }), env);
  assert.equal(response.status, 200);
  const [hit] = (await response.json()).hits;
  assert.equal(hit.source_path, undefined);
  assert.equal(hit.source_repository, undefined);
  assert.equal(hit.core_priority, undefined);
  assert.equal(hit.repository, 'meaninggraph/core');
  assert.equal(hit.repository_owner, undefined);
  assert.equal(hit.origin, 'core');
  assert.equal(hit.description, 'A customer.');
  assert.deepEqual(hit.field_preview, ['Country', 'City']);
  assert.equal(hit.field_count, 2);
  assert.equal(target.pathname, '/multi_search');
  assert.equal(target.search, '');
  assert.equal((await worker.fetch(request({ q: '*', domain: 'meaninggraph', filter_by: 'visibility:=private' }), env)).status, 400);
  assert.equal((await worker.fetch(request({ q: 'x', domain: 'meaninggraph' }, { origin: 'https://evil.example' }), env)).status, 403);
});
test('gateway searches a ModelSpec repository owner, name and full path with public context', async () => {
  const worker = createGateway(async (_url, options) => {
    const search = JSON.parse(options.body).searches[0];
    assert.equal(search.q, 'demo-db');
    assert.deepEqual({ query_by: search.query_by, query_by_weights: search.query_by_weights }, searchFields);
    assert.equal(search.prioritize_num_matching_fields, undefined);
    return Response.json({ results: [{ found: 1, hits: [{ document: {
      id: 'model', domain: 'modelspec', visibility: 'public', kind: 'model', title: 'Chinook',
      source_repository: 'demo-db/chinook', repository_owner: 'demo-db', repository_name: 'chinook', repository_full_name: 'demo-db/chinook',
      core_priority: 0, source_path: 'private', source_commit: sha('d')
    } }] }] });
  });
  const response = await worker.fetch(request({ q: 'demo-db', domain: 'modelspec' }), env);
  assert.equal(response.status, 200);
  const [hit] = (await response.json()).hits;
  assert.equal(hit.repository, 'demo-db/chinook');
  assert.equal(hit.source_repository, undefined);
  assert.equal(hit.source_path, undefined);
  assert.equal(hit.source_commit, undefined);
  assert.equal(hit.origin, 'public_registry');
});
test('gateway returns unavailable on engine failure and fails closed without rate binding', async () => {
  const worker = createGateway(async () => { throw new Error('secret engine error'); });
  const response = await worker.fetch(request({ q: 'x', domain: 'meaninggraph' }), env);
  assert.deepEqual(await response.json(), { error: 'unavailable' });
  assert.equal((await worker.fetch(request({ q: 'x', domain: 'meaninggraph' }), { ...env, REGISTRY_RATE_LIMITER: undefined })).status, 503);
  assert.equal((await worker.fetch(request({ q: 'x', domain: 'meaninggraph' }), { ...env, REGISTRY_SEARCH_MODE: 'production', REGISTRY_TYPESENSE_DEPLOYMENT: 'self-hosted' })).status, 503);
});
test('gateway rejects an indexed core-priority claim that disagrees with provenance', async () => {
  const worker = createGateway(async () => Response.json({ results: [{ found: 1, hits: [{ document: {
    id: 'a', domain: 'meaninggraph', visibility: 'public', title: 'Customer', kind: 'meaning_field',
    source_repository: 'meaninggraph/registry', core_priority: 1
  } }] }] }));
  const response = await worker.fetch(request({ q: 'Customer', domain: 'meaninggraph' }), env);
  assert.equal(response.status, 503);
  assert.deepEqual(await response.json(), { error: 'unavailable' });
});
test('gateway diagnostics disclose only a fixed stage, reason, and numeric upstream status', async () => {
  const logs = [];
  const warn = console.warn;
  console.warn = (...items) => logs.push(items);
  try {
    const secretQuery = 'private-query-marker';
    const secretKey = 'private-key-marker';
    const secretEnv = { ...env, REGISTRY_TYPESENSE_SEARCH_KEY: secretKey };
    const throwing = createGateway(async () => { throw new Error(`upstream ${secretQuery} ${secretKey}`); });
    assert.equal((await throwing.fetch(request({ q: secretQuery, domain: 'meaninggraph' }), secretEnv)).status, 503);
    assert.deepEqual(logs.at(-1), ['registry-search unavailable', { stage: 'engine_fetch', status: null, reason: 'exception' }]);
    const rejected = createGateway(async () => new Response('untrusted upstream body', { status: 502 }));
    assert.equal((await rejected.fetch(request({ q: secretQuery, domain: 'meaninggraph' }), secretEnv)).status, 503);
    assert.deepEqual(logs.at(-1), ['registry-search unavailable', { stage: 'engine_http', status: 502, reason: 'exception' }]);
    let calls = 0;
    const redirecting = createGateway(async () => { calls++; return Response.redirect('https://evil.example/multi_search', 302); });
    assert.equal((await redirecting.fetch(request({ q: secretQuery, domain: 'meaninggraph' }), secretEnv)).status, 503);
    assert.equal(calls, 1);
    assert.deepEqual(logs.at(-1), ['registry-search unavailable', { stage: 'engine_http', status: 302, reason: 'exception' }]);
    assert.equal((await rejected.fetch(request({ q: secretQuery, domain: 'meaninggraph' }), { ...secretEnv, REGISTRY_RATE_LIMITER: undefined })).status, 503);
    assert.deepEqual(logs.at(-1), ['registry-search unavailable', { stage: 'rate_limit', status: null, reason: 'missing_binding' }]);
    assert.doesNotMatch(JSON.stringify(logs), /private-query-marker|private-key-marker|untrusted upstream body/);
  } finally { console.warn = warn; }
});
test('production VM gateway pins the exact HTTPS engine host and search path', async () => {
  const live = { ...env, REGISTRY_SEARCH_MODE: 'production', REGISTRY_TYPESENSE_DEPLOYMENT: 'vm-pilot', REGISTRY_TYPESENSE_ORIGIN: 'https://vm1.sneat.dev/' };
  let target;
  const worker = createGateway(async url => {
    target = url.href;
    return Response.json({ results: [{ found: 0, hits: [] }] });
  });
  assert.equal((await worker.fetch(request({ q: 'Customer', domain: 'meaninggraph' }), live)).status, 200);
  assert.equal(target, 'https://vm1.sneat.dev/multi_search');
  for (const origin of ['https://vm1.sneat.dev.evil.example/', 'http://vm1.sneat.dev/', 'https://vm1.sneat.dev:8443/', 'https://vm1.sneat.dev/other']) {
    assert.equal((await worker.fetch(request({ q: 'Customer', domain: 'meaninggraph' }), { ...live, REGISTRY_TYPESENSE_ORIGIN: origin })).status, 503);
  }
  assert.throws(() => productionEngineOrigin({ ...live, REGISTRY_TYPESENSE_DEPLOYMENT: 'cloud', REGISTRY_TYPESENSE_CLOUD_HOST: 'cloud.example' }), /cloud engine host/);
  assert.equal((await worker.fetch(request({ q: 'Customer', domain: 'meaninggraph' }, { origin: 'https://evil.example' }), live)).status, 403);
  assert.equal((await worker.fetch(new Request('https://search.openvaultdb.com/v1/registry-search?x=1', { method: 'POST' }), live)).status, 404);
});
test('scheduled refresh builds pinned live manifest, keeps alias on failure, and increments the sequence', async () => {
  const { exports } = fixture();
  for (const envelope of exports) envelope.fixture = false;
  const hosts = { meaninggraph: 'meaninggraph.io', modelspec: 'modelspec.org', ovdb: 'directory.openvaultdb.com' };
  const fetcher = async url => {
    const domain = Object.keys(hosts).find(key => url === `https://${hosts[key]}/registry-search.json`);
    const response = Response.json(exports.find(item => item.domain === domain));
    Object.defineProperty(response, 'url', { value: url });
    return response;
  };
  const built = await fetchCurrentCorpus(fetcher);
  assert.equal(built.manifest.sources.length, 3);
  assert.equal(built.manifest.approved_revisions['shared/pin'], sha('f'));
  const dir = await mkdtemp(join(tmpdir(), 'registry-refresh-'));
  const engine = fakeEngine();
  const first = await refresh({ stateDir: dir, api: engine.api, fetcher });
  assert.equal(first.count, 3);
  assert.equal(JSON.parse(await readFile(join(dir, 'publication.json'))).sequence, 1);
  assert.equal((await refresh({ stateDir: dir, api: engine.api, fetcher })).unchanged, true);
  assert.equal(JSON.parse(await readFile(join(dir, 'publication.json'))).sequence, 1);
  const active = engine.alias;
  exports[0].fixture = true;
  await assert.rejects(refresh({ stateDir: dir, api: engine.api, fetcher }), /invalid public export/);
  assert.equal(engine.alias, active);
  exports[0].fixture = false;
  exports[0].documents[0].title = 'Updated Customer';
  engine.failImport = true;
  await assert.rejects(refresh({ stateDir: dir, api: engine.api, fetcher }), /rejected import/);
  assert.equal(engine.alias, active);
  exports[0].documents[0].title = 'Another Failed Candidate';
  await assert.rejects(refresh({ stateDir: dir, api: engine.api, fetcher }), /rejected import/);
  assert.equal(engine.alias, active);
  assert.equal(engine.collections.length, 2);
  engine.failImport = false;
  await refresh({ stateDir: dir, api: engine.api, fetcher });
  assert.notEqual(engine.alias, active);
  assert.equal(JSON.parse(await readFile(join(dir, 'publication.json'))).sequence, 2);
  exports[1].source_revisions['shared/pin'] = sha('e');
  await assert.rejects(refresh({ stateDir: dir, api: engine.api, fetcher }), /conflicting export source pins/);
  exports[1].source_revisions['shared/pin'] = sha('f');
  exports[0].documents[0].title = 'Updated Again';
  engine.otherAlias = active;
  await assert.rejects(refresh({ stateDir: dir, api: engine.api, fetcher }), /another alias/);
  assert.equal(engine.alias !== active, true);
  engine.otherAlias = null;
  await refresh({ stateDir: dir, api: engine.api, fetcher });
  exports[0].documents[0].title = 'Fourth Generation';
  engine.failDelete = true;
  await assert.rejects(refresh({ stateDir: dir, api: engine.api, fetcher }), /alias switched; inactive generation cleanup failed/);
  const switched = engine.alias;
  assert.notEqual(switched, active);
  engine.failDelete = false;
  await refresh({ stateDir: dir, api: engine.api, fetcher });
  assert.equal(engine.alias, switched);
  assert.equal(engine.collections.length, 2);
  exports[0].documents = Array(10_001).fill(exports[0].documents[0]);
  await assert.rejects(refresh({ stateDir: dir, api: engine.api, fetcher }), /pilot document capacity/);
  assert.equal(engine.collections.length, 2);
});
test('one-shot timer has an effective start timeout and private state path', () => {
  const unit = serviceUnit('/usr/bin/node');
  assert.match(unit, /TimeoutStartSec=240s/);
  assert.match(unit, /MemoryMax=768M/);
  assert.match(unit, /ReadWritePaths=\/opt\/datatug\/registry-search\/state/);
  assert.doesNotMatch(unit, /REGISTRY_TYPESENSE_ADMIN_KEY/);
});
test('refresh installer copies every local module its publisher imports', () => {
  const files = sourceFiles();
  for (const [name, bytes] of Object.entries(files)) {
    if (!name.endsWith('.mjs')) continue;
    const imports = [...bytes.toString('utf8').matchAll(/\bfrom ['"]\.\/([^'"]+)['"]/g)];
    for (const [, dependency] of imports) assert.ok(files[dependency], `${name} imports missing installed ${dependency}`);
  }
  assert.ok(files['provenance.mjs']);
});
test('Caddy route insertion preserves unrelated hosts and reverses exactly', async () => {
  const snippet = await readFile(new URL('../vm1-search.caddy', import.meta.url), 'utf8');
  const original = 'vm1.sneat.dev {\n    route {\n        handle {\n            respond 404\n        }\n    }\n}\nother.example {\n    basicauth secret-hash\n}\n';
  const installed = insertRoute(original, snippet);
  assert.equal(removeRoute(installed, snippet), original);
  assert.match(installed, /other\.example \{\n    basicauth secret-hash/);
  assert.throws(() => insertRoute(installed, snippet), /anchor missing|already owned/);
  assert.throws(() => removeRoute(installed.replace('reverse_proxy 127.0.0.1:8108', 'reverse_proxy 127.0.0.1:9999'), snippet), /owned Caddy route changed/);
});
test('gateway rejects populated engine cutoff results', async () => {
  const worker = createGateway(async () => Response.json({ results: [{ found: 1, search_cutoff: true, hits: [{ document: { id: 'a', domain: 'meaninggraph', visibility: 'public', title: 'Customer', kind: 'meaning_entity' } }] }] }));
  const response = await worker.fetch(request({ q: 'Customer', domain: 'meaninggraph' }), env);
  assert.equal(response.status, 503);
  assert.deepEqual(await response.json(), { error: 'unavailable' });
});
test('publisher retains lock after wrapper death; its own death releases it', async () => {
  if (!['darwin', 'linux'].includes(process.platform)) return;
  const dir = await mkdtemp(join(tmpdir(), 'registry-lock-'));
  const lock = join(dir, 'publication.lock');
  const marker = join(dir, 'entered');
  const lockModule = new URL('../lock.mjs', import.meta.url).href;
  const publisher = `import(${JSON.stringify(lockModule)}).then(({assertPublicationLock}) => { assertPublicationLock(${JSON.stringify(lock)}); require('node:fs').writeFileSync(${JSON.stringify(marker)}, String(process.pid)); setTimeout(() => {}, 5000); })`;
  const wrapper = `import(${JSON.stringify(lockModule)}).then(({runUnderPublicationLock}) => runUnderPublicationLock(${JSON.stringify(lock)}, process.execPath, ['-e', ${JSON.stringify(publisher)}], process.env, 'ignore'))`;
  const holder = spawn(process.execPath, ['-e', wrapper], { stdio: 'ignore' });
  const exited = new Promise(resolve => holder.once('exit', resolve));
  const probe = () => spawnSync('python3', ['-c', 'import fcntl,os,sys; f=os.open(sys.argv[1],os.O_RDWR|os.O_CREAT,0o600);\ntry: fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)\nexcept BlockingIOError: sys.exit(42)', lock], { stdio: 'ignore' }).status;
  let publisherPid;
  try {
    for (let i = 0; i < 50; i++) { try { await access(marker); break; } catch { await new Promise(resolve => setTimeout(resolve, 20)); } }
    await access(marker);
    publisherPid = Number(await readFile(marker, 'utf8'));
    assert.equal(probe(), 42);
    holder.kill('SIGKILL');
    await exited;
    process.kill(publisherPid, 0);
    assert.equal(probe(), 42);
  } finally {
    if (publisherPid) process.kill(publisherPid, 'SIGKILL');
    else holder.kill('SIGKILL');
  }
  for (let i = 0; i < 50 && probe() !== 0; i++) await new Promise(resolve => setTimeout(resolve, 20));
  assert.equal(probe(), 0);
  assert.equal(await runUnderPublicationLock(lock, process.execPath, ['-e', ''], process.env, 'ignore'), 0);
  assert.throws(() => assertPublicationLock(lock), /not held/);
});
test('publisher lock serializes contenders without unlinking its file', async () => {
  if (!['darwin', 'linux'].includes(process.platform)) return;
  const dir = await mkdtemp(join(tmpdir(), 'registry-lock-order-'));
  const lock = join(dir, 'publication.lock');
  const order = join(dir, 'order');
  const first = runUnderPublicationLock(lock, process.execPath, ['-e', `require('node:fs').appendFileSync(${JSON.stringify(order)}, 'A'); setTimeout(() => {}, 300)`], process.env, 'ignore');
  for (let i = 0; i < 50; i++) { try { await access(order); break; } catch { await new Promise(resolve => setTimeout(resolve, 20)); } }
  const start = Date.now();
  const second = runUnderPublicationLock(lock, process.execPath, ['-e', `require('node:fs').appendFileSync(${JSON.stringify(order)}, 'B')`], process.env, 'ignore');
  assert.deepEqual(await Promise.all([first, second]), [0, 0]);
  assert.equal(await readFile(order, 'utf8'), 'AB');
  assert.ok(Date.now() - start >= 150);
});
test('local plaintext engine access requires an explicit loopback proof', () => {
  assert.throws(() => typesenseClient('http://127.0.0.1:18108/', 'key'), /invalid engine/);
  assert.throws(() => typesenseClient('http://example.com/', 'key', fetch, { allowLocalHTTP: true }), /invalid engine/);
  assert.equal(typeof typesenseClient('http://127.0.0.1:18108/', 'key', fetch, { allowLocalHTTP: true }), 'function');
});
