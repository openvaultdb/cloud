import { createHash } from 'node:crypto';
import { domains, validateDocument } from './schema.mjs';
import { combinedManifestFormat, combinedExportFormat, combinedRepository, combinedURL, directoryURL, exactDomains, isCombined } from './profiles.mjs';
import { corePriority, fieldParentKinds, kindPriority, repositoryTerms } from './provenance.mjs';

export const canonicalRoutes = Object.freeze({
  meaninggraph: { hosts: ['meaninggraph.io'], paths: ['/graphs/'] },
  modelspec: { hosts: ['modelspec.org'], paths: ['/registry/models/'] },
  ovdb: { hosts: ['directory.openvaultdb.com'], paths: ['/ovdb/', '/databases/', '/servers/'] }
});
const sourceRepos = { meaninggraph: 'meaninggraph/registry', modelspec: 'modelspec-org/registry', ovdb: 'openvaultdb/directory' };

const sha = /^[a-f0-9]{40}$/;
const own = (value, key) => Object.hasOwn(value, key);
export function validateManifest(manifest, { allowFixtures = false } = {}) {
  if (manifest?.format !== undefined && manifest.format !== combinedManifestFormat) throw new Error('unknown manifest format');
  if (isCombined(manifest)) return validateCombinedManifest(manifest, { allowFixtures });
  if (!manifest || typeof manifest !== 'object' || Array.isArray(manifest) || !Array.isArray(manifest.sources) || manifest.sources.length !== 3) throw new Error('expected three sources');
  if (manifest.routes && JSON.stringify(manifest.routes) !== JSON.stringify(canonicalRoutes)) throw new Error('route allowlist cannot be overridden');
  if (!Number.isSafeInteger(manifest.sequence) || manifest.sequence < 1) throw new Error('invalid publication sequence');
  if (!manifest.approved_revisions || typeof manifest.approved_revisions !== 'object' || Array.isArray(manifest.approved_revisions)) throw new Error('missing approved revisions');
  for (const [repository, revision] of Object.entries(manifest.approved_revisions)) if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repository) || !sha.test(revision)) throw new Error('invalid approved revision');
  const seen = new Set();
  for (const source of manifest.sources) {
    if (!domains.includes(source.domain) || seen.has(source.domain)) throw new Error('missing or repeated domain');
    seen.add(source.domain);
    if (source.repository !== sourceRepos[source.domain] || !sha.test(source.revision) || !/^[a-f0-9]{64}$/.test(source.sha256)) throw new Error('invalid pinned source');
    if (source.file) { if (!allowFixtures || source.url) throw new Error('local fixture forbidden'); }
    else {
      const url = new URL(source.url);
      if (url.protocol !== 'https:' || url.username || url.password || url.port || url.search || url.hash || url.pathname !== '/registry-search.json' || !canonicalRoutes[source.domain]?.hosts?.includes(url.hostname)) throw new Error('unsafe source URL');
    }
  }
  return manifest;
}

function validateCombinedManifest(manifest, { allowFixtures }) {
  if (!Array.isArray(manifest.sources) || manifest.sources.length !== 2) throw new Error('expected two sources');
  if (manifest.routes && JSON.stringify(manifest.routes) !== JSON.stringify(canonicalRoutes)) throw new Error('route allowlist cannot be overridden');
  if (!Number.isSafeInteger(manifest.sequence) || manifest.sequence < 1) throw new Error('invalid publication sequence');
  if (!manifest.approved_revisions || typeof manifest.approved_revisions !== 'object' || Array.isArray(manifest.approved_revisions)) throw new Error('missing approved revisions');
  for (const [repository, revision] of Object.entries(manifest.approved_revisions)) if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repository) || typeof revision !== 'string' || !sha.test(revision)) throw new Error('invalid approved revision');
  if (!Array.isArray(manifest.empty_domains) || new Set(manifest.empty_domains).size !== manifest.empty_domains.length || manifest.empty_domains.some(domain => !domains.includes(domain))) throw new Error('invalid empty domains');
  const seen = new Set();
  for (const source of manifest.sources) {
    if (!source || typeof source !== 'object' || Array.isArray(source)) throw new Error('invalid pinned source');
    const combined = exactDomains(source.domains) && !own(source, 'domain');
    const directory = source.domain === 'ovdb' && !own(source, 'domains') && !own(source, 'catalog_sha256');
    const key = combined ? 'combined' : directory ? 'ovdb' : null;
    if (!key || seen.has(key)) throw new Error('missing or repeated domain');
    seen.add(key);
    if (source.repository !== (combined ? combinedRepository : sourceRepos.ovdb) || typeof source.revision !== 'string' || !sha.test(source.revision) || typeof source.sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(source.sha256) || combined && (typeof source.catalog_sha256 !== 'string' || !/^[a-f0-9]{64}$/.test(source.catalog_sha256))) throw new Error('invalid pinned source');
    // Exact raw URL equality rejects normalization, traversal and host/path swaps.
    if (own(source, 'file')) {
      if (!allowFixtures || typeof source.file !== 'string' || !source.file || own(source, 'url')) throw new Error('local fixture forbidden');
    } else if (source.url !== (combined ? combinedURL : directoryURL)) throw new Error('unsafe source URL');
  }
  return manifest;
}

export function mergeExports(manifest, exports) {
  validateManifest(manifest, { allowFixtures: true });
  const combined = isCombined(manifest);
  if (!Array.isArray(exports) || exports.length !== manifest.sources.length) throw new Error('missing export');
  const revisions = new Map(manifest.sources.map(source => [source.repository, source.revision]));
  for (const [repository, revision] of Object.entries(manifest.approved_revisions)) {
    if (revisions.has(repository) && revisions.get(repository) !== revision) throw new Error('conflicting manifest pin');
    revisions.set(repository, revision);
  }
  const byDomain = new Map(manifest.sources.map(source => [combined && source.domains ? 'combined' : source.domain, source]));
  const used = new Set();
  const ids = new Set();
  const docs = [];
  for (const envelope of exports) {
    const native = combined && envelope?.format === combinedExportFormat;
    const key = native ? 'combined' : envelope?.domain;
    if ((!native && envelope?.format !== 'registry-search-export/v1') || !byDomain.has(key) || !Array.isArray(envelope.documents) || typeof envelope.fixture !== 'boolean' || !envelope.source_revisions || typeof envelope.source_revisions !== 'object' || combined && Array.isArray(envelope.source_revisions)) throw new Error('invalid export envelope');
    const source = byDomain.get(key);
    if (combined && (!!source.domains !== native || !native && own(envelope, 'domains'))) throw new Error('invalid export envelope');
    if (native && (!exactDomains(envelope.domains) || own(envelope, 'domain') || !envelope.producer || Object.keys(envelope.producer).sort().join(',') !== 'catalog_sha256,repository,revision' || envelope.producer.repository !== source.repository || envelope.producer.revision !== source.revision || envelope.producer.catalog_sha256 !== source.catalog_sha256)) throw new Error('producer provenance mismatch');
    if (used.has(key)) throw new Error('repeated export');
    used.add(key);
    if (envelope.fixture && !source.file) throw new Error('fixture in remote source');
    if (envelope.source_revisions[source.repository] !== source.revision) throw new Error('source revision mismatch');
    for (const [repo, revision] of Object.entries(envelope.source_revisions)) {
      if (!sha.test(revision) || combined && (typeof revision !== 'string' || !/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo))) throw new Error('invalid referenced revision');
      if (!revisions.has(repo)) throw new Error('unapproved source repository');
      if (revisions.get(repo) !== revision) throw new Error('conflicting shared pin');
    }
    for (const doc of envelope.documents) {
      if (native && !source.domains.includes(doc?.domain)) throw new Error('source provenance mismatch');
      validateDocument(doc, native ? { ...source, domain: doc.domain } : source, canonicalRoutes, revisions, { combined });
      if (envelope.source_revisions[doc.source_repository] !== doc.source_commit) throw new Error('document missing source pin');
      if (ids.has(doc.id)) throw new Error('duplicate document id');
      ids.add(doc.id);
      docs.push(doc);
    }
  }
  if (manifest.sources.some(source => !used.has(combined && source.domains ? 'combined' : source.domain))) throw new Error('missing domain export');
  if (combined) {
    const empty = domains.filter(domain => !docs.some(doc => doc.domain === domain));
    if (JSON.stringify([...manifest.empty_domains].sort()) !== JSON.stringify([...empty].sort())) throw new Error('empty domains mismatch');
    const byId = new Map(docs.map(doc => [doc.id, doc]));
    for (const doc of docs) {
      if (!['meaning_field', 'model_field'].includes(doc.kind) || !own(doc, 'parent_id')) continue;
      const parent = byId.get(doc.parent_id);
      const allowedParents = doc.kind === 'meaning_field' ? ['meaning_entity'] : doc.kind === 'model_field' ? ['model_entity', 'model_record', 'model_collection'] : [];
      if (!parent || !allowedParents.includes(parent.kind) || parent.domain !== doc.domain || doc.kind === 'model_field' && (parent.source_repository !== doc.source_repository || parent.source_commit !== doc.source_commit)) throw new Error('invalid parent provenance');
    }
  }
  if (!docs.length && (combined ? manifest.allow_empty !== true : !manifest.allow_empty)) throw new Error('empty corpus requires explicit review');
  const fieldKinds = new Set(['meaning_field', 'model_field']);
  const entityKinds = new Set(fieldParentKinds);
  const fieldsByParent = new Map();
  for (const doc of docs) {
    if (!fieldKinds.has(doc.kind) || !doc.parent_id) continue;
    const fields = fieldsByParent.get(doc.parent_id) || [];
    fields.push(doc);
    fieldsByParent.set(doc.parent_id, fields);
  }
  const enriched = docs.map(doc => {
    const result = { ...doc, ...repositoryTerms(doc), core_priority: corePriority(doc), kind_priority: kindPriority(doc) };
    if (entityKinds.has(doc.kind)) {
      const fields = (fieldsByParent.get(doc.id) || [])
        .filter(field => field.domain === doc.domain && field.source_repository === doc.source_repository)
        .sort((a, b) => a.qualified_name < b.qualified_name ? -1 : a.qualified_name > b.qualified_name ? 1 : a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
      if (fields.length) {
        result.field_count = fields.length;
        result.field_preview = [...new Set(fields.map(field => field.title.trim()).filter(Boolean))].slice(0, 4).map(title => [...title].slice(0, 64).join(''));
      }
    }
    return result;
  });
  enriched.sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  const sortedDocs = enriched.map(doc => Object.fromEntries(Object.entries(doc).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0)));
  const canonical = sortedDocs.map(doc => JSON.stringify(doc)).join('\n') + (enriched.length ? '\n' : '');
  const hash = createHash('sha256').update(canonical).digest('hex');
  const generation = hash.slice(0, 24);
  return { documents: sortedDocs.map(doc => ({ ...doc, generation_id: generation })), hash, generation, source_revisions: Object.fromEntries([...revisions].sort()) };
}
