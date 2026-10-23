import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdtemp, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { stableId } from '../schema.mjs';
import { validateManifest, mergeExports } from '../merge.mjs';
import { loadExports, publish } from '../publish.mjs';
import { fetchCurrentCorpus, refresh } from '../refresh.mjs';
import { combinedURL, directoryURL, combinedRepository, inputProfile } from '../profiles.mjs';
import { fakeEngine } from './fake-engine.mjs';

// Synthetic consumer fixtures only: these are not native producer admission proofs.
const sha = c => c.repeat(40), hash = c => c.repeat(64);
const digest = bytes => createHash('sha256').update(bytes).digest('hex');
const repositories = ['meaninggraph/registry', 'modelspec-org/registry', 'openvaultdb/directory'];
function document(domain, kind, native_id, canonical_url, repository, commit) {
  return { id: stableId(domain, kind, native_id), domain, kind, native_id, title: native_id, identifier: native_id, qualified_name: `${domain}.${native_id}`, canonical_url, visibility: 'public', source_repository: repository, source_commit: commit, source_path: 'source.json' };
}
function fixture() {
  const semantic = document('meaninggraph', 'meaning_entity', 'Customer', 'https://meaninggraph.io/graphs/demo/concepts/Customer/', repositories[0], sha('1'));
  const model = document('modelspec', 'model', 'commerce', `https://meaninggraph.io/registry/models/commerce/versions/${sha('2')}/`, repositories[1], sha('2'));
  const record = document('modelspec', 'model_entity', 'commerce/Customer', `${model.canonical_url}#member-${hash('c')}`, repositories[1], sha('2'));
  record.parent_id = model.id;
  const field = document('modelspec', 'model_field', 'commerce/Customer/Name', `${model.canonical_url}#member-${hash('d')}`, repositories[1], sha('2'));
  field.parent_id = record.id;
  const database = document('ovdb', 'ovdb_database', 'chinook', 'https://directory.openvaultdb.com/databases/chinook/', repositories[2], sha('3'));
  const sources = [
    { domains: ['meaninggraph', 'modelspec'], repository: combinedRepository, revision: sha('1'), catalog_sha256: hash('b'), sha256: hash('a'), file: '/tmp/combined.json' },
    { domain: 'ovdb', repository: repositories[2], revision: sha('3'), sha256: hash('a'), file: '/tmp/directory.json' }
  ];
  return {
    manifest: { format: 'registry-search-manifest/v2', sequence: 1, sources, approved_revisions: { [repositories[1]]: sha('2'), 'shared/pin': sha('f') }, empty_domains: [] },
    exports: [
      { format: 'registry-search-export/v2', domains: ['meaninggraph', 'modelspec'], producer: { repository: combinedRepository, revision: sha('1'), catalog_sha256: hash('b') }, fixture: true, source_revisions: { [repositories[0]]: sha('1'), [repositories[1]]: sha('2'), 'shared/pin': sha('f') }, documents: [semantic, model, record, field] },
      { format: 'registry-search-export/v1', domain: 'ovdb', fixture: true, source_revisions: { [repositories[2]]: sha('3'), 'shared/pin': sha('f') }, documents: [database] }
    ]
  };
}
function remote(input = fixture()) {
  const { manifest, exports } = structuredClone(input);
  manifest.sources.forEach((source, i) => { delete source.file; source.url = i ? directoryURL : combinedURL; });
  exports.forEach(envelope => { envelope.fixture = false; });
  return { manifest, exports };
}
function smoke(exports) {
  return ['meaninggraph', 'modelspec', 'ovdb'].flatMap(domain => {
    const doc = exports.flatMap(envelope => envelope.documents).find(doc => doc.domain === domain);
    return doc ? [{ domain, id: doc.id, q: doc.identifier }] : [];
  });
}
function transport(exports, mutate) {
  const seen = [];
  const fetcher = async (url, options) => {
    seen.push(url);
    assert.equal(options.redirect, 'error');
    assert.ok(options.signal);
    const index = [combinedURL, directoryURL].indexOf(url);
    assert.notEqual(index, -1);
    const response = new Response(JSON.stringify(exports[index]), { headers: { 'content-type': 'application/json' } });
    Object.defineProperty(response, 'url', { value: url });
    return mutate ? mutate(response) : response;
  };
  return { fetcher, seen };
}
function empties(input, domains) {
  for (const envelope of input.exports) envelope.documents = envelope.documents.filter(doc => !domains.includes(doc.domain));
  input.manifest.empty_domains = domains;
  return input;
}

test('v2 merges the exact two-feed shape without rewriting stable IDs or record kinds', () => {
  const { manifest, exports } = fixture();
  const merged = mergeExports(manifest, exports);
  assert.equal(merged.documents.length, 5);
  assert.deepEqual(new Set(merged.documents.map(doc => doc.id)), new Set(exports.flatMap(envelope => envelope.documents).map(doc => doc.id)));
  const record = merged.documents.find(doc => doc.kind === 'model_entity');
  assert.equal(record.field_count, 1);
  assert.deepEqual(record.field_preview, ['commerce/Customer/Name']);
  assert.equal(mergeExports(manifest, [...exports].reverse()).hash, merged.hash);
  assert.equal(merged.hash, mergeExports(manifest, exports).hash);
});

test('v2 refuses unknown/ambiguous profiles, mixed feeds and source discriminators', () => {
  assert.equal(inputProfile(), 'legacy');
  assert.equal(inputProfile('combined-v2'), 'combined-v2');
  for (const profile of ['', null, 'v2', 'combined']) assert.throws(() => inputProfile(profile), /unknown/);
  const mutations = [
    x => { x.manifest.format = 'registry-search-manifest/v3'; },
    x => { x.manifest.sources.push(x.manifest.sources[1]); },
    x => { x.manifest.sources[0].domain = 'meaninggraph'; },
    x => { x.manifest.sources[0].domains.reverse(); },
    x => { x.manifest.sources[1].domains = ['ovdb']; },
    x => { x.manifest.sources[1] = x.manifest.sources[0]; },
    x => { x.exports[0].domain = 'meaninggraph'; },
    x => { x.exports[0].domains = ['meaninggraph']; },
    x => { x.exports[0].format = 'registry-search-export/v1'; x.exports[0].domain = 'combined'; },
    x => { x.exports[1] = x.exports[0]; },
    x => { x.exports[1].domains = ['ovdb']; },
    x => { x.exports[0].documents.push(x.exports[1].documents[0]); }
  ];
  for (const mutate of mutations) { const x = fixture(); mutate(x); assert.throws(() => mergeExports(x.manifest, x.exports)); }
});

test('v2 binds producer repository revision catalog digest and envelope pin exactly', () => {
  const mutations = [
    x => { x.manifest.sources[0].repository = 'modelspec-org/registry'; },
    x => { x.manifest.sources[0].revision = sha('2'); },
    x => { x.manifest.sources[0].catalog_sha256 = hash('c'); },
    x => { x.manifest.sources[0].catalog_sha256 = 'x'; },
    x => { x.exports[0].producer.repository = 'meaninggraph/core'; },
    x => { x.exports[0].producer.revision = sha('2'); },
    x => { x.exports[0].producer.catalog_sha256 = hash('c'); },
    x => { x.exports[0].producer.extra = true; },
    x => { delete x.exports[0].producer; },
    x => { x.exports[0].source_revisions[combinedRepository] = sha('4'); },
    x => { x.manifest.approved_revisions[combinedRepository] = sha('4'); },
    x => { x.exports[1].source_revisions['shared/pin'] = sha('e'); },
    x => { delete x.manifest.approved_revisions['shared/pin']; },
    x => { x.exports[0].source_revisions = []; },
    x => { delete x.exports[0].source_revisions[repositories[1]]; }
  ];
  for (const mutate of mutations) { const x = fixture(); mutate(x); assert.throws(() => mergeExports(x.manifest, x.exports)); }
});

test('v2 acquisition uses exact fixed host/path pairs and mutually exclusive fixture input', async () => {
  for (const url of [directoryURL, 'https://meaninggraph.io/registry-search.json', 'https://modelspec.org/registry-search-combined.json', combinedURL + '?x=1', combinedURL + '#x', 'https://meaninggraph.io:443/registry-search-combined.json', 'https://meaninggraph.io/x/../registry-search-combined.json']) {
    const x = remote(); x.manifest.sources[0].url = url;
    assert.throws(() => validateManifest(x.manifest), /unsafe source URL/);
  }
  const local = fixture();
  assert.throws(() => validateManifest(local.manifest), /fixture forbidden/);
  local.manifest.sources[0].url = combinedURL;
  assert.throws(() => validateManifest(local.manifest, { allowFixtures: true }), /fixture forbidden/);
  const x = remote();
  const t = transport(x.exports);
  x.exports.forEach((envelope, i) => { x.manifest.sources[i].sha256 = digest(JSON.stringify(envelope)); });
  assert.deepEqual(await loadExports(x.manifest, { fetcher: t.fetcher }), x.exports);
  x.manifest.sources[0].sha256 = hash('0');
  await assert.rejects(loadExports(x.manifest, { fetcher: t.fetcher }), /checksum/);
  const redirected = transport(x.exports, response => { Object.defineProperty(response, 'redirected', { value: true }); return response; });
  await assert.rejects(loadExports(x.manifest, { fetcher: redirected.fetcher }), /fetch failed/);
});

test('v2 document kinds must match technical domain while legacy acceptance stays intact', () => {
  const x = fixture();
  const doc = x.exports[0].documents[0];
  doc.kind = 'model'; doc.id = stableId(doc.domain, doc.kind, doc.native_id);
  assert.throws(() => mergeExports(x.manifest, x.exports), /kind domain/);
  const legacySources = repositories.map((repository, i) => ({ domain: ['meaninggraph', 'modelspec', 'ovdb'][i], repository, revision: sha(String(i + 1)), sha256: hash('a'), file: '/tmp/test.json' }));
  const legacyExports = legacySources.map(source => ({ format: 'registry-search-export/v1', domain: source.domain, fixture: true, source_revisions: { [source.repository]: source.revision }, documents: [] }));
  legacyExports[0].documents = [doc];
  const manifest = { sequence: 1, sources: legacySources, approved_revisions: {} };
  assert.equal(mergeExports(manifest, legacyExports).documents[0].id, doc.id);
});

test('v2 native models use exact version URLs, matching commit and member-hash fragments', () => {
  const canonical = fixture().exports[0].documents[2].canonical_url;
  const invalid = [
    canonical.replace('meaninggraph.io', 'modelspec.org'), canonical.replace('meaninggraph.io', 'meaninggraph.io.evil.test'),
    canonical.replace(sha('2'), sha('3')), canonical.replace('#member-', '#record-'), canonical.replace(hash('c'), 'short'),
    canonical.replace('/commerce/', '/Commerce/'), canonical.replace('/commerce/', '/com%6derce/'),
    canonical.replace('/commerce/', '/x/../commerce/'), canonical.replace('/commerce/', '/%2e%2e/commerce/'),
    canonical.replace('/commerce/', '/commerce%2fescape/'), canonical.replace('https:', 'http:'),
    canonical.replace('meaninggraph.io', 'meaninggraph.io:443'), canonical.replace('meaninggraph.io', 'user@meaninggraph.io'),
    canonical.replace('meaninggraph.io', 'MEANINGGRAPH.IO'), canonical.replace('/versions/' + sha('2') + '/', '/'),
    canonical.split('#')[0], canonical + '?x=1', canonical.replace('#member-', '?x=1#member-')
  ];
  for (const url of invalid) { const x = fixture(); x.exports[0].documents[2].canonical_url = url; assert.throws(() => mergeExports(x.manifest, x.exports), /unsafe canonical/, url); }
  const landing = fixture(); landing.exports[0].documents[1].canonical_url = 'https://meaninggraph.io/registry/models/commerce/';
  assert.equal(mergeExports(landing.manifest, landing.exports).documents.length, 5);
  landing.exports[0].documents[1].canonical_url += '#';
  assert.throws(() => mergeExports(landing.manifest, landing.exports), /unsafe canonical/);
  const semantic = fixture(); semantic.exports[0].documents[0].canonical_url = canonical;
  assert.throws(() => mergeExports(semantic.manifest, semantic.exports), /unsafe canonical/);
});

test('v2 model-field parent proof rejects dangling, cross-family and wrong-source parents', () => {
  for (const mutate of [
    x => { x.exports[0].documents[3].parent_id = hash('0'); },
    x => { x.exports[0].documents[3].parent_id = x.exports[0].documents[1].id; },
    x => { x.exports[0].documents[3].parent_id = x.exports[0].documents[0].id; },
    x => { const parent = x.exports[0].documents[2]; parent.source_repository = combinedRepository; parent.source_commit = sha('1'); parent.canonical_url = parent.canonical_url.replace(sha('2'), sha('1')); }
  ]) { const x = fixture(); mutate(x); assert.throws(() => mergeExports(x.manifest, x.exports), /parent provenance/); }
  for (const kind of ['model_record', 'model_collection']) {
    const x = fixture(), parent = x.exports[0].documents[2];
    parent.kind = kind; parent.id = stableId(parent.domain, kind, parent.native_id); x.exports[0].documents[3].parent_id = parent.id;
    assert.equal(mergeExports(x.manifest, x.exports).documents.length, 5);
  }
});

test('v2 semantic parents may be exact emitted current cross-repository entities; independent fields omit parents', () => {
  const x = fixture(), parent = x.exports[0].documents[0];
  x.manifest.approved_revisions['meaninggraph/core'] = sha('e');
  x.exports[0].source_revisions['meaninggraph/core'] = sha('e');
  const field = document('meaninggraph', 'meaning_field', 'country', 'https://meaninggraph.io/graphs/core/concepts/country/', 'meaninggraph/core', sha('e'));
  field.parent_id = parent.id; x.exports[0].documents.push(field);
  assert.equal(mergeExports(x.manifest, x.exports).documents.length, 6);
  delete field.parent_id;
  assert.equal(mergeExports(x.manifest, x.exports).documents.length, 6);
  field.parent_id = x.exports[0].documents[2].id;
  assert.throws(() => mergeExports(x.manifest, x.exports), /parent provenance/);
});

test('v2 preserves duplicate/private/provenance and new-kind refusal', () => {
  for (const mutate of [
    x => x.exports[0].documents.push(x.exports[0].documents[0]),
    x => { x.exports[0].documents[0].visibility = 'private'; },
    x => { x.exports[0].documents[0].kind = 'component'; },
    x => { x.exports[0].documents[0].source_commit = sha('9'); },
    x => { x.exports[0].documents[0].id = hash('0'); },
    x => { x.exports[0].documents[0].source_repository = 'unapproved/repo'; }
  ]) { const x = fixture(); mutate(x); assert.throws(() => mergeExports(x.manifest, x.exports)); }
});

test('v2 empty_domains is exact and wholly empty corpus retains separate approval', () => {
  for (const domains of [['meaninggraph'], ['modelspec'], ['ovdb'], ['meaninggraph', 'modelspec']]) {
    const x = empties(fixture(), domains);
    assert.ok(mergeExports(x.manifest, x.exports).documents.length);
    x.manifest.empty_domains = [];
    assert.throws(() => mergeExports(x.manifest, x.exports), /empty domains/);
  }
  const x = empties(fixture(), ['meaninggraph', 'modelspec', 'ovdb']);
  assert.throws(() => mergeExports(x.manifest, x.exports), /explicit review/);
  x.manifest.allow_empty = 'false';
  assert.throws(() => mergeExports(x.manifest, x.exports), /explicit review/);
  x.manifest.allow_empty = true;
  assert.equal(mergeExports(x.manifest, x.exports).documents.length, 0);
  x.manifest.empty_domains.push('ovdb');
  assert.throws(() => mergeExports(x.manifest, x.exports), /invalid empty/);
});

test('v2 refresh is opt-in, uses two fixed feeds and binds raw export bytes', async () => {
  const x = remote(), t = transport(x.exports);
  const fetched = await fetchCurrentCorpus(t.fetcher, { profile: 'combined-v2' });
  assert.deepEqual(t.seen, [combinedURL, directoryURL]);
  assert.equal(fetched.manifest.format, 'registry-search-manifest/v2');
  assert.deepEqual(fetched.manifest.empty_domains, []);
  assert.equal(fetched.manifest.sources[0].sha256, digest(JSON.stringify(x.exports[0])));
  assert.deepEqual(new Set(fetched.manifest.smoke_queries.map(q => q.domain)), new Set(['meaninggraph', 'modelspec', 'ovdb']));
  assert.equal(mergeExports(fetched.manifest, fetched.exports).documents.length, 5);
  await assert.rejects(fetchCurrentCorpus(t.fetcher, { profile: 'unknown' }), /unknown/);
  const conflict = remote(); conflict.exports[1].source_revisions['shared/pin'] = sha('e');
  await assert.rejects(fetchCurrentCorpus(transport(conflict.exports).fetcher, { profile: 'combined-v2' }), /conflicting export/);
});

test('v2 refresh never silently approves an empty family or fabricates a smoke target', async () => {
  for (const domains of [['meaninggraph'], ['modelspec'], ['meaninggraph', 'modelspec']]) {
    const x = remote(empties(fixture(), domains));
    await assert.rejects(fetchCurrentCorpus(transport(x.exports).fetcher, { profile: 'combined-v2' }), /unapproved empty/);
    const fetched = await fetchCurrentCorpus(transport(x.exports).fetcher, { profile: 'combined-v2', emptyDomains: domains });
    assert.deepEqual(fetched.manifest.empty_domains, domains);
    assert.equal(fetched.manifest.smoke_queries.some(query => domains.includes(query.domain)), false);
  }
  const x = remote(empties(fixture(), ['meaninggraph', 'modelspec', 'ovdb']));
  await assert.rejects(fetchCurrentCorpus(transport(x.exports).fetcher, { profile: 'combined-v2', emptyDomains: ['meaninggraph', 'modelspec', 'ovdb'] }), /explicit review/);
});

test('v2 publisher smokes only populated families including explicitly reviewed full empty', async () => {
  for (const domains of [[], ['meaninggraph'], ['modelspec'], ['ovdb'], ['meaninggraph', 'modelspec'], ['meaninggraph', 'modelspec', 'ovdb']]) {
    const x = remote(empties(fixture(), domains));
    if (domains.length === 3) x.manifest.allow_empty = true;
    const engine = fakeEngine(), stateDir = await mkdtemp(join(tmpdir(), 'combined-publish-'));
    const result = await publish(x.manifest, x.exports, { api: engine.api, stateDir, smokeQueries: smoke(x.exports) });
    assert.equal(engine.alias, `registry_metadata_${result.generation}`);
    assert.equal(result.count, x.exports.flatMap(e => e.documents).length);
    assert.equal((await publish(x.manifest, x.exports, { api: engine.api, stateDir, smokeQueries: smoke(x.exports) })).unchanged, true);
  }
});

test('v2 invalid input and smoke evidence are refused before publication engine calls', async () => {
  for (const mutate of [
    (x, queries) => { x.exports[0].producer.catalog_sha256 = hash('e'); },
    (x, queries) => { queries.pop(); },
    (x, queries) => { queries[0].id = hash('e'); },
    (x, queries) => { queries.push({ domain: 'other', q: 'x', id: 'x' }); }
  ]) {
    const x = remote(), queries = smoke(x.exports); mutate(x, queries);
    let calls = 0;
    await assert.rejects(publish(x.manifest, x.exports, { api: async () => { calls++; throw Error('engine touched'); }, stateDir: '/tmp/unused-combined', smokeQueries: queries }));
    assert.equal(calls, 0);
  }
});

test('v2 import/smoke failure preserves active generation and sequence fencing remains', async () => {
  const x = remote(), engine = fakeEngine(), stateDir = await mkdtemp(join(tmpdir(), 'combined-rollback-'));
  const first = await publish(x.manifest, x.exports, { api: engine.api, stateDir, smokeQueries: smoke(x.exports) });
  const active = engine.alias, state = await readFile(join(stateDir, 'publication.json'), 'utf8');
  x.manifest.sequence = 2; x.exports[0].documents[0].title += ' changed';
  engine.failImport = true;
  await assert.rejects(publish(x.manifest, x.exports, { api: engine.api, stateDir, smokeQueries: smoke(x.exports) }), /rejected import/);
  assert.equal(engine.alias, active); assert.equal(await readFile(join(stateDir, 'publication.json'), 'utf8'), state);
  engine.failImport = false;
  const badSmoke = smoke(x.exports); badSmoke[0].q = 'does-not-exist';
  await assert.rejects(publish(x.manifest, x.exports, { api: engine.api, stateDir, smokeQueries: badSmoke }), /smoke query failed/);
  assert.equal(engine.alias, active); assert.equal(await readFile(join(stateDir, 'publication.json'), 'utf8'), state);
  const second = await publish(x.manifest, x.exports, { api: engine.api, stateDir, smokeQueries: smoke(x.exports) });
  assert.notEqual(second.generation, first.generation);
  assert.ok(engine.collections.includes(active));
  x.manifest.sequence = 1;
  await assert.rejects(publish(x.manifest, x.exports, { api: engine.api, stateDir, smokeQueries: smoke(x.exports) }), /stale/);
});

test('v2 refresh shares durable sequence state without enabling a service', async () => {
  const x = remote(), engine = fakeEngine(), stateDir = await mkdtemp(join(tmpdir(), 'combined-refresh-'));
  const options = { api: engine.api, stateDir, fetcher: transport(x.exports).fetcher, profile: 'combined-v2' };
  const first = await refresh(options);
  assert.equal(JSON.parse(await readFile(join(stateDir, 'publication.json'))).sequence, 1);
  assert.equal((await refresh(options)).unchanged, true);
  const manifest = JSON.parse(await readFile(join(stateDir, 'live-manifest.json')));
  assert.equal(manifest.format, 'registry-search-manifest/v2');
  x.exports[0].documents[0].title += ' newer';
  assert.notEqual((await refresh(options)).generation, first.generation);
  assert.equal(JSON.parse(await readFile(join(stateDir, 'publication.json'))).sequence, 2);
});

test('v2 same-document legacy transition reuses published evidence with valid sequence and refuses bad targets', async () => {
  const current = remote();
  for (const doc of current.exports[0].documents.filter(doc => doc.domain === 'modelspec')) {
    doc.canonical_url = `https://modelspec.org/registry/models/commerce/${doc.kind === 'model' ? '' : '#record-' + (doc.kind === 'model_field' ? 'Name' : 'Customer')}`;
  }
  const legacySources = ['meaninggraph', 'modelspec', 'ovdb'].map((domain, i) => ({ domain, repository: repositories[i], revision: sha(String(i + 1)), sha256: hash('a'), url: `https://${['meaninggraph.io', 'modelspec.org', 'directory.openvaultdb.com'][i]}/registry-search.json` }));
  const documents = current.exports.flatMap(envelope => envelope.documents);
  const pins = Object.fromEntries(repositories.map((repo, i) => [repo, sha(String(i + 1))]));
  const legacyExports = legacySources.map(source => ({ format: 'registry-search-export/v1', domain: source.domain, fixture: false, source_revisions: pins, documents: documents.filter(doc => doc.domain === source.domain) }));
  const legacyManifest = { sequence: 1, sources: legacySources, approved_revisions: {} };
  assert.equal(mergeExports(legacyManifest, legacyExports).generation, mergeExports(current.manifest, current.exports).generation);
  const engine = fakeEngine(), stateDir = await mkdtemp(join(tmpdir(), 'combined-transition-'));
  const prior = await publish(legacyManifest, legacyExports, { api: engine.api, stateDir, smokeQueries: smoke(legacyExports) });
  current.manifest.sequence = 2;
  let searches = 0;
  const api = async (method, path, ...args) => { if (path.includes('/documents/search?')) searches++; return engine.api(method, path, ...args); };
  const next = await publish(current.manifest, current.exports, { api, stateDir, smokeQueries: smoke(current.exports) });
  assert.equal(next.unchanged, true);
  assert.equal(next.generation, prior.generation);
  assert.equal(searches, 0); // Existing published byte-identical generation already has smoke evidence.
  assert.equal(JSON.parse(await readFile(join(stateDir, 'publication.json'))).sequence, 2);
  assert.equal((await publish(current.manifest, current.exports, { api, stateDir, smokeQueries: smoke(current.exports) })).unchanged, true);
  const bad = smoke(current.exports); bad[0].id = hash('0');
  await assert.rejects(publish(current.manifest, current.exports, { api, stateDir, smokeQueries: bad }), /invalid smoke/);
  assert.equal(engine.alias, `registry_metadata_${prior.generation}`);
});
