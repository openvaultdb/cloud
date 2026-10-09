#!/usr/bin/env node
// Synthetic expansion benchmark. This measures exporter shape and JSONL bytes, not Typesense index cost.
import { performance } from 'node:perf_hooks';
import { createHash } from 'node:crypto';
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { stableId } from './schema.mjs';
import { mergeExports } from './merge.mjs';

const plans = [10000, 50000, 100000];
const sha = char => char.repeat(40);
const sources = [
  { domain: 'meaninggraph', repository: 'meaninggraph/registry', revision: sha('1'), sha256: 'a'.repeat(64), file: '/synthetic/meaninggraph' },
  { domain: 'modelspec', repository: 'modelspec-org/registry', revision: sha('2'), sha256: 'b'.repeat(64), file: '/synthetic/modelspec' },
  { domain: 'ovdb', repository: 'openvaultdb/directory', revision: sha('3'), sha256: 'c'.repeat(64), file: '/synthetic/ovdb' }
];
function document(source, kind, id, parent, ordinal, label = id) {
  const native_id = `${source.domain}/${id}`;
  const canonical_url = parent ? `${parent.canonical_url.split('#')[0]}#${kind}-${ordinal}-${encodeURIComponent(label)}` : kind === 'ovdb_server'
    ? `https://directory.openvaultdb.com/servers/${createHash('sha256').update(id).digest('hex')}/`
    : source.domain === 'meaninggraph'
    ? `https://meaninggraph.io/graphs/synthetic/concepts/${encodeURIComponent(id)}/`
    : source.domain === 'modelspec'
      ? `https://modelspec.org/registry/models/synthetic-${ordinal}/#${kind}-${ordinal}`
      : `https://directory.openvaultdb.com/databases/synthetic-${ordinal}/#recordset-${ordinal}`;
  return {
    id: stableId(source.domain, kind, native_id), domain: source.domain, kind, native_id,
    title: label, identifier: label, qualified_name: parent ? `${parent.qualified_name}.${label}` : `${source.domain}.Synthetic.${id}`,
    aliases: [label.replace(/([a-z])([A-Z])/g, '$1 $2'), label.replaceAll('_', ' ')],
    description: `${label} synthetic registry metadata with a long identifier and repeated CustomerId field name across parents. `.repeat(2),
    ...(parent ? { parent_id: parent.id, parent_label: parent.title } : {}),
    canonical_url, visibility: 'public', source_repository: source.repository,
    source_commit: source.revision, source_path: `synthetic/${ordinal}.json`
  };
}
export function generate(count) {
  const exports = sources.map(source => ({ format: 'registry-search-export/v1', domain: source.domain, fixture: true, source_revisions: { [source.repository]: source.revision }, documents: [] }));
  const mix = { meaninggraph: { source_records: 0, parents: 0, leaves: 0, expanded: 0, heavy_100_field_models: 0 }, modelspec: { source_records: 0, parents: 0, leaves: 0, expanded: 0, heavy_100_field_models: 0 }, ovdb: { source_records: 0, parents: 0, leaves: 0, expanded: 0, heavy_100_field_models: 0 } };
  for (let i = 0; i < count; i++) {
    const slot = i % 10 < 4 ? 0 : i % 10 < 7 ? 1 : 2;
    const source = sources[slot];
    const envelope = exports[slot];
    const stats = mix[source.domain];
    const parentKind = slot === 0 ? 'meaning_entity' : slot === 1 ? (i % 2 ? 'model_collection' : 'model') : 'ovdb_database';
    const fieldKind = slot === 0 ? 'meaning_field' : slot === 1 ? 'model_field' : 'ovdb_collection';
    const name = `CustomerOrder_Details_${i}`;
    const parent = document(source, parentKind, name, null, i);
    envelope.documents.push(parent);
    stats.source_records++;
    stats.parents++;
    if (slot === 2 && i % 100 === 7) {
      envelope.documents.push(document(source, 'ovdb_server', `server-${Math.floor(i / 100)}`, null, i));
      stats.parents++;
    }
    let fieldParent = parent;
    if (slot === 1 && parentKind === 'model') {
      // Both spellings of a ModelSpec record type occur, as they do while sites switch over.
      fieldParent = document(source, i % 4 === 0 ? 'model_record' : 'model_entity', `${name}/CustomerEntity`, parent, i, 'CustomerEntity');
      envelope.documents.push(fieldParent);
      stats.parents++;
    }
    const extra = slot === 1 && parentKind === 'model' ? i % 101 === 0 ? 100 : i % 17 === 0 ? 32 : i % 5 === 0 ? 8 : i % 3 === 0 ? 2 : 0
      : slot === 1 ? i % 17 === 0 ? 12 : i % 5 === 0 ? 4 : i % 3 === 0 ? 1 : 0
      : slot === 2 && i % 113 === 0 ? 64
      : i % 31 === 0 ? 16 : i % 4 === 0 ? 3 : i % 3 === 0 ? 1 : 0;
    if (slot === 1 && parentKind === 'model' && extra === 100) stats.heavy_100_field_models++;
    for (let j = 0; j < extra; j++) {
      const field = document(source, fieldKind, `${name}/CustomerId_${j}`, fieldParent, i, `CustomerId_${j}`);
      if (slot === 1 && j % 2 === 0) field.description += ' Effective inherited field declared in a reusable component.';
      envelope.documents.push(field);
      stats.leaves++;
    }
    stats.expanded += 1 + extra + (fieldParent === parent ? 0 : 1) + (slot === 2 && i % 100 === 7 ? 1 : 0);
  }
  return { exports, mix };
}
async function runProfile(sourceRecords, outputDir) {
  const start = performance.now();
  const { exports, mix } = generate(sourceRecords);
  const generation = mergeExports({ sequence: 1, sources, approved_revisions: {} }, exports);
  const jsonlBytes = generation.documents.reduce((bytes, doc) => bytes + Buffer.byteLength(JSON.stringify(doc)) + 1, 0);
  const result = { source_records: sourceRecords, source_record_unit: 'one graph concept, model descriptor/collection, or registered database; child fields/entities/collections and grouped servers are derived documents', expanded_documents: generation.documents.length, mix, jsonl_bytes: jsonlBytes, expansion_ms: Math.round(performance.now() - start), process_rss_bytes: process.memoryUsage().rss, note: 'synthetic export expansion only; no Typesense disk/RAM/query measurement or capacity qualification' };
  if (outputDir) {
    await mkdir(outputDir, { recursive: true });
    const manifest = { sequence: sourceRecords, sources: [], approved_revisions: {}, smoke_queries: [] };
    for (let i = 0; i < exports.length; i++) {
      const envelope = exports[i];
      const file = resolve(outputDir, `${envelope.domain}.json`);
      const raw = JSON.stringify(envelope);
      await writeFile(file, raw);
      manifest.sources.push({ ...sources[i], sha256: createHash('sha256').update(raw).digest('hex'), file });
      manifest.smoke_queries.push({ domain: envelope.domain, q: envelope.documents[0].identifier, id: envelope.documents[0].id });
    }
    await writeFile(resolve(outputDir, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
    result.fixture_dir = outputDir;
  }
  process.stdout.write(JSON.stringify(result) + '\n');
}
if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const args = process.argv.slice(2);
  const profile = args.includes('--profile') ? Number(args[args.indexOf('--profile') + 1]) : null;
  const output = args.includes('--out') ? resolve(args[args.indexOf('--out') + 1]) : null;
  if (profile && !plans.includes(profile) || output && !profile) throw new Error('use --profile 10000|50000|100000 with --out DIR');
  for (const count of profile ? [profile] : plans) await runProfile(count, output);
}
