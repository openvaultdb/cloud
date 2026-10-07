import { createHash } from 'node:crypto';
import { domains, validateDocument } from './schema.mjs';

export const canonicalRoutes = Object.freeze({
  meaninggraph: { hosts: ['meaninggraph.io'], paths: ['/graphs/'] },
  modelspec: { hosts: ['modelspec.org'], paths: ['/registry/models/'] },
  ovdb: { hosts: ['directory.openvaultdb.com'], paths: ['/ovdb/', '/databases/', '/servers/'] }
});
const sourceRepos = { meaninggraph: 'meaninggraph/registry', modelspec: 'modelspec-org/registry', ovdb: 'openvaultdb/directory' };

const sha = /^[a-f0-9]{40}$/;
const own = (value, key) => Object.hasOwn(value, key);
export function validateManifest(manifest, { allowFixtures = false } = {}) {
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

export function mergeExports(manifest, exports) {
  validateManifest(manifest, { allowFixtures: true });
  if (exports.length !== 3) throw new Error('missing export');
  const revisions = new Map(manifest.sources.map(source => [source.repository, source.revision]));
  for (const [repository, revision] of Object.entries(manifest.approved_revisions)) {
    if (revisions.has(repository) && revisions.get(repository) !== revision) throw new Error('conflicting manifest pin');
    revisions.set(repository, revision);
  }
  const byDomain = new Map(manifest.sources.map(source => [source.domain, source]));
  const used = new Set();
  const ids = new Set();
  const docs = [];
  for (const envelope of exports) {
    if (envelope?.format !== 'registry-search-export/v1' || !byDomain.has(envelope.domain) || !Array.isArray(envelope.documents) || typeof envelope.fixture !== 'boolean' || !envelope.source_revisions || typeof envelope.source_revisions !== 'object') throw new Error('invalid export envelope');
    const source = byDomain.get(envelope.domain);
    if (used.has(source.domain)) throw new Error('repeated export');
    used.add(source.domain);
    if (envelope.fixture && !source.file) throw new Error('fixture in remote source');
    if (envelope.source_revisions[source.repository] !== source.revision) throw new Error('source revision mismatch');
    for (const [repo, revision] of Object.entries(envelope.source_revisions)) {
      if (!sha.test(revision)) throw new Error('invalid referenced revision');
      if (!revisions.has(repo)) throw new Error('unapproved source repository');
      if (revisions.get(repo) !== revision) throw new Error('conflicting shared pin');
    }
    for (const doc of envelope.documents) {
      validateDocument(doc, source, canonicalRoutes, revisions);
      if (envelope.source_revisions[doc.source_repository] !== doc.source_commit) throw new Error('document missing source pin');
      if (ids.has(doc.id)) throw new Error('duplicate document id');
      ids.add(doc.id);
      docs.push(doc);
    }
  }
  if (manifest.sources.some(source => !used.has(source.domain))) throw new Error('missing domain export');
  if (!docs.length && !manifest.allow_empty) throw new Error('empty corpus requires explicit review');
  docs.sort((a, b) => a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  const sortedDocs = docs.map(doc => Object.fromEntries(Object.entries(doc).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0)));
  const canonical = sortedDocs.map(doc => JSON.stringify(doc)).join('\n') + (docs.length ? '\n' : '');
  const hash = createHash('sha256').update(canonical).digest('hex');
  const generation = hash.slice(0, 24);
  return { documents: sortedDocs.map(doc => ({ ...doc, generation_id: generation })), hash, generation, source_revisions: Object.fromEntries([...revisions].sort()) };
}
