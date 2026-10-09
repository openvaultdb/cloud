import { test } from 'node:test';
import assert from 'node:assert/strict';
import { stableId, validateDocument, kinds as schemaKinds } from '../schema.mjs';
import { kinds, fieldParentKinds, kindsForFilter, kindPriority } from '../provenance.mjs';
import { mergeExports } from '../merge.mjs';
import { createGateway } from '../gateway.mjs';
import { generate } from '../benchmark.mjs';

// model_record is the current name of a ModelSpec record type; model_entity is the earlier name.
// Both must behave identically everywhere, and a filter for either must return both.
const sha = char => char.repeat(40);
const source = { domain: 'modelspec', repository: 'modelspec-org/registry', revision: sha('2'), sha256: 'b'.repeat(64), file: '/tmp/modelspec.json' };
const routes = { modelspec: { hosts: ['modelspec.org'] } };
const revisions = new Map([[source.repository, source.revision]]);
const base = { visibility: 'public', source_repository: source.repository, source_commit: source.revision, source_path: 'models/commerce.json' };

function node(kind, nativeId, title, extra = {}) {
  return {
    ...base, id: stableId('modelspec', kind, nativeId), domain: 'modelspec', kind, native_id: nativeId,
    title, identifier: title, qualified_name: `commerce.${title}`,
    canonical_url: `https://modelspec.org/registry/models/commerce/#${kind}-${title}`, ...extra
  };
}
// One model with a record type spelled the earlier way (Customer), one spelled the current way
// (Invoice), each with two fields, as the corpus looks while one site has switched and another has not.
function mixedCorpus() {
  const model = node('model', 'modelspec/commerce', 'commerce', { canonical_url: 'https://modelspec.org/registry/models/commerce/' });
  const customer = node('model_entity', 'modelspec/commerce/Customer', 'Customer', { parent_id: model.id, parent_label: 'commerce' });
  const invoice = node('model_record', 'modelspec/commerce/Invoice', 'Invoice', { parent_id: model.id, parent_label: 'commerce' });
  const fields = [customer, invoice].flatMap(parent => ['name', 'total'].map(member =>
    node('model_field', `${parent.native_id}/${member}`, `${parent.title}_${member}`, { parent_id: parent.id, parent_label: parent.title })));
  const exports = [
    { format: 'registry-search-export/v1', domain: 'meaninggraph', fixture: true, source_revisions: { 'meaninggraph/registry': sha('1') }, documents: [] },
    { format: 'registry-search-export/v1', domain: 'modelspec', fixture: true, source_revisions: { [source.repository]: source.revision }, documents: [model, customer, invoice, ...fields] },
    { format: 'registry-search-export/v1', domain: 'ovdb', fixture: true, source_revisions: { 'openvaultdb/directory': sha('3') }, documents: [] }
  ];
  const sources = [
    { domain: 'meaninggraph', repository: 'meaninggraph/registry', revision: sha('1'), sha256: 'a'.repeat(64), file: '/tmp/meaninggraph.json' },
    source,
    { domain: 'ovdb', repository: 'openvaultdb/directory', revision: sha('3'), sha256: 'c'.repeat(64), file: '/tmp/ovdb.json' }
  ];
  return { manifest: { sequence: 1, sources, approved_revisions: {}, allow_empty: true }, exports, model, customer, invoice };
}

test('both record-type kinds pass validation; an unknown kind is still refused', () => {
  for (const kind of ['model_entity', 'model_record', 'model_collection']) {
    const doc = node(kind, `modelspec/commerce/${kind}`, 'Thing');
    assert.equal(validateDocument(doc, source, routes, revisions), doc);
  }
  for (const kind of ['model_recordset', 'model_property', 'record', 'model_Record']) {
    const doc = { ...node('model_record', 'modelspec/commerce/Thing', 'Thing'), kind };
    doc.id = stableId('modelspec', kind, doc.native_id);
    assert.throws(() => validateDocument(doc, source, routes, revisions), /invalid public scope/);
  }
  // The id hashes the kind, so a document cannot carry one kind with the other kind's id.
  const forged = { ...node('model_entity', 'modelspec/commerce/Thing', 'Thing'), kind: 'model_record' };
  assert.throws(() => validateDocument(forged, source, routes, revisions), /unstable document id/);
  assert.notEqual(stableId('modelspec', 'model_entity', 'x'), stableId('modelspec', 'model_record', 'x'));
  assert.equal(schemaKinds, kinds);
  assert.ok(kinds.includes('model_entity') && kinds.includes('model_record') && kinds.includes('model_collection'));
  assert.equal(new Set(kinds).size, kinds.length);
});

test('model_record has the same priority and field context as model_entity', () => {
  assert.equal(kindPriority({ kind: 'model_record' }), 2);
  assert.equal(kindPriority({ kind: 'model_record' }), kindPriority({ kind: 'model_entity' }));
  assert.ok(kindPriority({ kind: 'model' }) > kindPriority({ kind: 'model_record' }));
  assert.ok(kindPriority({ kind: 'model_record' }) > kindPriority({ kind: 'model_field' }));
  assert.equal(kindPriority({ kind: 'model_unknown' }), 0);
  assert.ok(fieldParentKinds.includes('model_record') && fieldParentKinds.includes('model_entity'));
});

test('the kind filter for either spelling expands to both, without duplicates', () => {
  assert.deepEqual(kindsForFilter([]), []);
  assert.deepEqual(kindsForFilter(['model_entity']), ['model_entity', 'model_record']);
  assert.deepEqual(kindsForFilter(['model_record']), ['model_entity', 'model_record']);
  assert.deepEqual(kindsForFilter(['model_record', 'model_entity']), ['model_entity', 'model_record']);
  assert.deepEqual(kindsForFilter(['model_field', 'model_record']), ['model_field', 'model_entity', 'model_record']);
  assert.deepEqual(kindsForFilter(['model_field', 'model_collection', 'model']), ['model_field', 'model_collection', 'model']);
});

test('a mixed corpus merges: both kinds get priority 2, a field preview and a count', () => {
  const { manifest, exports, customer, invoice } = mixedCorpus();
  const merged = mergeExports(manifest, exports);
  assert.equal(merged.documents.length, 7);
  for (const record of [customer, invoice]) {
    const doc = merged.documents.find(item => item.id === record.id);
    assert.equal(doc.kind, record.kind, 'stored kind is not rewritten');
    assert.equal(doc.kind_priority, 2);
    assert.equal(doc.field_count, 2);
    assert.deepEqual(doc.field_preview, [`${record.title}_name`, `${record.title}_total`]);
  }
  assert.equal(merged.documents.filter(doc => doc.kind === 'model_field').every(doc => doc.kind_priority === 0 && doc.field_count === undefined), true);
  assert.equal(merged.hash, mergeExports(mixedCorpus().manifest, mixedCorpus().exports).hash);
});

test('a corpus with only the earlier kind merges to the same shape as before', () => {
  const { manifest, exports, customer } = mixedCorpus();
  exports[1].documents = exports[1].documents.filter(doc => doc.kind !== 'model_record' && doc.parent_id !== mixedCorpus().invoice.id);
  const merged = mergeExports(manifest, exports);
  assert.equal(merged.documents.length, 4);
  assert.equal(merged.documents.find(doc => doc.id === customer.id).kind_priority, 2);
  assert.equal(merged.documents.filter(doc => doc.kind === 'model_record').length, 0);
});

test('a corpus with only the current kind merges, and an unknown kind fails the whole merge', () => {
  const { manifest, exports, customer } = mixedCorpus();
  exports[1].documents = exports[1].documents.filter(doc => doc.kind !== 'model_entity' && doc.parent_id !== customer.id);
  assert.equal(mergeExports(manifest, exports).documents.length, 4);
  const bad = mixedCorpus();
  const stray = node('model_entity', 'modelspec/commerce/Stray', 'Stray');
  stray.kind = 'model_stray';
  stray.id = stableId('modelspec', 'model_stray', stray.native_id);
  bad.exports[1].documents.push(stray);
  assert.throws(() => mergeExports(bad.manifest, bad.exports), /invalid public scope/);
});

const env = { REGISTRY_SEARCH_MODE: 'staging', REGISTRY_TYPESENSE_ORIGIN: 'https://engine.example/', REGISTRY_TYPESENSE_SEARCH_KEY: 'secret', REGISTRY_RATE_LIMITER: { limit: async () => ({ success: true }) } };
const request = body => new Request('https://search.example/v1/registry-search', { method: 'POST', headers: { 'content-type': 'application/json', origin: 'https://modelspec.org' }, body: JSON.stringify(body) });
// An engine stand-in that serves the merged documents and honours the gateway's kind filter.
function gatewayOver(documents, seen = []) {
  return createGateway(async (_url, options) => {
    const search = JSON.parse(options.body).searches[0];
    seen.push(search.filter_by);
    const wanted = /kind:=\[([^\]]*)\]/.exec(search.filter_by)?.[1].split(',');
    const hits = documents.filter(doc => !wanted || wanted.includes(doc.kind)).map(document => ({ document }));
    return Response.json({ results: [{ found: hits.length, hits }] });
  });
}
const search = (worker, body) => worker.fetch(request({ q: 'Customer', domain: 'modelspec', ...body }), env);

test('gateway: a filter for either spelling returns documents of both kinds, each with its stored kind', async () => {
  const { manifest, exports } = mixedCorpus();
  const { documents } = mergeExports(manifest, exports);
  for (const kind of ['model_entity', 'model_record', ['model_record'], ['model_entity', 'model_record']]) {
    const seen = [];
    const response = await search(gatewayOver(documents, seen), { kind });
    assert.equal(response.status, 200);
    assert.match(seen[0], /kind:=\[model_entity,model_record\]/);
    const { hits } = await response.json();
    assert.deepEqual(hits.map(hit => hit.kind).sort(), ['model_entity', 'model_record']);
    for (const hit of hits) {
      assert.equal(hit.field_count, 2, 'field_count is returned for both kinds');
      assert.equal(hit.field_preview.length, 2, 'field_preview is returned for both kinds');
    }
  }
});

test('gateway: other filters are unchanged, and the request still refuses an unknown or repeated kind', async () => {
  const { manifest, exports } = mixedCorpus();
  const { documents } = mergeExports(manifest, exports);
  const seen = [];
  const worker = gatewayOver(documents, seen);
  const fields = await (await search(worker, { kind: 'model_field' })).json();
  assert.equal(fields.hits.length, 4);
  assert.match(seen[0], /kind:=\[model_field\]/);
  const none = await (await search(worker, {})).json();
  assert.equal(none.hits.length, 7);
  assert.doesNotMatch(seen[1], /kind:=/);
  for (const kind of ['model_recordset', 'model_Record', 7, ['model_record', 'model_record'], ['model_entity', 'model_entity'], [...kinds, 'model']]) {
    assert.equal((await search(worker, { kind })).status, 400, JSON.stringify(kind));
  }
  assert.equal((await search(worker, { kind: [...kinds] })).status, 200, 'every accepted kind may be requested at once');
});

test('gateway: the priority comparison treats model_record like model_entity and still refuses a wrong claim', async () => {
  const { manifest, exports, invoice } = mixedCorpus();
  const { documents } = mergeExports(manifest, exports);
  const stored = documents.find(doc => doc.id === invoice.id);
  assert.equal((await search(gatewayOver([stored]), {})).status, 200);
  for (const wrong of [0, 1, 3]) {
    const response = await search(gatewayOver([{ ...stored, kind_priority: wrong }]), {});
    assert.equal(response.status, 503, `kind_priority ${wrong} on model_record`);
    assert.deepEqual(await response.json(), { error: 'unavailable' });
  }
  const unknown = { ...stored, kind: 'model_stray' };
  assert.equal((await search(gatewayOver([unknown]), {})).status, 503);
  const field = documents.find(doc => doc.kind === 'model_field');
  const leaked = await (await search(gatewayOver([{ ...field, field_count: 9, field_preview: ['x'] }]), {})).json();
  assert.equal(leaked.hits[0].field_count, undefined, 'field context is returned only for record-type kinds');
});

test('benchmark corpus mixes both spellings and still merges', () => {
  const { exports } = generate(40);
  const kindsSeen = new Set(exports.flatMap(envelope => envelope.documents.map(doc => doc.kind)));
  assert.ok(kindsSeen.has('model_entity') && kindsSeen.has('model_record'));
  const sources = exports.map(envelope => ({ domain: envelope.domain, repository: Object.keys(envelope.source_revisions)[0], revision: Object.values(envelope.source_revisions)[0], sha256: 'a'.repeat(64), file: `/synthetic/${envelope.domain}` }));
  const merged = mergeExports({ sequence: 1, sources, approved_revisions: {} }, exports);
  assert.equal(merged.documents.filter(doc => ['model_entity', 'model_record'].includes(doc.kind)).every(doc => doc.kind_priority === 2), true);
});
